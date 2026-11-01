package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"
)

// memoryRepo is a test double of the engine port with the store's transition rules: only
// running attempts complete or fail, a failure fails a running run, and a pending run starts
// once. The PostgreSQL implementation is exercised separately in the store package.
type memoryRepo struct {
	run     RunContext
	runs    []NodeRun
	payload map[string][]byte
	next    int
	output  *string
}

func newMemoryRepo(graph string, input *string) *memoryRepo {
	return &memoryRepo{run: RunContext{RunID: "run", WorkflowID: "wf", ProjectID: "p", Status: StatusPending, Input: input, Payload: []byte("{}"), Graph: graph}, payload: map[string][]byte{}}
}

func (m *memoryRepo) RunContext(context.Context, string) (RunContext, error) { return m.run, nil }
func (m *memoryRepo) NodeRuns(context.Context, string) ([]NodeRun, error) {
	return append([]NodeRun{}, m.runs...), nil
}

func (m *memoryRepo) insert(s NodeStart) NodeRun {
	m.next++
	r := NodeRun{ID: fmt.Sprintf("nr%d", m.next), NodeID: s.NodeID, NodeType: s.NodeType, Status: StatusRunning}
	m.runs = append(m.runs, r)
	return r
}

func (m *memoryRepo) StartRun(_ context.Context, _ string, entry NodeStart, prepare func([]byte) ([]byte, error)) (bool, error) {
	if m.run.Status != StatusPending || len(m.runs) != 0 {
		return false, nil
	}
	payload, err := prepare(m.run.Payload)
	if err != nil {
		return false, err
	}
	m.run.Payload, m.run.Status = payload, StatusRunning
	m.insert(entry)
	return true, nil
}

func (m *memoryRepo) StartNodes(_ context.Context, _ string, wave []NodeStart) ([]NodeRun, error) {
	created := []NodeRun{}
	for _, s := range wave {
		created = append(created, m.insert(s))
	}
	return created, nil
}

func (m *memoryRepo) find(id string) *NodeRun {
	for i := range m.runs {
		if m.runs[i].ID == id {
			return &m.runs[i]
		}
	}
	return nil
}

func (m *memoryRepo) CompleteNode(_ context.Context, _, id string, output *string, keys map[string]any, fold func([]byte, NodeRun) ([]byte, error)) (bool, error) {
	r := m.find(id)
	if r == nil || r.Status != StatusRunning {
		return false, nil
	}
	payload, err := fold(m.run.Payload, *r)
	if err != nil {
		return false, err
	}
	m.run.Payload = payload
	now := time.Unix(int64(m.next), 0)
	r.Status, r.Output, r.FinishedAt = StatusSucceeded, output, &now
	m.payload[id], _ = MergePayload(m.payload[id], keys)
	return true, nil
}

func (m *memoryRepo) FailNode(_ context.Context, _, id, _ string, output *string, detail func(int, []byte) map[string]any) (bool, error) {
	r := m.find(id)
	if r == nil || r.Status != StatusRunning {
		return false, nil
	}
	r.Status, r.Output = StatusFailed, output
	m.payload[id], _ = MergePayload(m.payload[id], map[string]any{"error_detail": detail(1, m.run.Payload)})
	if m.run.Status == StatusRunning {
		m.run.Status = StatusFailed
	}
	return true, nil
}

func (m *memoryRepo) FinishRun(_ context.Context, _ string, output *string) error {
	m.run.Status, m.output = StatusSucceeded, output
	return nil
}

func (m *memoryRepo) UpdateRunInput(_ context.Context, _ string, input *string, fold func([]byte) ([]byte, error)) error {
	payload, err := fold(m.run.Payload)
	if err != nil {
		return err
	}
	m.run.Input, m.run.Payload = input, payload
	return nil
}

// recorder captures dispatches instead of running Agents.
type recorder struct{ dispatched []AgentDispatch }

func (r *recorder) Dispatch(d AgentDispatch) { r.dispatched = append(r.dispatched, d) }

const chain = `{"nodes":[
 {"id":"start","data":{"kind":"start","inputVariables":[{"name":"topic","valueType":"string","required":true}]}},
 {"id":"a","data":{"kind":"agent","title":"Draft","agentConfig":{"executor":{"agentCli":"official/ora-space.opencode","modelId":"fixture"},"prompt":"Write about {{#start.topic#}}"}}},
 {"id":"b","data":{"kind":"agent","agentConfig":{"prompt":"Review: {{#a.output#}}"}}},
 {"id":"out","data":{"kind":"output","outputs":[{"name":"review","variableSelector":["b","output"]}]}}
],"edges":[{"source":"start","target":"a"},{"source":"a","target":"b"},{"source":"b","target":"out"}]}`

func str(s string) *string { return &s }

func startedEngine(t *testing.T, graph string) (*Engine, *memoryRepo, *recorder) {
	t.Helper()
	repo, agents := newMemoryRepo(graph, str("kickoff")), &recorder{}
	e := NewEngine(repo, agents, nil, func() time.Time { return time.UnixMilli(1700) })
	ctx := context.Background()
	values := map[string]json.RawMessage{}
	if graph == chain {
		values["topic"] = json.RawMessage(`"payments"`)
	}
	if err := e.UpdateInput(ctx, "run", str("kickoff"), values); err != nil {
		t.Fatalf("update input: %v", err)
	}
	if err := e.Start(ctx, "run"); err != nil {
		t.Fatalf("start: %v", err)
	}
	return e, repo, agents
}

// The basic closure: each Agent sees its predecessors' answers in the pool, and the Output
// node's bindings become the run output.
func TestEngineRunsStartAgentAgentOutput(t *testing.T) {
	e, repo, agents := startedEngine(t, chain)
	ctx := context.Background()
	if len(agents.dispatched) != 1 || agents.dispatched[0].Node.ID != "a" {
		t.Fatalf("first wave = %+v", agents.dispatched)
	}
	prompt, err := RenderTemplate(agents.dispatched[0].Node.Agent().Prompt, &agents.dispatched[0].Pool)
	if err != nil || prompt != "Write about payments" {
		t.Fatalf("rendered a = %q, %v", prompt, err)
	}
	if err := e.CompleteNode(ctx, "run", agents.dispatched[0].NodeRunID, Completion{Output: str("draft text")}); err != nil {
		t.Fatal(err)
	}
	if len(agents.dispatched) != 2 || agents.dispatched[1].Node.ID != "b" {
		t.Fatalf("second wave = %+v", agents.dispatched)
	}
	prompt, err = RenderTemplate(agents.dispatched[1].Node.Agent().Prompt, &agents.dispatched[1].Pool)
	if err != nil || prompt != "Review: draft text" {
		t.Fatalf("rendered b = %q, %v", prompt, err)
	}
	// A duplicate callback for the finished attempt changes nothing.
	if err := e.CompleteNode(ctx, "run", agents.dispatched[0].NodeRunID, Completion{Output: str("again")}); err != nil || len(agents.dispatched) != 2 {
		t.Fatalf("duplicate completion: %v, %d dispatches", err, len(agents.dispatched))
	}
	if err := e.CompleteNode(ctx, "run", agents.dispatched[1].NodeRunID, Completion{Output: str("looks good"), Keys: map[string]any{"stop_reason": "end_turn"}}); err != nil {
		t.Fatal(err)
	}
	if repo.run.Status != StatusSucceeded || repo.output == nil || *repo.output != `{"review":"looks good"}` {
		t.Fatalf("run = %s, output = %v", repo.run.Status, repo.output)
	}
	payload, _, _ := DecodeRunPayload(repo.run.Payload)
	if payload.VariablePool.Values["start.input"] != "kickoff" || payload.VariablePool.Values["sys.workflow_id"] != "wf" {
		t.Fatalf("pool = %v", payload.VariablePool.Values)
	}
}

// Without an Output node the latest Agent answer is the run output (rank fallback).
func TestEngineFallsBackToAgentOutput(t *testing.T) {
	graph := `{"nodes":[{"id":"start","data":{"kind":"start"}},{"id":"a","data":{"kind":"agent","agentConfig":{"prompt":"x"}}}],
	 "edges":[{"source":"start","target":"a"}]}`
	e, repo, agents := startedEngine(t, graph)
	if err := e.CompleteNode(context.Background(), "run", agents.dispatched[0].NodeRunID, Completion{Output: str("answer")}); err != nil {
		t.Fatal(err)
	}
	if repo.run.Status != StatusSucceeded || *repo.output != "answer" {
		t.Fatalf("run = %s, output = %v", repo.run.Status, repo.output)
	}
}

// A failed Agent fails the run, its successors never start, and a late success is ignored.
func TestEngineFailurePropagatesToRun(t *testing.T) {
	e, repo, agents := startedEngine(t, chain)
	ctx := context.Background()
	id := agents.dispatched[0].NodeRunID
	if err := e.FailNode(ctx, "run", id, Failure{Kind: FailureSession, Message: "agent crashed"}); err != nil {
		t.Fatal(err)
	}
	if repo.run.Status != StatusFailed {
		t.Fatalf("run = %s", repo.run.Status)
	}
	if err := e.CompleteNode(ctx, "run", id, Completion{Output: str("late")}); err != nil || len(agents.dispatched) != 1 {
		t.Fatalf("late completion: %v, %d dispatches", err, len(agents.dispatched))
	}
	var detail map[string]map[string]any
	_ = json.Unmarshal(repo.payload[id], &detail)
	if d := detail["error_detail"]; d["kind"] != "session" || d["resumable"] != true || d["attempt"] != float64(1) {
		t.Fatalf("error_detail = %v", d)
	}
}

func TestEngineStartRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		graph, code string
	}{
		"required start value": {chain, "workflow_run_input_invalid"},
		"unsupported kind": {`{"nodes":[{"id":"start","data":{"kind":"start"}},{"id":"c","data":{"kind":"condition","cases":[]}}],
			"edges":[{"source":"start","target":"c"}]}`, "workflow_run_unsupported_node_type"},
		"missing start": {`{"nodes":[{"id":"a","data":{"kind":"agent","agentConfig":{"prompt":"x"}}}],"edges":[]}`, "workflow_run_missing_start_node"},
		"output with successor": {`{"nodes":[{"id":"start","data":{"kind":"start"}},{"id":"o","data":{"kind":"output"}},{"id":"a","data":{"kind":"agent","agentConfig":{"prompt":"x"}}}],
			"edges":[{"source":"start","target":"o"},{"source":"o","target":"a"}]}`, "workflow_run_output_has_successors"},
	} {
		t.Run(name, func(t *testing.T) {
			repo := newMemoryRepo(tc.graph, nil)
			e := NewEngine(repo, &recorder{}, nil, time.Now)
			var startErr *StartError
			if err := e.Start(context.Background(), "run"); !errors.As(err, &startErr) || startErr.Code != tc.code {
				t.Fatalf("start error = %v, want %s", err, tc.code)
			}
			if repo.run.Status != StatusPending || len(repo.runs) != 0 {
				t.Fatalf("refused start changed the run: %s, %d rows", repo.run.Status, len(repo.runs))
			}
		})
	}
}

func TestEngineRejectsBadStartValues(t *testing.T) {
	repo := newMemoryRepo(chain, nil)
	e := NewEngine(repo, &recorder{}, nil, time.Now)
	for name, tc := range map[string]struct {
		values map[string]json.RawMessage
		reason string
	}{
		"undeclared":    {map[string]json.RawMessage{"nope": json.RawMessage(`"x"`)}, "undeclared"},
		"type mismatch": {map[string]json.RawMessage{"topic": json.RawMessage(`3`)}, "type_mismatch"},
	} {
		var rejection *InputRejection
		if err := e.UpdateInput(context.Background(), "run", nil, tc.values); !errors.As(err, &rejection) || rejection.Reason != tc.reason {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

func TestRenderTemplate(t *testing.T) {
	p := newPool()
	p.Declare("review.structured_output", "object", "review")
	p.Declare("start.topic", "string", "start")
	p.Declare("start.optional", "string", "start")
	if err := p.Set("review.structured_output", "review", map[string]any{"approved": true, "score": json.Number("92")}); err != nil {
		t.Fatal(err)
	}
	if err := p.Set("start.topic", "start", "检查支付模块"); err != nil {
		t.Fatal(err)
	}
	got, err := RenderTemplate("批准：{{#review.structured_output.approved#}}，主题：{{#start.topic#}}，全部：{{#review.structured_output#}}", &p)
	if want := `批准：true，主题：检查支付模块，全部：{"approved":true,"score":92}`; err != nil || got != want {
		t.Fatalf("rendered %q, %v", got, err)
	}
	for _, bad := range []string{"{{#start.optional#}}", "{{#other.text#}}", "{{#start.topic", "{{#only#}}"} {
		if _, err := RenderTemplate(bad, &p); err == nil {
			t.Errorf("%q rendered without error", bad)
		}
	}
	if err := p.Set("start.topic", "review", "x"); err == nil {
		t.Error("a foreign writer set a variable")
	}
}
