package store_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/store/storetest"
)

func workflowProject(t *testing.T, s *store.Store, user, tenant, id string) store.Project {
	t.Helper()
	project, err := s.CreateProject(context.Background(), execution.Scope{TenantID: tenant, UserID: user}, store.Project{
		ID:            id,
		Name:          "演示项目",
		RepositoryURL: "https://example.invalid/demo.git",
		Branch:        "main",
		Commit:        "0123456789abcdef0123456789abcdef01234567",
		MirrorPath:    "/tmp/" + id,
		SnapshotRef:   "refs/ora/snapshots/" + id,
	})
	if err != nil {
		t.Fatal(err)
	}
	return project
}

func publishedWorkflow(t *testing.T, s *store.Store, tenant, name, version string) (store.Workflow, store.WorkflowSnapshot) {
	t.Helper()
	ctx := context.Background()
	w, _, err := s.CreateWorkflow(ctx, tenant, name, graphA)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := s.PublishWorkflow(ctx, tenant, w.ID, version, nil)
	if err != nil {
		t.Fatal(err)
	}
	return w, snapshot
}

func TestWorkflowRunFreezesPublishedVersion(t *testing.T) {
	s := storetest.Open(t)
	user, tenant := storetest.Login(t, s, "run-owner")
	ctx := context.Background()
	project := workflowProject(t, s, user, tenant, "project-1")
	w, published := publishedWorkflow(t, s, tenant, "运行工作流", "v1")

	input := "检查这次改动"
	run, err := s.CreateWorkflowRun(ctx, tenant, w.ID, "", store.WorkflowRunInput{
		ProjectID: project.ID,
		Input:     &input,
	})
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != store.WorkflowStatusPending || run.SnapshotID != published.ID {
		t.Fatalf("run = %+v", run)
	}
	if run.Name != "运行工作流" || run.ProjectID != project.ID || run.Input == nil || *run.Input != input {
		t.Fatalf("frozen run fields = %+v", run)
	}
	if string(run.State) != `{"current_nodes": []}` {
		t.Fatalf("initial state = %s", run.State)
	}
	root, err := s.WorkflowRunRootScope(ctx, tenant, run.ID)
	if err != nil || root != "root:"+run.ID {
		t.Fatalf("root scope = %q %v", root, err)
	}

	// The definition owner controls which published version a run freezes; the draft never runs.
	other, _, err := s.CreateWorkflow(ctx, tenant, "未发布", graphA)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateWorkflowRun(ctx, tenant, other.ID, "", store.WorkflowRunInput{ProjectID: project.ID}); !errors.Is(err, store.ErrWorkflowNoPublishedSnapshot) {
		t.Fatalf("unpublished workflow = %v", err)
	}
	ownDraft, err := s.WorkflowDraft(ctx, tenant, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateWorkflowRun(ctx, tenant, w.ID, ownDraft.ID, store.WorkflowRunInput{ProjectID: project.ID}); !errors.Is(err, store.ErrWorkflowDraftSnapshot) {
		t.Fatalf("draft snapshot = %v", err)
	}
	if _, err := s.CreateWorkflowRun(ctx, tenant, w.ID, other.ID, store.WorkflowRunInput{ProjectID: project.ID}); !errors.Is(err, store.ErrWorkflowSnapshotNotFound) {
		t.Fatalf("foreign snapshot = %v", err)
	}
	if _, err := s.CreateWorkflowRun(ctx, tenant, w.ID, "", store.WorkflowRunInput{ProjectID: "missing"}); !errors.Is(err, store.ErrProjectNotFound) {
		t.Fatalf("unknown project = %v", err)
	}
	if _, err := s.CreateWorkflowRun(ctx, tenant, "00000000-0000-0000-0000-000000000000", "", store.WorkflowRunInput{ProjectID: project.ID}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown workflow = %v", err)
	}

	// An explicit published snapshot older than the current pointer stays runnable.
	if _, err := s.PublishWorkflow(ctx, tenant, w.ID, "v2", nil); err != nil {
		t.Fatal(err)
	}
	older, err := s.CreateWorkflowRun(ctx, tenant, w.ID, published.ID, store.WorkflowRunInput{
		ProjectID:   project.ID,
		Name:        "  指定旧版本  ",
		TriggerKind: "schedule",
		TriggerRef:  "schedule:config-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if older.SnapshotID != published.ID || older.Name != "  指定旧版本  " {
		t.Fatalf("explicit snapshot run = %+v", older)
	}
}

func TestWorkflowDeletionProtectsRunReferences(t *testing.T) {
	s := storetest.Open(t)
	user, tenant := storetest.Login(t, s, "reference-owner")
	ctx := context.Background()
	project := workflowProject(t, s, user, tenant, "project-3")
	w, v1 := publishedWorkflow(t, s, tenant, "引用保护", "v1")
	if _, err := s.CreateWorkflowRun(ctx, tenant, w.ID, v1.ID, store.WorkflowRunInput{ProjectID: project.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PublishWorkflow(ctx, tenant, w.ID, "v2", nil); err != nil {
		t.Fatal(err)
	}
	// v1 is no longer active but a live run froze it, so history must keep resolving.
	if _, err := s.DeleteWorkflowSnapshot(ctx, tenant, w.ID, "v1"); !errors.Is(err, store.ErrWorkflowSnapshotInUse) {
		t.Fatalf("delete referenced version = %v", err)
	}
	if err := s.DeleteWorkflow(ctx, tenant, w.ID); !errors.Is(err, store.ErrWorkflowActiveRuns) {
		t.Fatalf("delete referenced workflow = %v", err)
	}
	// The active version keeps its own refusal.
	if _, err := s.DeleteWorkflowSnapshot(ctx, tenant, w.ID, "v2"); !errors.Is(err, store.ErrWorkflowActiveVersion) {
		t.Fatalf("delete active version = %v", err)
	}
}

func TestWorkflowRunReadsNodesAndScopes(t *testing.T) {
	s := storetest.Open(t)
	user, tenant := storetest.Login(t, s, "history-owner")
	ctx := context.Background()
	project := workflowProject(t, s, user, tenant, "project-2")
	w, published := publishedWorkflow(t, s, tenant, "历史工作流", "v1")

	first, err := s.CreateWorkflowRun(ctx, tenant, w.ID, "", store.WorkflowRunInput{ProjectID: project.ID})
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.CreateWorkflowRun(ctx, tenant, w.ID, "", store.WorkflowRunInput{ProjectID: project.ID})
	if err != nil {
		t.Fatal(err)
	}

	runs, err := s.WorkflowRuns(ctx, tenant, w.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 || runs[0].ID != first.ID || runs[1].ID != second.ID {
		t.Fatalf("run list order = %+v", runs)
	}
	if runs[0].Version != "v1" || runs[0].ProjectID != project.ID || runs[0].Status != store.WorkflowStatusPending {
		t.Fatalf("summary = %+v", runs[0])
	}
	if byProject, err := s.WorkflowRuns(ctx, tenant, "", project.ID); err != nil || len(byProject) != 2 {
		t.Fatalf("project filter = %+v %v", byProject, err)
	}
	if none, err := s.WorkflowRuns(ctx, tenant, w.ID, "other-project"); err != nil || len(none) != 0 {
		t.Fatalf("unknown project filter = %+v %v", none, err)
	}

	started, err := s.StartWorkflowRun(ctx, tenant, first.ID)
	if err != nil || started.Status != store.WorkflowStatusRunning || started.StartedAt == nil {
		t.Fatalf("start = %+v %v", started, err)
	}
	if _, err := s.StartWorkflowRun(ctx, tenant, first.ID); !errors.Is(err, store.ErrWorkflowRunState) {
		t.Fatalf("double start = %v", err)
	}
	if _, err := s.StartWorkflowRun(ctx, tenant, "00000000-0000-0000-0000-000000000000"); !errors.Is(err, store.ErrWorkflowRunNotFound) {
		t.Fatalf("start unknown run = %v", err)
	}

	node, err := s.CreateWorkflowNodeRun(ctx, tenant, first.ID, store.WorkflowNodeRunInput{
		NodeID: "agent-1", NodeType: "agent",
	})
	if err != nil {
		t.Fatal(err)
	}
	if node.Status != store.WorkflowStatusRunning || node.ScopeID != "root:"+first.ID || node.StartedAt == nil || node.Iteration != nil {
		t.Fatalf("node = %+v", node)
	}
	if _, err := s.CreateWorkflowNodeRun(ctx, tenant, first.ID, store.WorkflowNodeRunInput{NodeID: "agent-1", NodeType: "agent"}); !errors.Is(err, store.ErrWorkflowNodeRunExists) {
		t.Fatalf("duplicate live attempt = %v", err)
	}
	iteration := 2
	round, err := s.CreateWorkflowNodeRun(ctx, tenant, first.ID, store.WorkflowNodeRunInput{
		NodeID: "agent-1", NodeType: "agent", Iteration: &iteration,
	})
	if err != nil {
		t.Fatalf("iteration round = %v", err)
	}
	if round.Iteration == nil || *round.Iteration != 2 {
		t.Fatalf("iteration row = %+v", round)
	}

	nodes, err := s.WorkflowRunNodes(ctx, tenant, first.ID)
	if err != nil || len(nodes) != 2 {
		t.Fatalf("nodes = %+v %v", nodes, err)
	}

	// Parking is the only way into `awaitingInput`; the stored status stays `running`.
	parked, err := s.ParkWorkflowNodeRun(ctx, tenant, first.ID, node.ID)
	if err != nil || parked.Status != store.WorkflowStatusPending {
		t.Fatalf("park = %+v %v", parked, err)
	}
	if _, err := s.ParkWorkflowNodeRun(ctx, tenant, first.ID, node.ID); !errors.Is(err, store.ErrWorkflowNodeRunState) {
		t.Fatalf("double park = %v", err)
	}
	if _, err := s.ParkWorkflowNodeRun(ctx, tenant, first.ID, "00000000-0000-0000-0000-000000000000"); !errors.Is(err, store.ErrWorkflowNodeRunNotFound) {
		t.Fatalf("park missing node = %v", err)
	}
	detail, err := s.WorkflowRun(ctx, tenant, first.ID)
	if err != nil || detail.Status != "awaitingInput" {
		t.Fatalf("derived run status = %+v %v", detail, err)
	}
	// A pending node in a different run does not make this one await input.
	otherRun, err := s.WorkflowRun(ctx, tenant, second.ID)
	if err != nil || otherRun.Status != store.WorkflowStatusPending {
		t.Fatalf("unrelated run status = %+v %v", otherRun, err)
	}

	scopes, err := s.WorkflowRunScopes(ctx, tenant, first.ID)
	if err != nil || len(scopes) != 0 {
		t.Fatalf("root scope must not be listed = %+v %v", scopes, err)
	}

	unknown := "00000000-0000-0000-0000-000000000000"
	if _, err := s.WorkflowRun(ctx, unknown, first.ID); !errors.Is(err, store.ErrWorkflowRunNotFound) {
		t.Fatalf("cross-tenant run read = %v", err)
	}
	if _, err := s.CreateWorkflowNodeRun(ctx, tenant, unknown, store.WorkflowNodeRunInput{NodeID: "x", NodeType: "agent"}); !errors.Is(err, store.ErrWorkflowRunNotFound) {
		t.Fatalf("node on unknown run = %v", err)
	}
	if nodes, err := s.WorkflowRunNodes(ctx, unknown, first.ID); err != nil || len(nodes) != 0 {
		t.Fatalf("cross-tenant nodes = %+v %v", nodes, err)
	}
	if run, err := s.WorkflowRun(ctx, tenant, first.ID); err != nil || run.SnapshotID != published.ID {
		t.Fatalf("run snapshot = %+v %v", run, err)
	}
}

func TestWorkflowNodeDispatchCreatesOneLiveAttempt(t *testing.T) {
	s := storetest.Open(t)
	user, tenant := storetest.Login(t, s, "dispatch-owner")
	ctx := context.Background()
	project := workflowProject(t, s, user, tenant, "project-dispatch")
	w, _ := publishedWorkflow(t, s, tenant, "派发", "v1")
	run, err := s.CreateWorkflowRun(ctx, tenant, w.ID, "", store.WorkflowRunInput{ProjectID: project.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartWorkflowRun(ctx, tenant, run.ID); err != nil {
		t.Fatal(err)
	}

	const dispatchers = 8
	errs := make([]error, dispatchers)
	var wg sync.WaitGroup
	for i := range dispatchers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = s.CreateWorkflowNodeRun(ctx, tenant, run.ID, store.WorkflowNodeRunInput{
				NodeID: "agent-1", NodeType: "agent",
			})
		}(i)
	}
	wg.Wait()
	created := 0
	for _, err := range errs {
		switch {
		case err == nil:
			created++
		case errors.Is(err, store.ErrWorkflowNodeRunExists):
		default:
			t.Fatalf("unexpected dispatch error: %v", err)
		}
	}
	if created != 1 {
		t.Fatalf("%d concurrent dispatches created the attempt", created)
	}
	nodes, err := s.WorkflowRunNodes(ctx, tenant, run.ID)
	if err != nil || len(nodes) != 1 {
		t.Fatalf("live attempts = %+v %v", nodes, err)
	}
}

// TestWorkflowRunCreationAndDeletionSerialize proves the definition row lock keeps a run and its
// frozen snapshot from being orphaned: either the run exists and deletion is refused, or deletion
// wins and creation finds no definition.
func TestWorkflowRunCreationAndDeletionSerialize(t *testing.T) {
	s := storetest.Open(t)
	user, tenant := storetest.Login(t, s, "serialize-owner")
	project := workflowProject(t, s, user, tenant, "project-serialize")
	for i := range 6 {
		ctx := context.Background()
		w, _ := publishedWorkflow(t, s, tenant, "串行"+string(rune('a'+i)), "v1")
		var createErr, deleteErr error
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, createErr = s.CreateWorkflowRun(ctx, tenant, w.ID, "", store.WorkflowRunInput{ProjectID: project.ID})
		}()
		go func() {
			defer wg.Done()
			deleteErr = s.DeleteWorkflow(ctx, tenant, w.ID)
		}()
		wg.Wait()

		switch {
		case createErr == nil && deleteErr == nil:
			t.Fatalf("round %d: deleted a definition whose run was created", i)
		case createErr == nil && errors.Is(deleteErr, store.ErrWorkflowActiveRuns):
		case errors.Is(createErr, store.ErrNotFound) && deleteErr == nil:
		default:
			t.Fatalf("round %d: create=%v delete=%v", i, createErr, deleteErr)
		}
	}
}
