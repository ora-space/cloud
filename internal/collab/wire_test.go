package collab

import (
	"context"
	"testing"

	"github.com/wanglongan587/cloud/internal/core"
)

// TestWireDevelopmentFixturesInstallsAllPorts pins the single composition point: enabling the
// development fixtures must make every fixture collaboration port (Directory/Context/Dispatcher/
// Assist) available at once — a half-wired store would surface targets the runtime cannot serve.
// Forms is not among them: it is production wiring on NewStore, because the descriptor comes from the
// workflow document rather than from a fixture.
func TestWireDevelopmentFixturesInstallsAllPorts(t *testing.T) {
	store := &core.Store{}
	WireDevelopmentFixtures(store)
	if store.Directory == nil || store.Context == nil || store.Dispatcher == nil || store.Assist == nil {
		t.Fatalf("WireDevelopmentFixtures must install all four fixture collaboration ports")
	}
}

// TestFixtureDirectoryAllNonHumanTargetTypesDiscoverable verifies that the fixture directory serves
// agent + team through the same target discovery interface the @ picker uses, with stable IDs and
// interaction descriptors consistent with the frozen target-type defaults, and never users or
// workflows — the latter are real, and come from the workflows table.
func TestFixtureDirectoryAllNonHumanTargetTypesDiscoverable(t *testing.T) {
	var d core.CollaborationDirectory = FixtureAgentTeamDirectory{}
	targets, err := d.ListTargets(context.Background(), "", "")
	if err != nil {
		t.Fatalf("ListTargets: %v", err)
	}
	// Frozen target-type -> interaction-mode default (Issue collaboration D3): user=mention, agent/team=task (task
	// required), workflow=form. The fixture descriptors must agree with it.
	expected := map[string][2]any{
		"agent": {"task", true},
		"team":  {"task", true},
	}
	byType := map[string]int{}
	for _, s := range targets {
		byType[s.Type]++
		wantMode, wantTask := expected[s.Type][0].(string), expected[s.Type][1].(bool)
		if s.InteractionDescriptor.Mode != wantMode || s.InteractionDescriptor.RequiresTask != wantTask {
			t.Fatalf("descriptor for %s %q disagrees with frozen default (want mode=%s requiresTask=%v, got mode=%s requiresTask=%v)",
				s.Type, s.DisplayName, wantMode, wantTask, s.InteractionDescriptor.Mode, s.InteractionDescriptor.RequiresTask)
		}
	}
	if byType["agent"] != 2 || byType["team"] != 1 {
		t.Fatalf("unexpected fixture target mix (want 2 agent/1 team): %v", byType)
	}
	if byType["user"] != 0 || byType["workflow"] != 0 {
		t.Fatalf("the fixtures own agent/team only: %v", byType)
	}

	// Resolve each stable fixture ID through the same seam.
	for _, s := range targets {
		if _, ok, err := d.ResolveTarget(context.Background(), "", s.Type, s.ID); err != nil || !ok {
			t.Fatalf("ResolveTarget(%s, %s) = ok=%v err=%v", s.Type, s.ID, ok, err)
		}
	}
}

// stubDirectory is a primary that answers one target, to prove FallbackDirectory delegates rather
// than merging blindly.
type stubDirectory struct {
	summary core.CollaborationTargetSummary
}

func (s stubDirectory) ListTargets(context.Context, string, string) ([]core.CollaborationTargetSummary, error) {
	return []core.CollaborationTargetSummary{s.summary}, nil
}

func (s stubDirectory) ResolveTarget(_ context.Context, _, targetType, targetID string) (core.CollaborationTargetSummary, bool, error) {
	if targetType == s.summary.Type && targetID == s.summary.ID {
		return s.summary, true, nil
	}
	return core.CollaborationTargetSummary{}, false, nil
}

// TestFallbackDirectoryComposesPrimaryAndFixtures covers the composition development relies on:
// workflow targets come from the real directory, agent/team from the fixtures, and neither leaks into
// the other's half.
func TestFallbackDirectoryComposesPrimaryAndFixtures(t *testing.T) {
	workflow := core.CollaborationTargetSummary{
		Type: "workflow", ID: "44444444-4444-4444-8444-444444444444", DisplayName: "Review flow",
		InteractionDescriptor: core.InteractionDescriptor{Mode: "form", FormRef: "44444444-4444-4444-8444-444444444444"},
	}
	d := FallbackDirectory{Workflows: stubDirectory{summary: workflow}}

	targets, err := d.ListTargets(context.Background(), "t1", "")
	if err != nil {
		t.Fatalf("ListTargets: %v", err)
	}
	byType := map[string]int{}
	for _, s := range targets {
		byType[s.Type]++
	}
	if byType["workflow"] != 1 || byType["agent"] != 2 || byType["team"] != 1 {
		t.Fatalf("composed target mix wrong: %v", byType)
	}

	// A workflow resolves through the primary; an agent resolves through the fixtures.
	if _, ok, _ := d.ResolveTarget(context.Background(), "t1", "workflow", workflow.ID); !ok {
		t.Fatal("a real workflow target must resolve through the primary")
	}
	if _, ok, _ := d.ResolveTarget(context.Background(), "t1", "agent", BackendAgentID); !ok {
		t.Fatal("an agent target must resolve through the fixtures")
	}
	// An unknown workflow id is not-found: it must not fall through to the fixtures.
	if _, ok, _ := d.ResolveTarget(context.Background(), "t1", "workflow", "55555555-5555-4555-8555-555555555555"); ok {
		t.Fatal("an unknown workflow must not resolve")
	}
}

// TestFallbackDirectoryWithoutPrimary covers a store assembled without a pool: the fixtures still
// answer, and nothing panics on the missing delegate.
func TestFallbackDirectoryWithoutPrimary(t *testing.T) {
	var d FallbackDirectory
	targets, err := d.ListTargets(context.Background(), "", "")
	if err != nil || len(targets) != 3 {
		t.Fatalf("fixture-only directory = %v, err=%v", targets, err)
	}
	if _, ok, _ := d.ResolveTarget(context.Background(), "", "workflow", "44444444-4444-4444-8444-444444444444"); ok {
		t.Fatal("without a primary there are no workflow targets")
	}
}
