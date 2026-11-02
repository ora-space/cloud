package server

import (
	"encoding/json"
	"sort"
	"strings"

	"github.com/wanglongan587/cloud/v2-server/internal/store"
)

// workflowRunVariableView is one declared variable of the run's root pool, desktop's
// WorkflowRunVariable. Value is absent while the variable is unassigned, so a null value stays
// distinguishable from none.
type workflowRunVariableView struct {
	Selector     []string        `json:"selector"`
	ValueType    string          `json:"valueType"`
	SourceNodeID string          `json:"sourceNodeId"`
	Value        json.RawMessage `json:"value,omitempty"`
}

// workflowRunVariables projects the run payload's variable pool as desktop's project_variable_pool
// does: one entry per catalog key, in key order, without the legacy selected_branch_id routing
// entries. A run that has not started has no pool and reads as none; a key that is not
// "<node>.<name>" is left out rather than failing the whole run detail.
func workflowRunVariables(payload []byte) []workflowRunVariableView {
	var p struct {
		VariablePool struct {
			Catalog map[string]struct {
				ValueType string `json:"valueType"`
				Writer    string `json:"writer"`
			} `json:"catalog"`
			Values map[string]json.RawMessage `json:"values"`
		} `json:"variablePool"`
	}
	views := []workflowRunVariableView{}
	if json.Unmarshal(payload, &p) != nil {
		return views
	}
	keys := make([]string, 0, len(p.VariablePool.Catalog))
	for key := range p.VariablePool.Catalog {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		node, name, ok := strings.Cut(key, ".")
		if !ok || name == "selected_branch_id" {
			continue
		}
		entry := p.VariablePool.Catalog[key]
		views = append(views, workflowRunVariableView{
			Selector: []string{node, name}, ValueType: entry.ValueType, SourceNodeID: entry.Writer,
			Value: p.VariablePool.Values[key],
		})
	}
	return views
}

// workflowFailedAttemptView is one earlier failed attempt of a node, desktop's
// WorkflowNodeFailedAttempt, read from the attempt's error_detail.
type workflowFailedAttemptView struct {
	NodeRunID   string   `json:"nodeRunId"`
	NodeID      string   `json:"nodeId"`
	ScopeID     string   `json:"scopeId"`
	Iteration   *int     `json:"iteration"`
	SessionID   *string  `json:"sessionId"`
	Attempt     int      `json:"attempt"`
	Kind        string   `json:"kind"`
	Message     string   `json:"message"`
	SourceChain []string `json:"sourceChain"`
	RecordedAt  int64    `json:"recordedAt"`
	StartedAt   *int64   `json:"startedAt"`
	FinishedAt  *int64   `json:"finishedAt"`
}

// workflowFailedAttempts maps the replaced failed rows of a run, skipping one without an
// error_detail as desktop's map_failed_attempt does. v2 neither retries nor resumes, so nothing
// replaces a failed attempt yet and the list stays empty; the current failure is the node's own row.
func workflowFailedAttempts(rows []store.WorkflowNodeRun) []workflowFailedAttemptView {
	views := []workflowFailedAttemptView{}
	for _, row := range rows {
		var p struct {
			ErrorDetail *struct {
				Kind        string   `json:"kind"`
				Message     string   `json:"message"`
				SourceChain []string `json:"source_chain"`
				Attempt     int      `json:"attempt"`
				RecordedAt  int64    `json:"recorded_at"`
			} `json:"error_detail"`
		}
		if json.Unmarshal(row.Payload, &p) != nil || p.ErrorDetail == nil {
			continue
		}
		d := p.ErrorDetail
		chain := d.SourceChain
		if chain == nil {
			chain = []string{}
		}
		views = append(views, workflowFailedAttemptView{
			NodeRunID: row.ID, NodeID: row.NodeID, ScopeID: row.ScopeID, Iteration: row.Iteration, SessionID: row.SessionID,
			Attempt: d.Attempt, Kind: d.Kind, Message: d.Message, SourceChain: chain, RecordedAt: d.RecordedAt,
			StartedAt: millisPtr(row.StartedAt), FinishedAt: millisPtr(row.FinishedAt),
		})
	}
	return views
}
