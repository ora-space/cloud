package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/taskdecomp"
)

// decompositionCatalogStore is what the decomposition picker reads: lightweight workflow refs, and
// a published snapshot's document only when its verdict is not cached yet.
type decompositionCatalogStore interface {
	WorkflowRefs(context.Context, string) ([]store.WorkflowRef, error)
	WorkflowSnapshotGraph(ctx context.Context, tenant, workflowID, snapshotID string) (string, error)
}

const (
	// maxDecompositionVerdicts bounds the verdict cache. An entry is a 36-byte uuid string kept
	// twice (map key and order slice, sharing one backing array) plus a bool and map overhead,
	// roughly 100 bytes, so a full cache is about 6.5 MiB. Every request walks a tenant's refs in a
	// fixed order, which defeats FIFO (and LRU) eviction once the walked set exceeds the bound: the
	// hit rate then drops to zero, so the bound is set far above any demo deployment's published
	// snapshot count rather than near it.
	maxDecompositionVerdicts = 65536
	// maxConcurrentVerdictParses is how many published graphs the process reads and parses at
	// once. Judging a graph allocates several times its size (up to 6 MiB each), so after a restart
	// a burst of pickers over large workflows queues here instead of multiplying the live heap.
	maxConcurrentVerdictParses = 2
	// verdictTimeout bounds one shared judgement. It runs detached from the request that started it,
	// because other requests may be waiting on the same snapshot, but never past the server's
	// lifetime.
	verdictTimeout = 30 * time.Second
)

// decompositionVerdicts answers taskdecomp.CanDecompose per published snapshot id for the whole
// process: answers are cached, concurrent misses for one snapshot share a single read and parse, and
// at most maxConcurrentVerdictParses snapshots are read and parsed at the same time.
type decompositionVerdicts struct {
	// lifetime ends every detached judgement when the server shuts down, so a stalled read cannot
	// hold the database pool open past shutdown.
	lifetime context.Context
	cache    *verdictCache
	flight   singleflight.Group
	slots    chan struct{} // one token per read-and-parse in progress
	// decide is the rule; tests substitute it to observe or break the parse.
	decide func(graph string) bool
}

// errVerdictPanicked marks a judgement that panicked. The panic is logged with its stack where it
// is recovered, so the handler answers 500 without logging it again for every waiting request.
var errVerdictPanicked = errors.New("decomposition verdict panicked")

func newDecompositionVerdicts(lifetime context.Context) *decompositionVerdicts {
	return &decompositionVerdicts{
		lifetime: lifetime,
		cache:    newVerdictCache(maxDecompositionVerdicts),
		slots:    make(chan struct{}, maxConcurrentVerdictParses),
		decide:   taskdecomp.CanDecompose,
	}
}

// verdict answers from the cache or joins the one judgement of this snapshot in progress, starting
// it if there is none. A caller whose request ends stops waiting; the judgement itself runs to
// completion (bounded by verdictTimeout and the server lifetime) and caches its answer for the
// others. A panic in the judgement becomes an uncached errVerdictPanicked: singleflight re-raises a
// DoChan panic on a goroutine nobody can recover, which would end the whole process. The flight is keyed
// by the global snapshot id alone: refs are tenant-scoped, so two callers that share an id share a
// tenant.
func (v *decompositionVerdicts) verdict(ctx context.Context, s decompositionCatalogStore, tenant string, ref store.WorkflowRef) (bool, error) {
	id := *ref.PublishedSnapshotID
	if verdict, ok := v.cache.get(id); ok {
		return verdict, nil
	}
	result := v.flight.DoChan(id, func() (verdict any, err error) {
		defer func() {
			if p := recover(); p != nil {
				slog.Error("judge decomposition verdict", "snapshot", id, "panic", p, "stack", string(debug.Stack()))
				verdict, err = false, fmt.Errorf("%w: snapshot %s", errVerdictPanicked, id)
			}
		}()
		work, cancel := context.WithTimeout(v.lifetime, verdictTimeout)
		defer cancel()
		return v.judge(work, s, tenant, ref.ID, id)
	})
	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case r := <-result:
		if r.Err != nil {
			return false, r.Err
		}
		return r.Val.(bool), nil
	}
}

// judge reads and parses one published snapshot while holding a parse slot. The cache is checked
// again under the slot because a judgement that finished between the caller's miss and its joining
// the flight has already been forgotten by the flight. A snapshot deleted after the ref query (its
// workflow was re-activated meanwhile) is reported as not decomposable and left uncached; Start
// re-checks the live version anyway.
func (v *decompositionVerdicts) judge(ctx context.Context, s decompositionCatalogStore, tenant, workflowID, snapshotID string) (bool, error) {
	select {
	case v.slots <- struct{}{}:
		defer func() { <-v.slots }()
	case <-ctx.Done():
		return false, ctx.Err()
	}
	if verdict, ok := v.cache.get(snapshotID); ok {
		return verdict, nil
	}
	graph, err := s.WorkflowSnapshotGraph(ctx, tenant, workflowID, snapshotID)
	if errors.Is(err, store.ErrWorkflowSnapshotNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	verdict := v.decide(graph)
	v.cache.put(snapshotID, verdict)
	return verdict, nil
}

// verdictCache remembers taskdecomp.CanDecompose per published snapshot id. Published snapshots are
// immutable, so an entry never goes stale; the oldest insertion is evicted once the cache is full.
// A snapshot id is a global uuid that is only looked up after the tenant-scoped ref query returned
// it, so sharing the cache across tenants cannot answer for a snapshot the caller cannot see.
type verdictCache struct {
	mu       sync.Mutex
	limit    int
	verdicts map[string]bool
	order    []string // insertion order, oldest first
}

func newVerdictCache(limit int) *verdictCache {
	return &verdictCache{limit: limit, verdicts: make(map[string]bool, limit)}
}

func (c *verdictCache) get(snapshotID string) (verdict, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	verdict, ok = c.verdicts[snapshotID]
	return verdict, ok
}

func (c *verdictCache) put(snapshotID string, verdict bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.verdicts[snapshotID]; ok {
		return
	}
	if len(c.order) >= c.limit {
		delete(c.verdicts, c.order[0])
		c.order = c.order[1:]
	}
	c.verdicts[snapshotID] = verdict
	c.order = append(c.order, snapshotID)
}

type decompositionWorkflowView struct {
	ID                         string  `json:"id"`
	Name                       string  `json:"name"`
	PublishedVersion           *string `json:"publishedVersion"`
	CanDecompose               bool    `json:"canDecompose"`
	DecompositionBlockedReason string  `json:"decompositionBlockedReason"`
}

// listDecompositionWorkflows projects the picker-only verdict without making the general workflow
// editor and run clients depend on decomposition policy. Only published snapshots missing from the
// verdict cache are read and parsed, so a repeat request costs one id query; see
// decompositionVerdicts for how concurrent first requests share that work.
func (h *handler) listDecompositionWorkflows(w http.ResponseWriter, r *http.Request, a access) {
	s, ok := h.store.(decompositionCatalogStore)
	if !ok {
		fault(w, http.StatusServiceUnavailable, "workflow_store_unavailable")
		return
	}
	refs, err := s.WorkflowRefs(r.Context(), a.TenantID)
	if err != nil {
		workflowFault(w, err, "list decomposition workflows", "")
		return
	}
	items := make([]decompositionWorkflowView, 0, len(refs))
	for _, ref := range refs {
		item := decompositionWorkflowView{ID: ref.ID, Name: ref.Name, PublishedVersion: ref.PublishedVersion}
		if ref.PublishedSnapshotID == nil {
			item.DecompositionBlockedReason = "工作流尚未发布。"
			items = append(items, item)
			continue
		}
		verdict, err := h.verdicts.verdict(r.Context(), s, a.TenantID, ref)
		if errors.Is(err, errVerdictPanicked) {
			fault(w, http.StatusInternalServerError, "internal") // logged once where it was recovered
			return
		}
		if err != nil {
			workflowFault(w, err, "list decomposition workflows", "")
			return
		}
		item.CanDecompose = verdict
		if !verdict {
			item.DecompositionBlockedReason = taskdecomp.OutputRequirement
		}
		items = append(items, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}
