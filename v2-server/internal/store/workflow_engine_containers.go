package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// WorkflowEngineLoopRounds reads every durable Loop scope for an admitted run.
func (s *Store) WorkflowEngineLoopRounds(ctx context.Context, runID string) ([]WorkflowScope, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+workflowScopeColumns+` FROM v2_workflow_scopes WHERE run_id=$1 AND parent_loop_node_run_id IS NOT NULL ORDER BY created_at,id`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []WorkflowScope{}
	for rows.Next() {
		scope, err := scanWorkflowScope(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, scope)
	}
	return result, rows.Err()
}

// WorkflowEngineScopeNodes lists live attempts in precisely one execution scope.
func (s *Store) WorkflowEngineScopeNodes(ctx context.Context, runID, scopeID string) ([]WorkflowNodeRun, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+workflowNodeRunColumns+` FROM v2_workflow_node_runs WHERE run_id=$1 AND scope_id=$2 AND deleted_at IS NULL ORDER BY created_at,id`, runID, scopeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []WorkflowNodeRun{}
	for rows.Next() {
		node, err := scanNodeRun(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, node)
	}
	return result, rows.Err()
}

// WorkflowEngineRoundStart inserts a private round and its entry together.
type WorkflowEngineRoundStart struct {
	Index int
	State []byte
	Entry WorkflowEngineStart
}

// WorkflowEngineContainerResult settles a parent without interpreting its domain payload.
type WorkflowEngineContainerResult struct {
	Status        string
	Output, Error *string
	Merge         func([]byte) ([]byte, error)
}

// WorkflowEngineContainerAdvance commits a scope/ledger decision with its continuation.
// All folds are pure, so no external work happens while the run is locked.
type WorkflowEngineContainerAdvance struct {
	ScopeID, ScopeStatus string
	ScopeState           []byte
	CancelScope          bool
	ExitNodeID           string
	Next                 *WorkflowEngineRoundStart
	Starts               []WorkflowEngineStart
	Fold                 func([]byte) ([]byte, error)
	Result               *WorkflowEngineContainerResult
}

// AdvanceWorkflowEngineContainer holds the same run lock as callbacks and cancellation. An
// inactive parent/round is a no-op, so delayed cleanup cannot overwrite a terminal outcome.
// Its transaction includes the ledger, exports, old round closure, and new round entry.
func (s *Store) AdvanceWorkflowEngineContainer(ctx context.Context, runID, parentID string, in WorkflowEngineContainerAdvance) ([]WorkflowNodeRun, bool, error) {
	created := []WorkflowNodeRun{}
	changed := false
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		r, err := lockEngineRun(ctx, tx, runID)
		if err != nil {
			return err
		}
		if r.status != WorkflowStatusRunning {
			return nil
		}
		parent, err := lockWorkflowNodeRun(ctx, tx, r.tenant, runID, parentID)
		if errors.Is(err, ErrWorkflowNodeRunNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if parent.Status != WorkflowStatusRunning {
			return nil
		}
		if parent.ScopeID != r.root || parent.Iteration != nil || (parent.NodeType != "loop" && parent.NodeType != "iteration") {
			return ErrWorkflowNodeRunState
		}
		var current WorkflowScope
		if in.ScopeID != "" {
			current, err = scanWorkflowScope(tx.QueryRow(ctx, `SELECT `+workflowScopeColumns+` FROM v2_workflow_scopes WHERE id=$1 AND run_id=$2 FOR UPDATE`, in.ScopeID, runID))
			if err != nil {
				return err
			}
			if current.ParentLoopNodeRunID == nil || *current.ParentLoopNodeRunID != parentID {
				return ErrWorkflowScopeState
			}
			if current.Status == nil || *current.Status != WorkflowStatusRunning {
				return nil
			}
			if in.CancelScope {
				// The requested exit must itself be a live control in this exact round.
				exit, err := lockWorkflowNodeRun(ctx, tx, r.tenant, runID, in.ExitNodeID)
				if err != nil {
					return err
				}
				if exit.ScopeID != in.ScopeID || exit.NodeType != "loopExit" {
					return ErrWorkflowNodeRunState
				}
				if exit.Status != WorkflowStatusRunning {
					return nil
				}
				if _, err := tx.Exec(ctx, `UPDATE v2_workflow_node_runs SET status='succeeded', payload=COALESCE(payload,'{}'::jsonb)||'{"stop_reason":"loop_exit"}'::jsonb, finished_at=now(), updated_at=now() WHERE id=$1`, exit.ID); err != nil {
					return err
				}
				if _, err := tx.Exec(ctx, `UPDATE v2_workflow_node_runs SET status='cancelled', finished_at=now(), updated_at=now() WHERE scope_id=$1 AND deleted_at IS NULL AND status IN ('pending','running')`, in.ScopeID); err != nil {
					return err
				}
			}
			status := *current.Status
			if in.ScopeStatus != "" {
				status = in.ScopeStatus
			}
			state := current.State
			if in.ScopeState != nil {
				state = in.ScopeState
			}
			if _, err := tx.Exec(ctx, `UPDATE v2_workflow_scopes SET status=$2, state=$3, updated_at=now() WHERE id=$1`, in.ScopeID, status, state); err != nil {
				return err
			}
		}
		if in.Fold != nil {
			payload, err := in.Fold(r.payload)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE v2_workflow_runs SET payload=$2,updated_at=now() WHERE id=$1`, runID, payload); err != nil {
				return err
			}
		}
		starts := append([]WorkflowEngineStart{}, in.Starts...)
		if in.Next != nil {
			if parent.NodeType != "loop" {
				return ErrWorkflowNodeRunState
			}
			if in.ScopeID == "" {
				var exists bool
				if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM v2_workflow_scopes WHERE parent_loop_node_run_id=$1)`, parentID).Scan(&exists); err != nil {
					return err
				}
				if exists {
					return ErrWorkflowScopeExists
				}
				if in.Next.Index != 1 {
					return ErrWorkflowScopeState
				}
			} else if current.RoundIndex == nil || in.Next.Index != *current.RoundIndex+1 || in.ScopeStatus != WorkflowStatusSucceeded {
				return ErrWorkflowScopeState
			}
			scopeID := fmt.Sprintf("scope:%s:%d", parentID, in.Next.Index)
			if _, err := tx.Exec(ctx, `INSERT INTO v2_workflow_scopes (id,tenant_id,run_id,parent_loop_node_run_id,round_index,status,state) VALUES($1,$2,$3,$4,$5,'running',$6)`, scopeID, r.tenant, runID, parentID, in.Next.Index, in.Next.State); err != nil {
				return err
			}
			entry := in.Next.Entry
			entry.ScopeID = scopeID
			starts = append(starts, entry)
		}
		for _, start := range starts {
			node, err := insertEngineNode(ctx, tx, r, runID, start)
			if err != nil {
				return err
			}
			created = append(created, node)
		}
		if result := in.Result; result != nil {
			payload := parent.Payload
			if result.Merge != nil {
				payload, err = result.Merge(payload)
				if err != nil {
					return err
				}
			}
			if _, err := tx.Exec(ctx, `UPDATE v2_workflow_node_runs SET status=$2,output=$3,error=$4,payload=$5,finished_at=now(),updated_at=now() WHERE id=$1`, parentID, result.Status, result.Output, result.Error, payload); err != nil {
				return err
			}
			currentNodes := without(r.current, parent.NodeID)
			if result.Status == WorkflowStatusFailed {
				if _, err := tx.Exec(ctx, `UPDATE v2_workflow_runs SET status='failed',error=$2,finished_at=now(),updated_at=now() WHERE id=$1`, runID, result.Error); err != nil {
					return err
				}
				currentNodes = []string{parent.NodeID}
			}
			if err := writeCurrentNodes(ctx, tx, runID, currentNodes); err != nil {
				return err
			}
		}
		changed = true
		return nil
	})
	if err != nil {
		return nil, false, fmt.Errorf("advance workflow container: %w", err)
	}
	return created, changed, nil
}
