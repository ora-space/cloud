package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// Engine-level Condition / Aggregator scenarios, adapted from desktop's aggregator_tests.rs
// (JOIN_GRAPH, PARALLEL_GRAPH, START_EDGE_GRAPH, NO_MATCH_GRAPH) and engine.rs validation tests.

const joinGraph = `{"nodes":[
 {"id":"start","data":{"kind":"start"}},
 {"id":"cond","data":{"kind":"condition","cases":[{"id":"yes","logic":"and","conditions":[{"variableSelector":["start","input"],"operator":"equals","value":"a"}]}]}},
 {"id":"a","data":{"kind":"agent","agentConfig":{"executor":{"agentCli":"c","modelId":"m"},"prompt":"a"}}},
 {"id":"b","data":{"kind":"agent","agentConfig":{"executor":{"agentCli":"c","modelId":"m"},"prompt":"b"}}},
 {"id":"agg","data":{"kind":"aggregator","aggregatorConfig":{"variables":[["a","output"],["b","output"]]}}},
 {"id":"out","data":{"kind":"output","outputs":[{"name":"text","variableSelector":["agg","output"]}]}}
],"edges":[
 {"source":"start","target":"cond"},
 {"source":"cond","sourceHandle":"yes","target":"a"},
 {"source":"cond","sourceHandle":"else","target":"b"},
 {"source":"a","target":"agg"},{"source":"b","target":"agg"},{"source":"agg","target":"out"}
]}`

const parallelGraph = `{"nodes":[
 {"id":"start","data":{"kind":"start"}},
 {"id":"a","data":{"kind":"agent","agentConfig":{"prompt":"a"}}},
 {"id":"b","data":{"kind":"agent","agentConfig":{"prompt":"b"}}},
 {"id":"agg","data":{"kind":"aggregator","aggregatorConfig":{"variables":[["b","output"],["a","output"]]}}},
 {"id":"out","data":{"kind":"output"}}
],"edges":[{"source":"start","target":"a"},{"source":"start","target":"b"},{"source":"a","target":"agg"},{"source":"b","target":"agg"},{"source":"agg","target":"out"}]}`

func engineWithInput(t *testing.T, graph, input string) (*Engine, *memoryRepo, *recorder) {
	t.Helper()
	repo, agents := newMemoryRepo(graph, &input), &recorder{}
	e := NewEngine(repo, agents, nil, func() time.Time { return time.UnixMilli(1700) })
	if err := e.Start(context.Background(), "run"); err != nil {
		t.Fatalf("start: %v", err)
	}
	return e, repo, agents
}

func (m *memoryRepo) status(nodeID string) string {
	for _, r := range m.runs {
		if r.NodeID == nodeID {
			return r.Status
		}
	}
	return ""
}

func (m *memoryRepo) nodeOutput(nodeID string) string {
	for _, r := range m.runs {
		if r.NodeID == nodeID && r.Output != nil {
			return *r.Output
		}
	}
	return ""
}

func poolValue(t *testing.T, m *memoryRepo, key string) any {
	t.Helper()
	p, _, err := DecodeRunPayload(m.run.Payload)
	if err != nil {
		t.Fatal(err)
	}
	return p.VariablePool.Values[key]
}

func dispatched(agents *recorder, nodeID string) string {
	for _, d := range agents.dispatched {
		if d.Node.ID == nodeID {
			return d.NodeRunID
		}
	}
	return ""
}

func TestConditionRoutesToOneBranchAndTheAggregatorPassesItThrough(t *testing.T) {
	cases := []struct{ input, taken, skipped, answer string }{
		{"a", "a", "b", "A-output"},
		{"b", "b", "a", "B-output"},
	}
	for _, c := range cases {
		t.Run("input "+c.input, func(t *testing.T) {
			e, repo, agents := engineWithInput(t, joinGraph, c.input)
			if len(agents.dispatched) != 1 || agents.dispatched[0].Node.ID != c.taken {
				t.Fatalf("dispatched %+v, want only %s", agents.dispatched, c.taken)
			}
			// The decision is scheduler state: recorded in conditionDecisions, not the pool.
			p, _, _ := DecodeRunPayload(repo.run.Payload)
			want := map[string]string{"cond": map[bool]string{true: "yes", false: ElseBranch}[c.input == "a"]}
			if p.ConditionDecisions["cond"] != want["cond"] || len(p.ConditionDecisions) != 1 {
				t.Fatalf("decisions = %v, want %v", p.ConditionDecisions, want)
			}
			if err := e.CompleteNode(context.Background(), "run", agents.dispatched[0].NodeRunID, Completion{Output: str(c.answer)}); err != nil {
				t.Fatal(err)
			}
			if repo.status(c.skipped) != "" {
				t.Fatalf("the branch not taken created a row: %s", repo.status(c.skipped))
			}
			if repo.status("agg") != StatusSucceeded || repo.nodeOutput("agg") != `"`+c.answer+`"` || poolValue(t, repo, "agg.output") != c.answer {
				t.Fatalf("aggregator %s output %q pool %v", repo.status("agg"), repo.nodeOutput("agg"), poolValue(t, repo, "agg.output"))
			}
			if repo.run.Status != StatusSucceeded || *repo.output != `{"text":"`+c.answer+`"}` {
				t.Fatalf("run %s output %v", repo.run.Status, repo.output)
			}
		})
	}
}

func TestParallelFanInUsesTheDeclaredPriority(t *testing.T) {
	e, repo, agents := engineWithInput(t, parallelGraph, "kickoff")
	ctx := context.Background()
	if err := e.CompleteNode(ctx, "run", dispatched(agents, "a"), Completion{Output: str("A-output")}); err != nil {
		t.Fatal(err)
	}
	if repo.status("agg") != "" {
		t.Fatal("the aggregator started before every active branch finished")
	}
	if err := e.CompleteNode(ctx, "run", dispatched(agents, "b"), Completion{Output: str("B-output")}); err != nil {
		t.Fatal(err)
	}
	// b is declared first even though a finished first.
	if poolValue(t, repo, "agg.output") != "B-output" || repo.run.Status != StatusSucceeded {
		t.Fatalf("aggregated %v, run %s", poolValue(t, repo, "agg.output"), repo.run.Status)
	}
}

func TestAggregatorPassesFalsyValuesAndFailsWithoutAnyAssigned(t *testing.T) {
	falsy := `{"nodes":[
	 {"id":"start","data":{"kind":"start","inputVariables":[{"name":"mode","valueType":"string"},{"name":"flag","valueType":"boolean","value":false}]}},
	 {"id":"agg","data":{"kind":"aggregator","aggregatorConfig":{"variables":[["start","flag"]]}}},
	 {"id":"out","data":{"kind":"output"}}
	],"edges":[{"source":"start","target":"agg"},{"source":"agg","target":"out"}]}`
	_, repo, _ := engineWithInput(t, falsy, "kickoff")
	if repo.status("agg") != StatusSucceeded || repo.nodeOutput("agg") != "false" || poolValue(t, repo, "agg.output") != false || repo.run.Status != StatusSucceeded {
		t.Fatalf("falsy pass-through: %s %q %v run %s", repo.status("agg"), repo.nodeOutput("agg"), poolValue(t, repo, "agg.output"), repo.run.Status)
	}

	noMatch := strings.Replace(falsy, `[["start","flag"]]`, `[["start","mode"]]`, 1)
	_, repo, _ = engineWithInput(t, noMatch, "kickoff")
	if repo.status("agg") != StatusFailed || repo.run.Status != StatusFailed {
		t.Fatalf("no match: agg %s run %s", repo.status("agg"), repo.run.Status)
	}
	var detail struct {
		ErrorDetail struct{ Kind, Message string } `json:"error_detail"`
	}
	for _, r := range repo.runs {
		if r.NodeID == "agg" {
			_ = json.Unmarshal(repo.payload[r.ID], &detail)
		}
	}
	if detail.ErrorDetail.Kind != "aggregator_no_match" || !strings.Contains(detail.ErrorDetail.Message, "start.mode") ||
		!strings.Contains(detail.ErrorDetail.Message, "no assigned variable") {
		t.Fatalf("failure detail = %+v", detail.ErrorDetail)
	}
}

func TestConditionOnAnUnsetVariableFailsTheRun(t *testing.T) {
	graph := `{"nodes":[
	 {"id":"start","data":{"kind":"start","inputVariables":[{"name":"mode","valueType":"string"}]}},
	 {"id":"cond","data":{"kind":"condition","cases":[{"id":"x","conditions":[{"variableSelector":["start","mode"],"operator":"equals","value":"x"}]}]}},
	 {"id":"out","data":{"kind":"output"}}
	],"edges":[{"source":"start","target":"cond"},{"source":"cond","sourceHandle":"x","target":"out"}]}`
	_, repo, _ := engineWithInput(t, graph, "kickoff")
	var detail struct {
		ErrorDetail struct{ Kind, Message string } `json:"error_detail"`
	}
	for _, r := range repo.runs {
		if r.NodeID == "cond" {
			_ = json.Unmarshal(repo.payload[r.ID], &detail)
		}
	}
	if repo.run.Status != StatusFailed || detail.ErrorDetail.Kind != "condition_evaluation" || !strings.Contains(detail.ErrorDetail.Message, "start.mode has not been assigned") {
		t.Fatalf("run %s detail %+v", repo.run.Status, detail.ErrorDetail)
	}
}

// An unlabeled edge from a Condition is its else branch; the run ends once nothing on the taken
// path remains, even though the other branch never ran.
func TestUnlabeledConditionEdgeIsTheElseBranch(t *testing.T) {
	graph := `{"nodes":[
	 {"id":"start","data":{"kind":"start"}},
	 {"id":"cond","data":{"kind":"condition","cases":[{"id":"yes","conditions":[{"variableSelector":["start","input"],"operator":"equals","value":"yes"}]}]}},
	 {"id":"hit","data":{"kind":"output","outputs":[{"name":"via","variableSelector":["start","input"]}]}},
	 {"id":"miss","data":{"kind":"output"}}
	],"edges":[{"source":"start","target":"cond"},{"source":"cond","sourceHandle":"yes","target":"hit"},{"source":"cond","target":"miss"}]}`
	_, repo, _ := engineWithInput(t, graph, "no")
	if repo.status("miss") != StatusSucceeded || repo.status("hit") != "" || repo.run.Status != StatusSucceeded {
		t.Fatalf("hit %q miss %q run %s", repo.status("hit"), repo.status("miss"), repo.run.Status)
	}
}

func TestStartRejectsMalformedConditionBranches(t *testing.T) {
	cases := map[string]struct{ cases, edge, code string }{
		"duplicate case": {`[{"id":"x","conditions":[]},{"id":"x","conditions":[]}]`, `{"source":"cond","target":"out"}`, "workflow_run_duplicate_condition_case"},
		"unknown branch": {`[{"id":"x","conditions":[]}]`, `{"source":"cond","sourceHandle":"ghost","target":"out"}`, "workflow_run_unknown_condition_branch"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			graph := `{"nodes":[{"id":"start","data":{"kind":"start"}},{"id":"cond","data":{"kind":"condition","cases":` + c.cases + `}},{"id":"out","data":{"kind":"output"}}],` +
				`"edges":[{"source":"start","target":"cond"},` + c.edge + `]}`
			repo := newMemoryRepo(graph, str("x"))
			err := NewEngine(repo, &recorder{}, nil, time.Now).Start(context.Background(), "run")
			var start *StartError
			if !errors.As(err, &start) || start.Code != c.code || start.NodeID != "cond" {
				t.Fatalf("start = %v, want %s", err, c.code)
			}
			if repo.run.Status != StatusPending {
				t.Fatalf("a refused run left pending changed to %s", repo.run.Status)
			}
		})
	}
}

func TestAgentStructuredOutputReachesTheCondition(t *testing.T) {
	graph := `{"nodes":[
	 {"id":"start","data":{"kind":"start"}},
	 {"id":"review","data":{"kind":"agent","agentConfig":{"prompt":"review","outputContract":{"type":"structured","schema":{"type":"object","properties":{"approved":{"type":"boolean"},"score":{"type":"integer"}},"required":["approved"]}}}}},
	 {"id":"cond","data":{"kind":"condition","cases":[{"id":"ok","conditions":[{"variableSelector":["review","structured_output","approved"],"operator":"is","value":true}]}]}},
	 {"id":"ship","data":{"kind":"output","outputs":[{"name":"score","variableSelector":["review","structured_output","score"]}]}},
	 {"id":"stop","data":{"kind":"output"}}
	],"edges":[{"source":"start","target":"review"},{"source":"review","target":"cond"},{"source":"cond","sourceHandle":"ok","target":"ship"},{"source":"cond","sourceHandle":"else","target":"stop"}]}`
	e, repo, agents := engineWithInput(t, graph, "kickoff")
	answer := `{"approved": true, "score": 92}`
	value, err := ParseStructuredOutput(answer, agents.dispatched[0].Node.Agent().Structured)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.CompleteNode(context.Background(), "run", agents.dispatched[0].NodeRunID, Completion{Output: &answer, Structured: value}); err != nil {
		t.Fatal(err)
	}
	if repo.status("ship") != StatusSucceeded || repo.status("stop") != "" || *repo.output != `{"score":92}` {
		t.Fatalf("ship %q stop %q output %v", repo.status("ship"), repo.status("stop"), repo.output)
	}
	if poolValue(t, repo, "review.output") != answer {
		t.Fatalf("the raw answer is not the node output: %v", poolValue(t, repo, "review.output"))
	}
}
