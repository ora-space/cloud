package workflow

import (
	"context"
	"errors"
	"testing"
	"time"
)

// The start dialog warns from analysis while start refuses on its own check; both must agree on
// which node is unsupported, or the dialog warns about a run that would start (or stays quiet).
func TestAnalysisUnsupportedNodesMatchStartRefusal(t *testing.T) {
	for name, graph := range map[string]string{
		"reachable tool": `{"nodes":[{"id":"start","data":{"kind":"start"}},{"id":"t","data":{"kind":"tool"}}],
			"edges":[{"source":"start","target":"t"}]}`,
		"reachable prompt behind spare tool": `{"nodes":[{"id":"start","data":{"kind":"start"}},{"id":"spare","data":{"kind":"tool"}},{"id":"p","data":{"kind":"prompt"}}],
			"edges":[{"source":"start","target":"p"}]}`,
		"spare prompt": `{"nodes":[{"id":"start","data":{"kind":"start"}},{"id":"o","data":{"kind":"output"}},{"id":"p","data":{"kind":"prompt"}}],
			"edges":[{"source":"start","target":"o"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			analysis, err := AnalyzeWorkflow(graph)
			if err != nil {
				t.Fatal(err)
			}
			repo := newMemoryRepo(graph, nil)
			startErr := NewEngine(repo, &recorder{}, nil, time.Now).Start(context.Background(), "run")
			var refusal *StartError
			refused := errors.As(startErr, &refusal) && refusal.Code == "workflow_run_unsupported_node_type"
			if refused != (len(analysis.UnsupportedNodes) != 0) {
				t.Fatalf("start error %v, analysis %#v: they disagree about an unsupported node", startErr, analysis.UnsupportedNodes)
			}
			if refused && analysis.UnsupportedNodes[0].NodeID != refusal.NodeID {
				t.Fatalf("start refuses %s, analysis names %#v first", refusal.NodeID, analysis.UnsupportedNodes)
			}
		})
	}
}
