//go:build execution_integration && linux

package scheduler_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wanglongan587/cloud/v2-server/internal/agentruntime"
	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/executionruntime"
	"github.com/wanglongan587/cloud/v2-server/internal/nodeservices"
	"github.com/wanglongan587/cloud/v2-server/internal/projectrepos"
	"github.com/wanglongan587/cloud/v2-server/internal/scheduler"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/store/storetest"
)

type realSchedulerFixture struct {
	t        *testing.T
	ctx      context.Context
	db       *store.Store
	config   *pgxpool.Config
	scope    execution.Scope
	root     string
	mode     string
	provider nodeservices.Provider
	project  string
	svc      *executionruntime.Service
}

func newRealSchedulerFixture(t *testing.T, mode string, provider nodeservices.Provider) *realSchedulerFixture {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Fatal("runtime fault acceptance requires TEST_DATABASE_URL")
	}
	if mode == "agent" && os.Getenv("V2_NODE_ASSETS_DIR") == "" {
		t.Fatal("Agent acceptance requires prepared V2_NODE_ASSETS_DIR")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	t.Cleanup(cancel)
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("scheduler_fault_%s_%d", mode, time.Now().UnixNano())
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	db, err := store.OpenConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp(home, "s847-")
	if err != nil {
		t.Fatal(err)
	}
	f := &realSchedulerFixture{t: t, ctx: ctx, db: db, config: config, root: root, mode: mode, provider: provider}
	t.Cleanup(func() {
		f.closeService()
		db.Close()
		if _, err := admin.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE"); err != nil {
			t.Errorf("drop schema: %v", err)
		}
		if err := admin.Close(context.Background()); err != nil {
			t.Errorf("close admin connection: %v", err)
		}
		if err := os.RemoveAll(root); err != nil {
			t.Errorf("remove test root: %v", err)
		}
	})
	token := storetest.SignIn(t, db, "schedule-fault-"+mode, "Owner", store.RoleOwner)
	user, err := db.SessionUser(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	tenants, err := db.Tenants(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	f.scope = execution.Scope{TenantID: tenants[0].ID, UserID: user.ID}
	if mode == "agent" {
		repos, err := projectrepos.New(db, projectrepos.Options{Root: filepath.Join(root, "repos")})
		if err != nil {
			t.Fatal(err)
		}
		project, err := repos.SeedDemo(ctx, f.scope)
		if err != nil {
			t.Fatal(err)
		}
		f.project = project.ID
	}
	f.startService()
	return f
}

func (f *realSchedulerFixture) startService() {
	f.t.Helper()
	models, err := executionruntime.ModelsFromEnvironment(func(string) string { return "" })
	if err != nil {
		f.t.Fatal(err)
	}
	if f.provider != nil {
		choice := models["fixture"]
		choice.Binding.Provider = f.provider
		models["fixture"] = choice
	}
	config := executionruntime.Config{Store: f.db, Models: models}
	if f.mode == "agent" {
		gitPath, err := exec.CommandContext(f.ctx, "git", "--exec-path").Output()
		if err != nil {
			f.t.Fatal(err)
		}
		driver, err := agentruntime.New(agentruntime.Options{
			Root:            filepath.Join(f.root, "agents"),
			AssetsDirectory: os.Getenv("V2_NODE_ASSETS_DIR"),
			GitBackend:      filepath.Join(strings.TrimSpace(string(gitPath)), "git-http-backend"),
			Bindings:        map[string]nodeservices.Binding{"fixture": models["fixture"].Binding},
			ResolveProject: func(ctx context.Context, scope execution.Scope, id string) (agentruntime.Project, error) {
				project, err := f.db.GetProject(ctx, scope, id)
				return agentruntime.Project{MirrorPath: project.MirrorPath, Commit: project.Commit, SnapshotRef: project.SnapshotRef}, err
			},
		})
		if err != nil {
			f.t.Fatal(err)
		}
		config.Agent = driver
	}
	svc, err := executionruntime.New(f.ctx, config)
	if err != nil {
		f.t.Fatal(err)
	}
	f.svc = svc
}

func (f *realSchedulerFixture) closeService() {
	if f.svc == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := f.svc.Close(ctx); err != nil {
		f.t.Errorf("close execution service: %v", err)
	}
	f.svc = nil
}

func (f *realSchedulerFixture) restartService() {
	f.closeService()
	f.startService()
}

func (f *realSchedulerFixture) record(target json.RawMessage, name string) (store.ScheduleConfig, store.Occurrence) {
	f.t.Helper()
	config, err := f.db.CreateScheduleConfig(f.ctx, f.scope.TenantID, f.scope.UserID, store.ScheduleInput{
		Name:      name,
		ProjectID: f.project,
		Target:    target,
		Rule:      json.RawMessage(`{"kind":"hours","hours":1,"timezone":"UTC"}`),
	})
	if err != nil {
		f.t.Fatal(err)
	}
	at := config.CreatedAt.Add(time.Second)
	occurrence, err := f.db.RecordOccurrence(f.ctx, f.scope.TenantID, f.scope.UserID, config.ID, store.Trigger{
		Kind: "manual", Key: name, At: at, Version: config.Version,
	}, at)
	if err != nil {
		f.t.Fatal(err)
	}
	return config, occurrence
}

func (f *realSchedulerFixture) occurrence(id string) store.Occurrence {
	f.t.Helper()
	occurrence, err := f.db.GetOccurrence(f.ctx, f.scope.TenantID, f.scope.UserID, id)
	if err != nil {
		f.t.Fatal(err)
	}
	return occurrence
}

func (f *realSchedulerFixture) executionCounts() (int, int) {
	f.t.Helper()
	conn, err := pgx.ConnectConfig(f.ctx, f.config.ConnConfig.Copy())
	if err != nil {
		f.t.Fatal(err)
	}
	defer conn.Close(context.Background())
	var sessions, turns int
	err = conn.QueryRow(f.ctx, `SELECT (SELECT count(*) FROM v2_execution_sessions), (SELECT count(*) FROM v2_execution_turns)`).Scan(&sessions, &turns)
	if err != nil {
		f.t.Fatal(err)
	}
	return sessions, turns
}

func (f *realSchedulerFixture) waitForTurn(caller execution.Caller) execution.Turn {
	f.t.Helper()
	deadline := time.NewTimer(2 * time.Minute)
	defer deadline.Stop()
	poll := time.NewTicker(25 * time.Millisecond)
	defer poll.Stop()
	for {
		turn, err := f.svc.ReadAttempt(f.ctx, f.scope, caller)
		if err == nil && turn.Result != nil {
			return turn
		}
		if err != nil && !errors.Is(err, execution.ErrNotFound) {
			f.t.Fatal(err)
		}
		select {
		case <-poll.C:
		case <-deadline.C:
			f.t.Fatal("timed out waiting for the real execution result")
		case <-f.ctx.Done():
			f.t.Fatal(f.ctx.Err())
		}
	}
}

func scheduleCaller(occurrence store.Occurrence) execution.Caller {
	return execution.Caller{Kind: execution.CallerSchedule, ID: occurrence.ID, AttemptID: occurrence.ID}
}

type droppedAdmissionResponse struct {
	execution.Service
	starts atomic.Int32
}

func (d *droppedAdmissionResponse) Start(ctx context.Context, scope execution.Scope, request execution.StartRequest) (execution.Ref, error) {
	ref, err := d.Service.Start(ctx, scope, request)
	if err != nil {
		return ref, err
	}
	d.starts.Add(1)
	return execution.Ref{}, context.DeadlineExceeded
}

func (d *droppedAdmissionResponse) StartModel(ctx context.Context, scope execution.Scope, request execution.ModelStartRequest) (execution.Ref, error) {
	ref, err := d.Service.StartModel(ctx, scope, request)
	if err != nil {
		return ref, err
	}
	d.starts.Add(1)
	return execution.Ref{}, context.DeadlineExceeded
}

func testAcceptedResponseLoss(t *testing.T, mode string) {
	t.Helper()
	f := newRealSchedulerFixture(t, mode, nil)
	target := json.RawMessage(`{"kind":"chat","model":"fixture","prompt":"accepted response loss"}`)
	if mode == "agent" {
		target = json.RawMessage(`{"kind":"chat","executorId":"opencode:fixture","prompt":"Read README and report the accepted response-loss fixture."}`)
	}
	_, occurrence := f.record(target, "lost-response")
	executor := &droppedAdmissionResponse{Service: f.svc}
	adapter, err := scheduler.NewConversationAdapter(f.db, executor)
	if err != nil {
		t.Fatal(err)
	}
	if err = adapter.Dispatch(f.ctx, f.scope, occurrence); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("injected admission response loss: %v", err)
	}
	if starts := executor.starts.Load(); starts != 1 {
		t.Fatalf("admission calls = %d, want 1", starts)
	}
	pending := f.occurrence(occurrence.ID)
	if pending.Status != "reconciling" || pending.Reason != "lookup_required" || pending.Released || pending.ExecutionRef != "" {
		t.Fatalf("response-loss occurrence = %+v", pending)
	}
	caller := scheduleCaller(occurrence)
	turn := f.waitForTurn(caller)
	if turn.Ref.SessionID == "" || turn.Ref.TurnID == "" || !sameRuntimeValue(turn.Caller, caller) {
		t.Fatalf("real accepted attempt = %+v", turn)
	}
	if turn.Result == nil || turn.Result.Status != execution.StatusCompleted || !turn.Result.AnswerComplete || turn.Result.Answer == "" {
		t.Fatalf("real accepted result = %+v", turn.Result)
	}
	if mode == "agent" && turn.Result.Answer != nodeservices.FixtureAnswer {
		t.Fatalf("Agent did not complete fixture tool round: %q", turn.Result.Answer)
	}
	if sessions, turns := f.executionCounts(); sessions != 1 || turns != 1 {
		t.Fatalf("before reconciliation: sessions=%d turns=%d", sessions, turns)
	}
	f.restartService()
	adapter, err = scheduler.NewConversationAdapter(f.db, f.svc)
	if err != nil {
		t.Fatal(err)
	}
	if err = adapter.Reconcile(f.ctx, f.scope, pending); err != nil {
		t.Fatal(err)
	}
	finished := f.occurrence(occurrence.ID)
	encoded, _ := json.Marshal(turn.Ref)
	if finished.Status != "succeeded" || !finished.Released || finished.Reason != "" || finished.ExecutionRef != string(encoded) {
		t.Fatalf("reconciled occurrence = %+v", finished)
	}
	reconciled, err := f.svc.ReadAttempt(f.ctx, f.scope, caller)
	if err != nil || !sameRuntimeValue(reconciled, turn) {
		t.Fatalf("original attempt changed during reconciliation: %+v %v", reconciled, err)
	}
	session, err := f.svc.ReadSession(f.ctx, f.scope, turn.Ref.SessionID)
	if err != nil || session.EndedAt == nil {
		t.Fatalf("reconciled session = %+v %v", session, err)
	}
	if mode == "agent" && session.WorkspaceID == "" {
		t.Fatal("Agent schedule has no workspace")
	}
	if sessions, turns := f.executionCounts(); sessions != 1 || turns != 1 {
		t.Fatalf("after reconciliation: sessions=%d turns=%d", sessions, turns)
	}
	if starts := executor.starts.Load(); starts != 1 {
		t.Fatalf("reconciliation re-admitted execution: %d starts", starts)
	}
	t.Logf("%s accepted once, response lost, then reconciled by caller %s with one durable session/turn", mode, caller.AttemptID)
}

func TestScheduledModelAcceptedResponseLossReconciles(t *testing.T) {
	testAcceptedResponseLoss(t, "model")
}

func TestScheduledAgentAcceptedResponseLossReconciles(t *testing.T) {
	testAcceptedResponseLoss(t, "agent")
}

func TestScheduledBusyAdmissionIsSkipped(t *testing.T) {
	gate := &gateFixture{block: true, started: make(chan struct{}, 1)}
	f := newRealSchedulerFixture(t, "model", gate)
	target := json.RawMessage(`{"kind":"chat","model":"fixture","prompt":"busy admission"}`)
	_, occurrence := f.record(target, "busy-admission")
	competing := execution.Caller{Kind: execution.CallerSchedule, ID: occurrence.ID, AttemptID: "competing-attempt"}
	if _, err := f.svc.StartModel(f.ctx, f.scope, execution.ModelStartRequest{
		Caller: competing, ModelID: "fixture", Prompt: execution.TextPrompt("hold the workspace"),
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-gate.started:
	case <-f.ctx.Done():
		t.Fatal(f.ctx.Err())
	}
	adapter, err := scheduler.NewConversationAdapter(f.db, f.svc)
	if err != nil {
		t.Fatal(err)
	}
	if err = adapter.Dispatch(f.ctx, f.scope, occurrence); !errors.Is(err, execution.ErrWorkspaceBusy) {
		t.Fatalf("real Busy admission: %v", err)
	}
	skipped := f.occurrence(occurrence.ID)
	if skipped.Status != "skipped" || skipped.Reason != "execution_busy" || !skipped.Released || skipped.ExecutionRef != "" {
		t.Fatalf("Busy occurrence = %+v", skipped)
	}
	if _, err = f.svc.ReadAttempt(f.ctx, f.scope, scheduleCaller(occurrence)); !errors.Is(err, execution.ErrNotFound) {
		t.Fatalf("Busy occurrence received an execution attempt: %v", err)
	}
	if sessions, turns := f.executionCounts(); sessions != 1 || turns != 1 {
		t.Fatalf("Busy created a replacement execution: sessions=%d turns=%d", sessions, turns)
	}
	t.Logf("real Busy rejected admission once; occurrence is skipped/execution_busy with no execution attempt")
}

func TestScheduledAgentTasksWithSameProjectKeepDistinctWorkspaces(t *testing.T) {
	f := newRealSchedulerFixture(t, "agent", nil)
	target := json.RawMessage(`{"kind":"chat","executorId":"opencode:fixture","prompt":"Read README and keep this scheduled task isolated."}`)
	_, first := f.record(target, "isolated-first")
	_, second := f.record(target, "isolated-second")
	adapter, err := scheduler.NewConversationAdapter(f.db, f.svc)
	if err != nil {
		t.Fatal(err)
	}
	for _, occurrence := range []store.Occurrence{first, second} {
		if err = adapter.Dispatch(f.ctx, f.scope, occurrence); err != nil {
			t.Fatalf("same-project task %s: %v", occurrence.Key, err)
		}
	}
	firstTurn := f.waitForTurn(scheduleCaller(first))
	secondTurn := f.waitForTurn(scheduleCaller(second))
	if firstTurn.Ref.SessionID == secondTurn.Ref.SessionID {
		t.Fatal("same-project schedule tasks shared a session")
	}
	firstSession, err := f.svc.ReadSession(f.ctx, f.scope, firstTurn.Ref.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	secondSession, err := f.svc.ReadSession(f.ctx, f.scope, secondTurn.Ref.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if firstSession.WorkspaceID == "" || secondSession.WorkspaceID == "" || firstSession.WorkspaceID == secondSession.WorkspaceID {
		t.Fatalf("same-project workspaces: %q / %q", firstSession.WorkspaceID, secondSession.WorkspaceID)
	}
	for index, occurrence := range []store.Occurrence{first, second} {
		turn := []execution.Turn{firstTurn, secondTurn}[index]
		if turn.Result == nil || turn.Result.Status != execution.StatusCompleted || !turn.Result.AnswerComplete || turn.Result.Answer != nodeservices.FixtureAnswer {
			t.Fatalf("same-project task %s result = %+v", occurrence.Key, turn.Result)
		}
		if err = adapter.Reconcile(f.ctx, f.scope, occurrence); err != nil {
			t.Fatal(err)
		}
		finished := f.occurrence(occurrence.ID)
		if finished.Status != "succeeded" || !finished.Released {
			t.Fatalf("same-project task %s = %+v", occurrence.Key, finished)
		}
	}
	if sessions, turns := f.executionCounts(); sessions != 2 || turns != 2 {
		t.Fatalf("same-project executions: sessions=%d turns=%d", sessions, turns)
	}
	t.Logf("same-project schedule tasks completed in distinct workspaces %q and %q", firstSession.WorkspaceID, secondSession.WorkspaceID)
}
