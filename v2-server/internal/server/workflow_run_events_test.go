package server_test

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/server"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
)

// fakeRunEvents is a run invalidation hub a test publishes on by hand.
type fakeRunEvents struct {
	mu   sync.Mutex
	subs map[string][]chan struct{}
}

func (f *fakeRunEvents) Subscribe(runID string) (<-chan struct{}, func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.subs == nil {
		f.subs = map[string][]chan struct{}{}
	}
	ch := make(chan struct{}, 1)
	f.subs[runID] = append(f.subs[runID], ch)
	return ch, func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		kept := f.subs[runID][:0]
		for _, c := range f.subs[runID] {
			if c != ch {
				kept = append(kept, c)
			}
		}
		f.subs[runID] = kept
	}
}

func (f *fakeRunEvents) publish(runID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, ch := range f.subs[runID] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (f *fakeRunEvents) subscribers(runID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.subs[runID])
}

// pendingRun creates a published workflow and a pending run of it in a fresh project.
func pendingRun(t *testing.T, call workflowCall, s *store.Store) (tenant, user, runID string) {
	t.Helper()
	tenant = workflowTenant(t, call)
	_, me := call("GET", "/api/v1/me", nil)
	user = me["id"].(string)
	project := workflowRunProject(t, s, user, tenant)
	base := "/api/v2/tenants/" + tenant + "/workflows"
	_, created := call("POST", base, map[string]any{"name": "事件流", "graph": nil})
	id := created["workflow"].(map[string]any)["id"].(string)
	call("POST", base+"/"+id+"/publish", map[string]any{"version": "v1"})
	status, run := call("POST", base+"/"+id+"/runs", map[string]any{"projectId": project.ID})
	if status != 201 {
		t.Fatalf("create run = %d %v", status, run)
	}
	return tenant, user, run["run"].(map[string]any)["id"].(string)
}

// sseLines reads the non-blank lines of an open event stream, one at a time.
func sseLines(t *testing.T, body io.Reader) <-chan string {
	t.Helper()
	lines := make(chan string)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(body)
		for sc.Scan() {
			if l := sc.Text(); l != "" {
				lines <- l
			}
		}
	}()
	return lines
}

func nextLine(t *testing.T, lines <-chan string) string {
	t.Helper()
	select {
	case l, ok := <-lines:
		if !ok {
			t.Fatal("stream closed")
		}
		return l
	case <-time.After(5 * time.Second):
		t.Fatal("no event within 5s")
	}
	return ""
}

func TestWorkflowRunEventsAnnounceEveryChange(t *testing.T) {
	hub := &fakeRunEvents{}
	call, s, srv := workflowServerWithClient(t, func(o *server.Options) { o.WorkflowRunEvents = hub })
	tenant, _, runID := pendingRun(t, call, s)
	path := "/api/v2/tenants/" + tenant + "/workflow-runs/" + runID + "/events"

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.base+path, nil)
	res, err := srv.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 || res.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("events = %d %s", res.StatusCode, res.Header.Get("Content-Type"))
	}
	lines := sseLines(t, res.Body)
	invalidated := func() {
		t.Helper()
		if l := nextLine(t, lines); l != "event: workflow_run_invalidated" {
			t.Fatalf("event line = %q", l)
		}
		var data map[string]string
		if l := nextLine(t, lines); json.Unmarshal([]byte(strings.TrimPrefix(l, "data: ")), &data) != nil || len(data) != 1 || data["runId"] != runID {
			t.Fatalf("data line = %q", l)
		}
	}
	// One at once, for whatever changed between the page's read and this subscription.
	invalidated()
	hub.publish(runID)
	invalidated()
	hub.publish("00000000-0000-4000-8000-000000000000")
	hub.publish(runID)
	invalidated()

	// Closing the stream releases the subscription.
	cancel()
	deadline := time.Now().Add(5 * time.Second)
	for hub.subscribers(runID) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the subscription outlived its stream")
		}
		time.Sleep(10 * time.Millisecond)
	}

	if status, failure := call("GET", "/api/v2/tenants/"+tenant+"/workflow-runs/00000000-0000-4000-8000-000000000000/events", nil); status != 404 || failure["code"] != "workflow_run_not_found" {
		t.Fatalf("unknown run = %d %v", status, failure)
	}
}

func TestWorkflowRunEventsNeedTheRunHost(t *testing.T) {
	call, s := workflowServerWithStore(t, nil)
	tenant, _, runID := pendingRun(t, call, s)
	status, failure := call("GET", "/api/v2/tenants/"+tenant+"/workflow-runs/"+runID+"/events", nil)
	if status != 503 || failure["code"] != "workflow_run_events_unavailable" {
		t.Fatalf("without a host = %d %v", status, failure)
	}
}

func TestWorkflowNodeEventsRelayTheAttemptSession(t *testing.T) {
	rt, _ := chatRuntime()
	var mu sync.Mutex
	var seen []execution.Scope
	replay := rt.SubscribeFunc
	rt.SubscribeFunc = func(ctx context.Context, scope execution.Scope, sid string, after uint64) (execution.Stream, error) {
		mu.Lock()
		seen = append(seen, scope)
		mu.Unlock()
		if sid != "s-node" {
			return nil, execution.ErrNotFound
		}
		return replay(ctx, scope, sid, after)
	}
	call, s, srv := workflowServerWithClient(t, func(o *server.Options) { o.Execution, o.Catalog = rt, rt })
	tenant, user, runID := pendingRun(t, call, s)
	ctx := context.Background()
	if _, err := s.StartWorkflowRun(ctx, tenant, runID); err != nil {
		t.Fatal(err)
	}
	node, err := s.CreateWorkflowNodeRun(ctx, tenant, runID, store.WorkflowNodeRunInput{NodeID: "agent-1", NodeType: "agent"})
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/v2/tenants/" + tenant + "/workflow-runs/" + runID + "/nodes/" + node.ID + "/events"

	// Before dispatch the attempt has no session to read.
	if status, failure := call("GET", path, nil); status != 404 || failure["code"] != "workflow_node_session_not_found" {
		t.Fatalf("undispatched = %d %v", status, failure)
	}
	if _, err := s.BindWorkflowNodeSession(ctx, tenant, runID, node.ID, "s-node"); err != nil {
		t.Fatal(err)
	}

	read := func(query string) []string {
		t.Helper()
		res, err := srv.client.Get(srv.base + path + query)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		if res.StatusCode != 200 || res.Header.Get("Content-Type") != "text/event-stream" {
			t.Fatalf("events = %d %s", res.StatusCode, res.Header.Get("Content-Type"))
		}
		var lines []string
		for l := range sseLines(t, res.Body) {
			lines = append(lines, l)
		}
		return lines
	}
	// The chat stream's frames: sequence ids, a turn_ended with its prompt, then end.
	lines := read("")
	if len(lines) != 8 || lines[0] != "id: 1" || lines[2] != "id: 2" || lines[4] != "id: 3" || lines[6] != "event: end" {
		t.Fatalf("stream = %q", lines)
	}
	var ended map[string]any
	_ = json.Unmarshal([]byte(strings.TrimPrefix(lines[3], "data: ")), &ended)
	if ended["kind"] != "turn_ended" || ended["prompt"] != "你好" || ended["result"].(map[string]any)["answer"] != "好" {
		t.Fatalf("turn_ended = %v", ended)
	}
	if resumed := read("?after=2"); len(resumed) != 4 || resumed[0] != "id: 3" {
		t.Fatalf("resumed = %q", resumed)
	}
	// The session is read in the caller's own scope, never another member's.
	mu.Lock()
	for _, scope := range seen {
		if scope != (execution.Scope{TenantID: tenant, UserID: user}) {
			t.Errorf("subscribed as %+v", scope)
		}
	}
	mu.Unlock()

	if status, failure := call("GET", path+"?after=x", nil); status != 400 || failure["code"] != "invalid_cursor" {
		t.Fatalf("bad cursor = %d %v", status, failure)
	}
	other := "/api/v2/tenants/" + tenant + "/workflow-runs/" + runID + "/nodes/00000000-0000-4000-8000-000000000000/events"
	if status, failure := call("GET", other, nil); status != 404 || failure["code"] != "workflow_node_run_not_found" {
		t.Fatalf("unknown attempt = %d %v", status, failure)
	}
	missingRun := "/api/v2/tenants/" + tenant + "/workflow-runs/00000000-0000-4000-8000-000000000000/nodes/" + node.ID + "/events"
	if status, failure := call("GET", missingRun, nil); status != 404 || failure["code"] != "workflow_run_not_found" {
		t.Fatalf("unknown run = %d %v", status, failure)
	}
}

func TestWorkflowNodeEventsOfAnotherMembersSessionAreNotFound(t *testing.T) {
	rt, _ := chatRuntime()
	// The execution service keeps the session in its starter's scope; anyone else does not find it.
	rt.SubscribeFunc = func(context.Context, execution.Scope, string, uint64) (execution.Stream, error) {
		return nil, execution.ErrNotFound
	}
	call, s, _ := workflowServerWithClient(t, func(o *server.Options) { o.Execution, o.Catalog = rt, rt })
	tenant, _, runID := pendingRun(t, call, s)
	ctx := context.Background()
	if _, err := s.StartWorkflowRun(ctx, tenant, runID); err != nil {
		t.Fatal(err)
	}
	node, err := s.CreateWorkflowNodeRun(ctx, tenant, runID, store.WorkflowNodeRunInput{NodeID: "agent-1", NodeType: "agent"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.BindWorkflowNodeSession(ctx, tenant, runID, node.ID, "s-starter"); err != nil {
		t.Fatal(err)
	}
	path := "/api/v2/tenants/" + tenant + "/workflow-runs/" + runID + "/nodes/" + node.ID + "/events"
	if status, failure := call("GET", path, nil); status != 404 || failure["code"] != "workflow_node_session_not_found" {
		t.Fatalf("another member's session = %d %v", status, failure)
	}
}
