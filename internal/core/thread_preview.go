package core

import (
	"context"
	"encoding/json"
	"sync"
	"time"
)

const (
	threadPreviewMaxRuns   = 256
	threadPreviewMaxBlocks = 32
	threadPreviewMaxBytes  = 256 << 10
	threadPreviewTTL       = 2 * time.Minute
)

// ThreadPreviewBlock replaces one still-open history position; it is never a durable Thread row.
type ThreadPreviewBlock struct {
	HistorySequence int64           `json:"historySequence"`
	TurnID          string          `json:"turnId"`
	Record          json.RawMessage `json:"record"`
}

// ThreadPreviewSnapshot is an execution-scoped replacement, so missed frames never lose a delta.
type ThreadPreviewSnapshot struct {
	ExecutionID string               `json:"executionId"`
	Version     int64                `json:"version"`
	Blocks      []ThreadPreviewBlock `json:"blocks"`
	Incomplete  bool                 `json:"incomplete"`
	Available   bool                 `json:"available"`
}

type cachedThreadPreview struct {
	snapshot ThreadPreviewSnapshot
	observed time.Time
}

// ThreadPreviewHub holds bounded, ephemeral snapshots. Subscribers receive coalesced wakeups and
// re-read the latest authorized snapshot; neither a slow subscriber nor a missing preview is an Ack.
type ThreadPreviewHub struct {
	mu        sync.Mutex
	runs      map[string]cachedThreadPreview
	subs      map[string]map[chan struct{}]struct{}
	now       func() time.Time
	listeners int
}

// NewThreadPreviewHub builds a process-local optional preview cache. Persisted history works when
// a replica lacks the producer's cache, and the browser explicitly reports preview availability.
func NewThreadPreviewHub() *ThreadPreviewHub {
	return &ThreadPreviewHub{runs: map[string]cachedThreadPreview{}, subs: map[string]map[chan struct{}]struct{}{}, now: time.Now}
}

func cloneThreadPreview(snapshot ThreadPreviewSnapshot) ThreadPreviewSnapshot {
	cloned := snapshot
	cloned.Blocks = make([]ThreadPreviewBlock, len(snapshot.Blocks))
	for index, block := range snapshot.Blocks {
		cloned.Blocks[index] = block
		cloned.Blocks[index].Record = append(json.RawMessage(nil), block.Record...)
	}
	return cloned
}

// Publish replaces only newer snapshots. Equal versions cannot alter already observed content.
func (h *ThreadPreviewHub) Publish(runID string, snapshot ThreadPreviewSnapshot) {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	for id, cached := range h.runs {
		if now.Sub(cached.observed) > threadPreviewTTL {
			delete(h.runs, id)
		}
	}
	prior, exists := h.runs[runID]
	if exists && prior.snapshot.ExecutionID == snapshot.ExecutionID && prior.snapshot.Version >= snapshot.Version {
		if prior.snapshot.Version == snapshot.Version {
			prior.observed = now
			h.runs[runID] = prior
		}
		return
	}
	if !exists && len(h.runs) >= threadPreviewMaxRuns {
		var oldest string
		var at time.Time
		for id, cached := range h.runs {
			if oldest == "" || cached.observed.Before(at) {
				oldest, at = id, cached.observed
			}
		}
		delete(h.runs, oldest)
	}
	h.runs[runID] = cachedThreadPreview{snapshot: cloneThreadPreview(snapshot), observed: now}
	for subscriber := range h.subs[runID] {
		select {
		case subscriber <- struct{}{}:
		default:
		}
	}
}

// Snapshot returns a copy; expired caches cannot pretend to be a currently connected producer.
func (h *ThreadPreviewHub) Snapshot(runID string) ThreadPreviewSnapshot {
	h.mu.Lock()
	defer h.mu.Unlock()
	if cached, ok := h.runs[runID]; ok && h.now().Sub(cached.observed) <= threadPreviewTTL {
		return cloneThreadPreview(cached.snapshot)
	}
	delete(h.runs, runID)
	return ThreadPreviewSnapshot{Blocks: []ThreadPreviewBlock{}}
}

// Subscribe owns a bounded wake channel until cancel. Its initial wake closes the subscribe/read race.
func (h *ThreadPreviewHub) Subscribe(runID string) (events <-chan struct{}, cancel func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.listeners >= 1024 || len(h.subs[runID]) >= 64 {
		return nil, func() {}
	}
	channel := make(chan struct{}, 1)
	if h.subs[runID] == nil {
		h.subs[runID] = map[chan struct{}]struct{}{}
	}
	h.subs[runID][channel] = struct{}{}
	h.listeners++
	channel <- struct{}{}
	var once sync.Once
	return channel, func() {
		once.Do(func() {
			h.mu.Lock()
			defer h.mu.Unlock()
			delete(h.subs[runID], channel)
			h.listeners--
			if len(h.subs[runID]) == 0 {
				delete(h.subs, runID)
			}
		})
	}
}

// PublishThreadPreview verifies the lease and live execution before accepting ephemeral content.
// It writes no receipt, Thread entry or execution fact; Controller must never Ack it to Node.
func (s *Store) PublishThreadPreview(ctx context.Context, service *Claims, epoch int64, runID string, snapshot ThreadPreviewSnapshot) error {
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		return &Fault{Status: 400, Code: "invalid_preview", Params: Object{}}
	}
	_, err = s.transact(ctx, func(t *transaction) Object {
		require(service != nil && service.Role == "controller", 403, "service_forbidden")
		leaseValid(t, &ControlRequest{Service: service, Body: Object{"epoch": epoch}})
		require(validID(runID) && snapshot.ExecutionID != "" && snapshot.Version > 0 && len(snapshot.Blocks) <= threadPreviewMaxBlocks && len(encoded) <= threadPreviewMaxBytes, 400, "invalid_preview")
		execution := t.one(`SELECT e.execution_id FROM node_executions e JOIN issue_runs r ON r.id=e.operation_id
			WHERE e.execution_id=$1 AND e.operation_id=$2 AND e.kind='agent_session' AND e.result IS NULL
			AND e.terminated_by_force_stop_id IS NULL AND r.deleted_at IS NULL AND r.thread_state IN ('pending','active','idle','ending')`, snapshot.ExecutionID, runID)
		require(execution != nil, 409, "dispatch_conflict")
		seen := map[int64]bool{}
		for _, block := range snapshot.Blocks {
			var record map[string]json.RawMessage
			require(block.HistorySequence >= 0 && block.HistorySequence <= 4294967295 && !seen[block.HistorySequence] && (block.TurnID == "" || validID(block.TurnID)) && len(block.Record) <= 65536 && json.Unmarshal(block.Record, &record) == nil && record != nil, 400, "invalid_preview")
			seen[block.HistorySequence] = true
		}
		return Object{}
	})
	if err == nil && s.ThreadPreviews != nil {
		snapshot.Available = true
		s.ThreadPreviews.Publish(runID, snapshot)
	}
	return err
}

func threadPreviewRead(t *transaction, store *Store, request *PublicRequest) Object {
	issue(t, request.TenantID, request.IssueID)
	run := run(t, request.TenantID, request.IssueID, request.RunID)
	require(run.S("executorType") == "agent", 404, "not_found")
	snapshot := ThreadPreviewSnapshot{Blocks: []ThreadPreviewBlock{}}
	if store.ThreadPreviews != nil {
		snapshot = store.ThreadPreviews.Snapshot(request.RunID)
	}
	if snapshot.ExecutionID != "" {
		live := t.one("SELECT execution_id FROM node_executions WHERE execution_id=$1 AND operation_id=$2 AND kind='agent_session' AND result IS NULL AND terminated_by_force_stop_id IS NULL", snapshot.ExecutionID, request.RunID)
		if live == nil {
			snapshot = ThreadPreviewSnapshot{Blocks: []ThreadPreviewBlock{}}
		}
	}
	finals := t.list("SELECT DISTINCT history_sequence FROM thread_entries WHERE run_id=$1 AND history_sequence IS NOT NULL", request.RunID)
	settled := map[int64]bool{}
	for _, final := range finals {
		settled[final.N("historySequence")] = true
	}
	blocks := make([]ThreadPreviewBlock, 0, len(snapshot.Blocks))
	for _, block := range snapshot.Blocks {
		if !settled[block.HistorySequence] {
			blocks = append(blocks, block)
		}
	}
	snapshot.Blocks = blocks
	return Object{"available": snapshot.Available, "executionId": snapshot.ExecutionID, "version": snapshot.Version, "blocks": blocks, "incomplete": snapshot.Incomplete}
}
