package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"reflect"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/taskdecomp"
)

const (
	catalogTasksGraph   = `{"nodes":[{"id":"start","data":{"kind":"start"}},{"id":"out","data":{"kind":"output","outputs":[{"name":"tasks","variableSelector":["start","input"]}]}}],"edges":[{"source":"start","target":"out"}]}`
	catalogSummaryGraph = `{"nodes":[{"id":"start","data":{"kind":"start"}},{"id":"out","data":{"kind":"output","outputs":[{"name":"summary","variableSelector":["start","input"]}]}}],"edges":[{"source":"start","target":"out"}]}`
)

// countingCatalog serves fixed refs and graphs and records every snapshot read, so the test can
// tell a cache hit from a database read. With a non-nil release channel each read also blocks until
// it receives a value (or the read's context ends), so a test can hold reads in flight.
type countingCatalog struct {
	Store   // nil: only the catalog methods are called
	refs    []store.WorkflowRef
	graphs  map[string]string
	release chan struct{}

	mu      sync.Mutex
	err     error // returned by every read while set, like a transient database failure
	reads   []string
	reading []string // reads entered and not yet released, in entry order
}

func (c *countingCatalog) WorkflowRefs(context.Context, string) ([]store.WorkflowRef, error) {
	return c.refs, nil
}

func (c *countingCatalog) WorkflowSnapshotGraph(ctx context.Context, _, _, id string) (string, error) {
	c.mu.Lock()
	c.reads = append(c.reads, id)
	c.reading = append(c.reading, id)
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.reading = slices.DeleteFunc(c.reading, func(r string) bool { return r == id })
		c.mu.Unlock()
	}()
	if c.release != nil {
		select {
		case <-c.release:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	c.mu.Lock()
	err := c.err
	c.mu.Unlock()
	if err != nil {
		return "", err
	}
	graph, ok := c.graphs[id]
	if !ok {
		return "", store.ErrWorkflowSnapshotNotFound
	}
	return graph, nil
}

// snapshot returns copies of the completed-or-entered reads and the reads still held.
func (c *countingCatalog) snapshot() (reads, reading []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.reads), slices.Clone(c.reading)
}

func ref(id, snapshot string) store.WorkflowRef {
	version := "v1"
	return store.WorkflowRef{ID: id, Name: id, PublishedVersion: &version, PublishedSnapshotID: &snapshot}
}

func catalog(t *testing.T, h *handler) []decompositionWorkflowView {
	t.Helper()
	items, err := requestCatalog(h)
	if err != nil {
		t.Fatal(err)
	}
	return items
}

func serveCatalog(h *handler) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.listDecompositionWorkflows(rec, httptest.NewRequest("GET", "/", nil), access{Access: store.Access{TenantID: "t"}})
	return rec
}

// requestCatalog serves one catalog request; it reports failures instead of failing the test so
// concurrent requests can call it from their own goroutines.
func requestCatalog(h *handler) ([]decompositionWorkflowView, error) {
	rec := serveCatalog(h)
	var body struct{ Items []decompositionWorkflowView }
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || rec.Code != 200 {
		return nil, fmt.Errorf("catalog = %d %s (%v)", rec.Code, rec.Body, err)
	}
	return body.Items, nil
}

func TestDecompositionCatalogCachesVerdictPerPublishedSnapshot(t *testing.T) {
	s := &countingCatalog{
		refs:   []store.WorkflowRef{ref("ok", "s-ok"), ref("blocked", "s-blocked"), {ID: "draft", Name: "draft"}},
		graphs: map[string]string{"s-ok": catalogTasksGraph, "s-blocked": catalogSummaryGraph},
	}
	h := &handler{store: s, verdicts: newDecompositionVerdicts(context.Background())}
	want := []decompositionWorkflowView{
		{ID: "ok", Name: "ok", PublishedVersion: s.refs[0].PublishedVersion, CanDecompose: true},
		{ID: "blocked", Name: "blocked", PublishedVersion: s.refs[1].PublishedVersion, DecompositionBlockedReason: taskdecomp.OutputRequirement},
		{ID: "draft", Name: "draft", DecompositionBlockedReason: "工作流尚未发布。"},
	}
	for round := range 2 {
		if got := catalog(t, h); !reflect.DeepEqual(got, want) {
			t.Fatalf("round %d: catalog = %+v, want %+v", round, got, want)
		}
	}
	if !slices.Equal(s.reads, []string{"s-ok", "s-blocked"}) {
		t.Fatalf("snapshot reads = %v, want each published snapshot read once", s.reads)
	}

	// Republishing points the workflow at a new snapshot id, which is judged afresh.
	s.refs[1] = ref("blocked", "s-blocked-v2")
	s.graphs["s-blocked-v2"] = catalogTasksGraph
	if got := catalog(t, h); !got[1].CanDecompose || got[1].DecompositionBlockedReason != "" {
		t.Fatalf("republished = %+v", got[1])
	}

	// A snapshot that vanished between the two queries is refused without being cached.
	s.refs = []store.WorkflowRef{ref("gone", "s-gone")}
	s.reads = nil
	for range 2 {
		if got := catalog(t, h); got[0].CanDecompose {
			t.Fatalf("vanished snapshot = %+v", got[0])
		}
	}
	if len(s.reads) != 2 {
		t.Fatalf("vanished snapshot reads = %v, want no caching", s.reads)
	}
}

func TestVerdictCacheEvictsOldest(t *testing.T) {
	c := newVerdictCache(2)
	c.put("a", true)
	c.put("b", false)
	c.put("a", false) // already present: neither overwritten nor reordered
	c.put("c", true)
	if _, ok := c.get("a"); ok {
		t.Fatal("oldest entry a survived eviction")
	}
	for id, want := range map[string]bool{"b": false, "c": true} {
		if got, ok := c.get(id); !ok || got != want {
			t.Fatalf("%s = %v %v, want %v", id, got, ok, want)
		}
	}
	if len(c.verdicts) != 2 || len(c.order) != 2 {
		t.Fatalf("cache grew past its limit: %v %v", c.verdicts, c.order)
	}
}

// TestDecompositionCatalogConcurrentFirstRequestsShareEachParse is the post-restart burst: K members
// open the picker at once with an empty cache. Every snapshot must be read and parsed exactly once,
// with the other requests waiting on that one judgement rather than repeating it.
func TestDecompositionCatalogConcurrentFirstRequestsShareEachParse(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const k = 5
		s := &countingCatalog{
			refs:    []store.WorkflowRef{ref("a", "s-a"), ref("b", "s-b"), ref("c", "s-c")},
			graphs:  map[string]string{"s-a": catalogTasksGraph, "s-b": catalogSummaryGraph, "s-c": catalogTasksGraph},
			release: make(chan struct{}),
		}
		h := &handler{store: s, verdicts: newDecompositionVerdicts(context.Background())}
		var wg sync.WaitGroup
		results := make([][]decompositionWorkflowView, k)
		errs := make([]error, k)
		for i := range k {
			wg.Go(func() { results[i], errs[i] = requestCatalog(h) })
		}
		// Every request walks the refs in order, so each snapshot is reached by all K requests while
		// its single read is held; one read at a time is in flight, and it is never repeated.
		for i, id := range []string{"s-a", "s-b", "s-c"} {
			synctest.Wait()
			reads, reading := s.snapshot()
			if !slices.Equal(reading, []string{id}) || len(reads) != i+1 {
				t.Fatalf("before releasing %s: reads %v, in flight %v; want one shared read per snapshot", id, reads, reading)
			}
			s.release <- struct{}{}
		}
		wg.Wait()
		if reads, _ := s.snapshot(); !slices.Equal(reads, []string{"s-a", "s-b", "s-c"}) {
			t.Fatalf("reads = %v, want each snapshot exactly once", reads)
		}
		for i := range k {
			if errs[i] != nil {
				t.Fatalf("request %d: %v", i, errs[i])
			}
			got := []bool{results[i][0].CanDecompose, results[i][1].CanDecompose, results[i][2].CanDecompose}
			if !slices.Equal(got, []bool{true, false, true}) {
				t.Fatalf("request %d verdicts = %v", i, got)
			}
		}
	})
}

// TestDecompositionVerdictsLimitConcurrentParses checks the process-wide bound: distinct snapshots
// judged at once never read more than maxConcurrentVerdictParses graphs at the same time, and the
// queued ones proceed as slots free up.
func TestDecompositionVerdictsLimitConcurrentParses(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ids := []string{"s-1", "s-2", "s-3", "s-4", "s-5"}
		s := &countingCatalog{graphs: map[string]string{}, release: make(chan struct{})}
		for _, id := range ids {
			s.graphs[id] = catalogTasksGraph
		}
		v := newDecompositionVerdicts(context.Background())
		var wg sync.WaitGroup
		for _, id := range ids {
			wg.Go(func() {
				if ok, err := v.verdict(t.Context(), s, "t", ref("w-"+id, id)); err != nil || !ok {
					t.Errorf("%s = %v %v", id, ok, err)
				}
			})
		}
		for remaining := len(ids); remaining > 0; remaining-- {
			synctest.Wait()
			_, reading := s.snapshot()
			// The README documents a limit of 2; pin it so a change is deliberate.
			if want := min(remaining, 2); len(reading) != want {
				t.Fatalf("%d judgements left: %d reads in flight %v, want %d", remaining, len(reading), reading, want)
			}
			s.release <- struct{}{}
		}
		wg.Wait()
		if reads, _ := s.snapshot(); len(reads) != len(ids) {
			t.Fatalf("reads = %v, want each snapshot once", reads)
		}
	})
}

// TestDecompositionVerdictOutlivesTheRequestThatStartedIt: a picker closed mid-request must not
// fail the requests sharing its judgement, so the caller stops waiting while the read finishes on
// its own context and caches the answer.
func TestDecompositionVerdictOutlivesTheRequestThatStartedIt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := &countingCatalog{graphs: map[string]string{"s-a": catalogTasksGraph}, release: make(chan struct{})}
		v := newDecompositionVerdicts(context.Background())
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() {
			_, err := v.verdict(ctx, s, "t", ref("a", "s-a"))
			done <- err
		}()
		synctest.Wait()
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled caller = %v, want context.Canceled", err)
		}
		s.release <- struct{}{}
		synctest.Wait()
		if verdict, ok := v.cache.get("s-a"); !ok || !verdict {
			t.Fatalf("cache after the abandoned judgement = %v %v, want true", verdict, ok)
		}
		if ok, err := v.verdict(t.Context(), s, "t", ref("a", "s-a")); err != nil || !ok {
			t.Fatalf("next caller = %v %v", ok, err)
		}
		if reads, _ := s.snapshot(); len(reads) != 1 {
			t.Fatalf("reads = %v, want the abandoned read reused", reads)
		}
	})
}

// failedCatalog asserts a catalog request failed with the stable internal fault and cached nothing
// for snapshot, so the next request judges it again.
func failedCatalog(t *testing.T, h *handler, snapshot string) {
	t.Helper()
	rec := serveCatalog(h)
	var body struct{ Code string }
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || rec.Code != 500 || body.Code != "internal" {
		t.Fatalf("catalog = %d %s (%v), want 500 internal", rec.Code, rec.Body, err)
	}
	if verdict, ok := h.verdicts.cache.get(snapshot); ok {
		t.Fatalf("failed judgement cached %v for %s", verdict, snapshot)
	}
}

// TestDecompositionCatalogPanicIsAnUncachedError: singleflight re-raises a judgement's panic where
// nothing can recover it, so without the judgement's own recover one bad graph would end the
// process (and, with the cache empty after the restart, end it again on the next picker open).
func TestDecompositionCatalogPanicIsAnUncachedError(t *testing.T) {
	s := &countingCatalog{refs: []store.WorkflowRef{ref("a", "s-a")}, graphs: map[string]string{"s-a": catalogTasksGraph}}
	h := &handler{store: s, verdicts: newDecompositionVerdicts(context.Background())}
	h.verdicts.decide = func(string) bool { panic("injected parser bug") }
	failedCatalog(t, h, "s-a")
	h.verdicts.decide = taskdecomp.CanDecompose
	if got := catalog(t, h); !got[0].CanDecompose {
		t.Fatalf("after the panic = %+v, want judged afresh", got[0])
	}
	if reads, _ := s.snapshot(); len(reads) != 2 {
		t.Fatalf("reads = %v, want the panicked snapshot read again", reads)
	}
}

// TestDecompositionCatalogStoreErrorIsNotCached: a transient database failure must fail the
// request, never be reported or remembered as "not decomposable", or a valid workflow would stay
// blocked with the tasks requirement as its reason until eviction or restart.
func TestDecompositionCatalogStoreErrorIsNotCached(t *testing.T) {
	s := &countingCatalog{refs: []store.WorkflowRef{ref("a", "s-a")}, graphs: map[string]string{"s-a": catalogTasksGraph},
		err: errors.New("pool acquire timeout")}
	h := &handler{store: s, verdicts: newDecompositionVerdicts(context.Background())}
	failedCatalog(t, h, "s-a")
	s.mu.Lock()
	s.err = nil
	s.mu.Unlock()
	if got := catalog(t, h); !got[0].CanDecompose || got[0].DecompositionBlockedReason != "" {
		t.Fatalf("after the database recovered = %+v, want decomposable", got[0])
	}
}

// TestDecompositionVerdictsLimitConcurrentParsesNotOnlyReads: the parse allocates most of a
// judgement's memory, so the slot must cover it, not just the read before it.
func TestDecompositionVerdictsLimitConcurrentParsesNotOnlyReads(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ids := []string{"s-1", "s-2", "s-3", "s-4", "s-5"}
		s := &countingCatalog{graphs: map[string]string{}}
		for _, id := range ids {
			s.graphs[id] = catalogTasksGraph
		}
		v := newDecompositionVerdicts(context.Background())
		var mu sync.Mutex
		parsing := 0
		parsed := make(chan struct{})
		v.decide = func(graph string) bool {
			mu.Lock()
			parsing++
			mu.Unlock()
			<-parsed
			mu.Lock()
			parsing--
			mu.Unlock()
			return taskdecomp.CanDecompose(graph)
		}
		var wg sync.WaitGroup
		for _, id := range ids {
			wg.Go(func() {
				if ok, err := v.verdict(t.Context(), s, "t", ref("w-"+id, id)); err != nil || !ok {
					t.Errorf("%s = %v %v", id, ok, err)
				}
			})
		}
		for remaining := len(ids); remaining > 0; remaining-- {
			synctest.Wait()
			mu.Lock()
			got := parsing
			mu.Unlock()
			if want := min(remaining, 2); got != want {
				t.Fatalf("%d judgements left: %d parses in progress, want %d", remaining, got, want)
			}
			parsed <- struct{}{}
		}
		wg.Wait()
	})
}

// TestDecompositionVerdictStopsOnShutdown: the judgement is detached from its request but not from
// the server, so shutdown ends a stalled read at once instead of after verdictTimeout, and the
// database pool can close.
func TestDecompositionVerdictStopsOnShutdown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		lifetime, shutdown := context.WithCancel(context.Background())
		s := &countingCatalog{graphs: map[string]string{"s-a": catalogTasksGraph}, release: make(chan struct{})}
		v := newDecompositionVerdicts(lifetime)
		done := make(chan error, 1)
		go func() {
			_, err := v.verdict(t.Context(), s, "t", ref("a", "s-a"))
			done <- err
		}()
		synctest.Wait()
		if _, reading := s.snapshot(); !slices.Equal(reading, []string{"s-a"}) {
			t.Fatalf("in flight before shutdown = %v", reading)
		}
		start := time.Now()
		shutdown()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("caller after shutdown = %v, want context.Canceled", err)
		}
		synctest.Wait()
		if _, reading := s.snapshot(); len(reading) != 0 || time.Since(start) != 0 {
			t.Fatalf("read still in flight %v after %v, want it ended at shutdown", reading, time.Since(start))
		}
		if _, ok := v.cache.get("s-a"); ok {
			t.Fatal("judgement cut short by shutdown was cached")
		}
	})
}

// TestDecompositionVerdictTimesOut: a read that never answers ends after verdictTimeout even when
// the request waiting on it does not, so a stalled database cannot pin a parse slot forever.
func TestDecompositionVerdictTimesOut(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := &countingCatalog{graphs: map[string]string{"s-a": catalogTasksGraph}, release: make(chan struct{})}
		v := newDecompositionVerdicts(context.Background())
		start := time.Now()
		if _, err := v.verdict(t.Context(), s, "t", ref("a", "s-a")); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("stalled read = %v, want context.DeadlineExceeded", err)
		}
		if elapsed := time.Since(start); elapsed != 30*time.Second {
			t.Fatalf("stalled read ended after %v, want 30s", elapsed)
		}
	})
}

// TestDecompositionVerdictFlightIsPerSnapshot: a republish during a judgement gives the workflow a
// new snapshot id; asking about it must not join the old snapshot's judgement and get its verdict.
func TestDecompositionVerdictFlightIsPerSnapshot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := &countingCatalog{graphs: map[string]string{"s-old": catalogTasksGraph, "s-new": catalogSummaryGraph},
			release: make(chan struct{})}
		v := newDecompositionVerdicts(context.Background())
		verdicts := map[string]bool{}
		var mu sync.Mutex
		var wg sync.WaitGroup
		for _, id := range []string{"s-old", "s-new"} {
			wg.Go(func() {
				ok, err := v.verdict(t.Context(), s, "t", ref("w", id))
				if err != nil {
					t.Errorf("%s: %v", id, err)
				}
				mu.Lock()
				verdicts[id] = ok
				mu.Unlock()
			})
		}
		synctest.Wait()
		if _, reading := s.snapshot(); len(reading) != 2 {
			t.Fatalf("in flight = %v, want both snapshots judged on their own", reading)
		}
		s.release <- struct{}{}
		s.release <- struct{}{}
		wg.Wait()
		if !verdicts["s-old"] || verdicts["s-new"] {
			t.Fatalf("verdicts = %v, want s-old true and s-new false", verdicts)
		}
	})
}

// TestDecompositionVerdictCacheHitSkipsTheQueue: a cached answer is served without waiting for a
// parse slot, so a burst of first requests over other snapshots never delays a repeat picker.
func TestDecompositionVerdictCacheHitSkipsTheQueue(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := &countingCatalog{graphs: map[string]string{"s-1": catalogTasksGraph, "s-2": catalogTasksGraph},
			release: make(chan struct{})}
		v := newDecompositionVerdicts(context.Background())
		v.cache.put("s-hit", true)
		var wg sync.WaitGroup
		for _, id := range []string{"s-1", "s-2"} {
			wg.Go(func() { _, _ = v.verdict(t.Context(), s, "t", ref("w-"+id, id)) })
		}
		synctest.Wait() // both slots are now held by stalled reads
		if ok, err := v.verdict(t.Context(), s, "t", ref("w-hit", "s-hit")); err != nil || !ok {
			t.Fatalf("cache hit = %v %v", ok, err)
		}
		s.release <- struct{}{}
		s.release <- struct{}{}
		wg.Wait()
	})
}

// TestDecompositionCatalogStillHitsAboveAThousandSnapshots: requests walk refs in a fixed order,
// which defeats FIFO eviction as soon as the walked set exceeds the cache bound, so a bound close to
// real deployments would re-parse every graph on every picker open. 1100 snapshots must stay cached.
func TestDecompositionCatalogStillHitsAboveAThousandSnapshots(t *testing.T) {
	s := &countingCatalog{graphs: map[string]string{}}
	for i := range 1100 {
		id := fmt.Sprintf("s-%04d", i)
		s.refs = append(s.refs, ref("w-"+id, id))
		s.graphs[id] = catalogTasksGraph
	}
	h := &handler{store: s, verdicts: newDecompositionVerdicts(context.Background())}
	catalog(t, h)
	catalog(t, h)
	if reads, _ := s.snapshot(); len(reads) != len(s.refs) {
		t.Fatalf("two walks over %d snapshots read %d graphs, want each once", len(s.refs), len(reads))
	}
}
