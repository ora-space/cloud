package workflowhost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/server"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/workflow"
)

// reviewBranchesWithRoleOnBothBranches puts a role on the branch that this run will not take:
// the structured review approves, so "ship" runs and "fix" never does. Desktop checks roles over
// the whole graph at start, and so does v2.
func reviewBranchesWithRoles(roles func(ship, fix string) string) string {
	graph := strings.Replace(reviewBranches, `"prompt":"发布"`, `"prompt":"发布","roleId":"`+roles("ship", "fix")+`"`, 1)
	return strings.Replace(graph, `"prompt":"返工"`, `"prompt":"返工","roleId":"`+"missing-archivist"+`"`, 1)
}

func TestStartRefusesARoleMissingFromAnyNodeOfTheGraph(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	runID := f.run(t, reviewBranchesWithRoles(func(string, string) string { return "reviewer" }), str("支付重构"))

	_, err := f.host.StartRun(ctx, f.scope, runID)
	if !errors.Is(err, server.ErrWorkflowRoleNotFound) {
		t.Fatalf("start error = %v, want a missing role", err)
	}
	if !strings.Contains(err.Error(), "missing-archivist") {
		t.Fatalf("start error does not name the missing role: %v", err)
	}
	f.exec.mu.Lock()
	started := len(f.exec.started)
	f.exec.mu.Unlock()
	if started != 0 {
		t.Fatalf("started %d Agents despite the missing role", started)
	}
	run, err := f.s.WorkflowRun(ctx, f.scope.TenantID, runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != store.WorkflowStatusPending {
		t.Fatalf("run status = %s, want it left pending", run.Status)
	}

	// Creating the role makes the same pending run startable, as desktop's dependency behaves.
	if _, err := f.s.CreateRole(ctx, f.scope.TenantID, f.scope.UserID, store.RoleInput{Name: "missing-archivist"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.host.StartRun(ctx, f.scope, runID); err != nil {
		t.Fatalf("start after creating the role = %v", err)
	}
	f.exec.finish(f.exec.awaitStart(t), completed(`{"approved":true}`))
	f.exec.finish(f.exec.awaitStart(t), completed("已发布"))
	f.await(t, runID, terminal)
}

func TestRoleDeletedAfterStartFailsTheNodeResumably(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	writer, err := f.s.CreateRole(ctx, f.scope.TenantID, f.scope.UserID, store.RoleInput{
		Name: "writer", Description: "写作", Content: "# 技术写作",
	})
	if err != nil {
		t.Fatal(err)
	}
	graph := strings.Replace(twoAgents, `"prompt":"审阅这段初稿：{{#a.output#}}"`,
		`"prompt":"审阅这段初稿：{{#a.output#}}","roleId":"writer"`, 1)
	runID := f.run(t, graph, str("支付重构"))
	if _, err := f.host.StartRun(ctx, f.scope, runID); err != nil {
		t.Fatal(err)
	}
	a := f.exec.awaitStart(t)
	// The catalog is read again when the node starts, not reused from the start check.
	if err := f.s.DeleteRole(ctx, f.scope.TenantID, writer.ID); err != nil {
		t.Fatal(err)
	}
	f.exec.finish(a, completed("初稿：先拆网关"))

	run, nodes := f.await(t, runID, terminal)
	if run.Status != store.WorkflowStatusFailed {
		t.Fatalf("run = %+v, want failed", run)
	}
	b := node(nodes, "b")
	if kind := errorKind(t, b); kind != "workflow_role_not_found" {
		t.Fatalf("node b failure = %s", kind)
	}
	var detail struct {
		ErrorDetail struct {
			Message       string `json:"message"`
			Resumable     bool   `json:"resumable"`
			AutoRetryable bool   `json:"auto_retryable"`
		} `json:"error_detail"`
	}
	if err := json.Unmarshal(b.Payload, &detail); err != nil {
		t.Fatal(err)
	}
	if !detail.ErrorDetail.Resumable || detail.ErrorDetail.AutoRetryable {
		t.Fatalf("detail = %+v", detail.ErrorDetail)
	}
	if !strings.Contains(detail.ErrorDetail.Message, "writer") {
		t.Fatalf("detail message = %q", detail.ErrorDetail.Message)
	}
	f.exec.mu.Lock()
	started := len(f.exec.started)
	f.exec.mu.Unlock()
	if started != 1 {
		t.Fatalf("sessions started = %d, want only the first node", started)
	}
}

func TestRoleContentIsOptionalAndCursorFreeWhenBlank(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.s.CreateRole(ctx, f.scope.TenantID, f.scope.UserID, store.RoleInput{
		Name: "blank", Description: "空角色", Content: " \n\t\n",
	}); err != nil {
		t.Fatal(err)
	}
	graph := strings.Replace(twoAgents, `"roleId":"reviewer"`, `"roleId":"blank"`, 1)
	runID := f.run(t, graph, str("输入"))
	if _, err := f.host.StartRun(ctx, f.scope, runID); err != nil {
		t.Fatal(err)
	}
	a := f.exec.awaitStart(t)
	if text := promptText(t, f.exec.request(a).Prompt); strings.Contains(text, "<system_instructions>") {
		t.Fatalf("blank role content still sent a block:\n%s", text)
	}
	f.exec.finish(a, completed("初稿"))
	f.exec.finish(f.exec.awaitStart(t), completed("可以合并"))
	f.await(t, runID, terminal)
}

func TestRoleReferenceResolvesCaseInsensitivelyAndByID(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	role, err := f.s.CreateRole(ctx, f.scope.TenantID, f.scope.UserID, store.RoleInput{
		Name: "Researcher", Description: "调研", Content: "# 调研",
	})
	if err != nil {
		t.Fatal(err)
	}
	for name, reference := range map[string]string{"lowercase name": "researcher", "id fallback": role.ID} {
		t.Run(name, func(t *testing.T) {
			graph := strings.Replace(twoAgents, `"roleId":"reviewer"`, `"roleId":"`+reference+`"`, 1)
			runID := f.run(t, graph, str("输入"))
			if _, err := f.host.StartRun(ctx, f.scope, runID); err != nil {
				t.Fatalf("start = %v", err)
			}
			a := f.exec.awaitStart(t)
			text := promptText(t, f.exec.request(a).Prompt)
			want := fmt.Sprintf("<system_instructions>\n在整个步骤中，请遵守以下角色定义，并将其视为对推理、行动和交付表达方式的约束。\n角色: %s\n\n# 调研\n</system_instructions>", reference)
			if !strings.Contains(text, want) {
				t.Fatalf("prompt lacks %q:\n%s", want, text)
			}
			f.exec.finish(a, completed("初稿"))
			f.exec.finish(f.exec.awaitStart(t), completed("可以合并"))
			f.await(t, runID, terminal)
		})
	}
}

// TestInteractiveRoleLookupDatabaseErrorIsNotAutoRetryable checks the Interactive conversion and
// that an interactive node whose role lookup hits a database error fails once, as `repository`
// (see TestDependencyLookupDatabaseErrorIsReportedAsRepository for the kind's contract): no
// session starts, no retry is scheduled and no replacement attempt is written.
func TestInteractiveRoleLookupDatabaseErrorIsNotAutoRetryable(t *testing.T) {
	for _, kind := range []workflow.FailureKind{workflow.FailureWorkflowRoleNotFound, workflow.FailureMissingAgentConfig} {
		interactiveFailure := (workflow.Failure{Kind: kind, Message: "role lookup failed"}).Interactive()
		detail := interactiveFailure.Detail(1, false, time.UnixMilli(1_700_000_000_000))
		if interactiveFailure.Kind != kind || detail["auto_retryable"] != false {
			t.Fatalf("Interactive(%s) = %+v (detail=%v), want same kind with auto_retryable=false", kind, interactiveFailure, detail)
		}
	}
	sessionConverted := (workflow.Failure{Kind: workflow.FailureSession, Message: "setup failed"}).Interactive()
	if sessionConverted.Kind != workflow.FailureInteractiveSession {
		t.Fatalf("Interactive(session) = %+v, want interactive_session", sessionConverted)
	}

	f, admin := newFixtureWithAdmin(t)
	ctx := context.Background()
	graph := strings.Replace(
		interactiveChain,
		`"prompt":"起草：{{#start.input#}}","interactive":true`,
		`"prompt":"起草：{{#start.input#}}","interactive":true,"roleId":"reviewer","retry":{"enabled":true,"maxRetries":2,"initialDelaySeconds":0}`,
		1,
	)
	runID := f.run(t, graph, str("角色库故障"))

	entered, release := f.holdDispatchOf(t, "a")
	if _, err := f.host.StartRun(ctx, f.scope, runID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("interactive Agent dispatch never started")
	}
	// Break v2_roles inside this test's isolated schema after StartRun's preflight passes so
	// ResolveWorkflowRole hits a database query error inside runAgent without falling back to public.v2_roles.
	if _, err := admin.Exec(ctx, `ALTER TABLE v2_roles RENAME COLUMN content TO broken_content`); err != nil {
		t.Fatal(err)
	}
	release()

	run, nodes := f.await(t, runID, terminal)
	if run.Status != store.WorkflowStatusFailed {
		t.Fatalf("run = %+v, want failed", run)
	}
	a := node(nodes, "a")
	if a.Status != store.WorkflowStatusFailed {
		t.Fatalf("node a = %+v, want failed", a)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(a.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"retry_wait", "auto_retry", "retry_chain"} {
		if _, ok := payload[key]; ok {
			t.Fatalf("interactive role error attempt carries %s: %s", key, a.Payload)
		}
	}
	var detail struct {
		ErrorDetail struct {
			Kind          string `json:"kind"`
			Message       string `json:"message"`
			Resumable     bool   `json:"resumable"`
			AutoRetryable bool   `json:"auto_retryable"`
		} `json:"error_detail"`
	}
	if err := json.Unmarshal(a.Payload, &detail); err != nil {
		t.Fatal(err)
	}
	if detail.ErrorDetail.Kind != string(workflow.FailureRepository) || !detail.ErrorDetail.Resumable || detail.ErrorDetail.AutoRetryable {
		t.Fatalf("error_detail = %+v, want repository resumable=true auto_retryable=false", detail.ErrorDetail)
	}
	if detail.ErrorDetail.Message != "could not resolve the selected role" {
		t.Fatalf("error_detail.message = %q, want the exact generic message", detail.ErrorDetail.Message)
	}
	history, err := f.s.WorkflowRunFailedAttempts(ctx, f.scope.TenantID, runID)
	if err != nil || len(history) != 0 {
		t.Fatalf("failed history = %+v %v, want no auto-retry replacements", history, err)
	}
	f.exec.mu.Lock()
	started := len(f.exec.started)
	f.exec.mu.Unlock()
	if started != 0 {
		t.Fatalf("started = %d, want no session started", started)
	}
}
