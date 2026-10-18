package integration

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/wanglongan587/cloud/internal/collab"
	"github.com/wanglongan587/cloud/internal/core"
)

// TestCollaborationTargetList covers the read-only Issues-facing target projection: humans reused
// from members, fixture agent/team and database-backed workflow from the directory,
// descriptor-driven modes, and ?q=.
func TestCollaborationTargetList(t *testing.T) {
	f := setup(t)
	// The workflow target is a real row now, so it has to exist before the catalog is read.
	wid := reviewWorkflow(t, f)
	items := issueItems(f.call("GET", f.path("/collaboration/targets"), nil, "", 200))

	byName := map[string]core.Object{}
	for _, it := range items {
		byName[it.S("displayName")] = it
	}
	// Human target derived from the active member (not the directory).
	alice, ok := byName["Alice"]
	if !ok || alice.S("type") != "user" || alice.S("id") != f.uid {
		t.Fatalf("human target missing/wrong: %v", alice)
	}
	if alice.O("interactionDescriptor").S("mode") != "mention" || alice.O("interactionDescriptor").B("requiresTask") {
		t.Fatalf("human descriptor wrong: %v", alice)
	}
	// Fixture agent/team and the database-backed workflow, with the frozen modes.
	agent, ok := byName["Backend Agent"]
	if !ok || agent.S("type") != "agent" || agent.S("id") != collab.BackendAgentID {
		t.Fatalf("agent fixture missing/wrong: %v", agent)
	}
	if agent.O("interactionDescriptor").S("mode") != "task" || !agent.O("interactionDescriptor").B("requiresTask") {
		t.Fatalf("agent descriptor wrong: %v", agent)
	}
	if team, ok := byName["Platform Team"]; !ok || team.S("type") != "team" || team.O("interactionDescriptor").S("mode") != "task" {
		t.Fatalf("team fixture wrong: %v", team)
	}
	// The retired fixture is replaced by the workflow this test created: the same display name, but
	// the id is the row's own and the descriptor is projected from its Start variables.
	if wf, ok := byName["Security Review Workflow"]; !ok || wf.S("type") != "workflow" || wf.S("id") != wid || wf.O("interactionDescriptor").S("mode") != "form" {
		t.Fatalf("workflow target wrong: %v", wf)
	}

	// q narrows the catalog (fixture) and the human members.
	backend := issueItems(f.call("GET", f.path("/collaboration/targets")+"?q=Backend", nil, "", 200))
	if len(backend) != 1 || backend[0].S("displayName") != "Backend Agent" {
		t.Fatalf("catalog q filter wrong: %v", backend)
	}
	aliceHit := issueItems(f.call("GET", f.path("/collaboration/targets")+"?q=Alice", nil, "", 200))
	if len(aliceHit) != 1 || aliceHit[0].S("id") != f.uid {
		t.Fatalf("human q filter wrong: %v", aliceHit)
	}
}

// TestHumanMentionPersistsTypedTarget covers Mention Mode: a typed target is persisted as an
// interaction with no IssueRun, no executor activity, and the comment body is unchanged.
func TestHumanMentionPersistsTypedTarget(t *testing.T) {
	f := setup(t)
	iss := f.call("POST", f.path("/issues"), core.Object{"title": "Mention"}, "m-issue", 200).O("resource")

	comment := f.call("POST", f.path("/issues/"+iss.S("id")+"/comments"), core.Object{
		"body":    "please look at this",
		"targets": []any{core.Object{"type": "user", "id": f.uid}},
	}, "m-1", 200).O("resource")

	// No run for a mention.
	if n := f.scalar("SELECT count(*) FROM issue_runs WHERE issue_id=$1", iss.S("id")); n != 0 {
		t.Fatalf("mention produced a run")
	}
	// One interaction in mention mode, linked to the comment, no run id.
	ints := issueItems(f.call("GET", f.path("/issues/"+iss.S("id")+"/interactions"), nil, "", 200))
	if len(ints) != 1 {
		t.Fatalf("mention interaction not persisted: %v", ints)
	}
	it := ints[0]
	if it.S("commentId") != comment.S("id") || it.S("targetType") != "user" || it.S("targetId") != f.uid || it.S("mode") != "mention" || it["runId"] != nil {
		t.Fatalf("mention interaction wrong: %v", it)
	}

	// The timeline still carries the original comment (and nothing else).
	tl := issueItems(f.call("GET", f.path("/issues/"+iss.S("id")+"/timeline"), nil, "", 200))
	if len(tl) != 1 || tl[0].S("kind") != "comment" || tl[0].S("body") != "please look at this" {
		t.Fatalf("mention timeline wrong: %v", tl)
	}
}

// TestAgentTaskRunsThroughMockExecution covers Agent Task Mode end-to-end: enqueue, post-commit
// dispatched/running/completed, the fixed agent reply comment, and the run.enqueued/started/completed
// activities ordered by shared seq.
func TestAgentTaskRunsThroughMockExecution(t *testing.T) {
	f := setup(t)
	iss := f.call("POST", f.path("/issues"), core.Object{"title": "Agent task"}, "at-issue", 200).O("resource")

	comment := f.call("POST", f.path("/issues/"+iss.S("id")+"/comments"), core.Object{
		"body":    "please implement this",
		"targets": []any{core.Object{"type": "agent", "id": collab.BackendAgentID, "task": "implement feature X"}},
	}, "at-1", 200).O("resource")

	// The mock executes synchronously post-commit: the run has completed.
	runs := issueItems(f.call("GET", f.path("/issues/"+iss.S("id")+"/runs"), nil, "", 200))
	if len(runs) != 1 {
		t.Fatalf("task run missing: %v", runs)
	}
	run := runs[0]
	if run.S("status") != "completed" || run.S("executorType") != "agent" || run.S("executorId") != collab.BackendAgentID {
		t.Fatalf("run not completed or wrong ref: %v", run)
	}
	if run.S("triggerEvidenceKind") != "comment" || run.S("triggerEvidenceRefId") != comment.S("id") {
		t.Fatalf("run provenance wrong: %v", run)
	}
	if run.S("externalExecutionId") == "" {
		t.Fatalf("run was not dispatched with an external id: %v", run)
	}

	// The interaction links the initial run and snapshots only the task (not the comment body).
	ints := issueItems(f.call("GET", f.path("/issues/"+iss.S("id")+"/interactions"), nil, "", 200))
	if len(ints) != 1 || ints[0].S("runId") != run.S("id") || ints[0].S("mode") != "task" || ints[0].S("task") != "implement feature X" {
		t.Fatalf("task interaction wrong: %v", ints)
	}

	// Timeline: comment (seq 1), run.enqueued (2), run.started (3), reply comment (4), run.completed (5).
	tl := issueItems(f.call("GET", f.path("/issues/"+iss.S("id")+"/timeline"), nil, "", 200))
	if len(tl) != 5 {
		t.Fatalf("timeline length wrong: %v", tl)
	}
	if tl[0].S("kind") != "comment" || tl[0].S("body") != "please implement this" {
		t.Fatalf("timeline[0] wrong: %v", tl[0])
	}
	if tl[1].S("kind") != "activity" || tl[1].S("action") != "run.enqueued" {
		t.Fatalf("timeline[1] wrong: %v", tl[1])
	}
	if tl[2].S("kind") != "activity" || tl[2].S("action") != "run.started" {
		t.Fatalf("timeline[2] wrong: %v", tl[2])
	}
	reply := tl[3]
	if reply.S("kind") != "comment" || reply.S("authorType") != "agent" || reply.S("authorId") != collab.BackendAgentID {
		t.Fatalf("reply comment wrong: %v", reply)
	}
	if reply.S("body") != "Demo response: I received the task and the supplied Issue context." {
		t.Fatalf("reply body wrong: %v", reply)
	}
	if tl[4].S("kind") != "activity" || tl[4].S("action") != "run.completed" {
		t.Fatalf("timeline[4] wrong: %v", tl[4])
	}

	// Context snapshot is deterministic and bounded.
	input := run.O("input")
	if input.S("task") != "implement feature X" || input.O("target").S("type") != "agent" {
		t.Fatalf("input snapshot wrong: %v", input)
	}
}

// TestTeamTaskContents differ from agent: the team reply text and team run reference.
func TestTeamTaskRunsThroughMockExecution(t *testing.T) {
	f := setup(t)
	iss := f.call("POST", f.path("/issues"), core.Object{"title": "Team task"}, "tt-issue", 200).O("resource")

	f.call("POST", f.path("/issues/"+iss.S("id")+"/comments"), core.Object{
		"body":    "please review",
		"targets": []any{core.Object{"type": "team", "id": collab.PlatformTeamID, "task": "review the change"}},
	}, "tt-1", 200)

	runs := issueItems(f.call("GET", f.path("/issues/"+iss.S("id")+"/runs"), nil, "", 200))
	if len(runs) != 1 || runs[0].S("executorType") != "team" || runs[0].S("status") != "completed" {
		t.Fatalf("team run wrong: %v", runs)
	}
	tl := issueItems(f.call("GET", f.path("/issues/"+iss.S("id")+"/timeline"), nil, "", 200))
	if len(tl) != 5 || tl[3].S("authorType") != "team" || tl[3].S("body") != "Demo response: The team received this task and context." {
		t.Fatalf("team reply/timeline wrong: %v", tl)
	}
}

// TestCollaborationTargetValidation covers atomic rejection (unknown targets, missing tasks for
// agent/team) and the un-executed workflow Form Mode, whose 3B-1 `409 workflow_not_available` is
// superseded: the interaction is now recorded with no run.
func TestCollaborationTargetValidation(t *testing.T) {
	f := setup(t)
	wid := reviewWorkflow(t, f)
	iss := f.call("POST", f.path("/issues"), core.Object{"title": "Validate"}, "v-issue", 200).O("resource")

	// Unknown agent id (directory resolves nothing) -> 404 target_not_found.
	f.call("POST", f.path("/issues/"+iss.S("id")+"/comments"), core.Object{
		"body": "x", "targets": []any{core.Object{"type": "agent", "id": uuid.NewString(), "task": "t"}},
	}, "v-1", 404)
	// Non-member human target -> 404.
	f.call("POST", f.path("/issues/"+iss.S("id")+"/comments"), core.Object{
		"body": "x", "targets": []any{core.Object{"type": "user", "id": uuid.NewString()}},
	}, "v-2", 404)
	// Agent task without a task -> 400 task_required.
	f.call("POST", f.path("/issues/"+iss.S("id")+"/comments"), core.Object{
		"body": "x", "targets": []any{core.Object{"type": "agent", "id": collab.BackendAgentID}},
	}, "v-3", 400)
	// Workflow is Form Mode: the interaction is recorded but NOT executed (supersedes the 3B-1
	// `409 workflow_not_available`). See workflow_interaction_test.go for the full Form Mode contract.
	f.call("POST", f.path("/issues/"+iss.S("id")+"/comments"), core.Object{
		"body": "x", "targets": []any{core.Object{"type": "workflow", "id": wid}},
	}, "v-4", 200)
	if n := f.scalar("SELECT count(*) FROM issue_runs WHERE issue_id=$1", iss.S("id")); n != 0 {
		t.Fatalf("workflow target must not execute on @")
	}
	// Atomic: each of v-1..v-3 rolled back its whole comment; only v-4 (the workflow target) committed.
	if n := f.scalar("SELECT count(*) FROM issue_comments WHERE issue_id=$1", iss.S("id")); n != 1 {
		t.Fatalf("rejected comments leaked or the accepted one was lost: %d", n)
	}
	if n := f.scalar("SELECT count(*) FROM issue_interactions WHERE issue_id=$1", iss.S("id")); n != 1 {
		t.Fatalf("rejected interactions leaked or the accepted one was lost: %d", n)
	}
}

// TestContextBuilderBoundedAndDeterministic seeds more than the recent-comments window and explicit
// context refs, then asserts the snapshot is bounded and deterministic.
func TestContextBuilderBoundedAndDeterministic(t *testing.T) {
	f := setup(t)
	iss := f.call("POST", f.path("/issues"), core.Object{"title": "Context", "description": "desc"}, "cb-issue", 200).O("resource")
	// Seed 12 explicit comments so the recent window is exercised.
	for i := 0; i < 12; i++ {
		f.call("POST", f.path("/issues/"+iss.S("id")+"/comments"), core.Object{"body": "seed"}, "cb-"+uuid.NewString(), 200)
	}
	// Explicit context refs are included unresolved.
	f.call("POST", f.path("/issues/"+iss.S("id")+"/context-refs"), core.Object{"refType": "project", "refId": uuid.NewString()}, "cb-ref", 200)

	f.call("POST", f.path("/issues/"+iss.S("id")+"/comments"), core.Object{
		"body":    "go",
		"targets": []any{core.Object{"type": "agent", "id": collab.BackendAgentID, "task": "the task"}},
	}, "cb-go", 200)

	run := issueItems(f.call("GET", f.path("/issues/"+iss.S("id")+"/runs"), nil, "", 200))[0]
	input := run.O("input")
	rc, _ := input["recentComments"].([]any)
	if len(rc) != 10 {
		t.Fatalf("recent comments window should be bounded to 10, got %d", len(rc))
	}
	// Deterministic layout: task, target, issue, and explicit refs are all snapshotted.
	if input.O("target").S("id") != collab.BackendAgentID || input.O("issue").S("title") != "Context" {
		t.Fatalf("deterministic bundle wrong: %v", input)
	}
	if refs, _ := input["contextRefs"].([]any); len(refs) != 1 {
		t.Fatalf("explicit context refs not snapshotted: %v", input)
	}
}

// TestSecurityCannotImpersonateActorAndFailureLifecycle covers the impersonation guard (clients
// cannot set authorType) and the inbound failed/cancelled observer transitions.
func TestSecurityCannotImpersonateActorAndFailureLifecycle(t *testing.T) {
	f := setup(t)
	iss := f.call("POST", f.path("/issues"), core.Object{"title": "Security"}, "s-issue", 200).O("resource")

	// The public comment API rejects a server-owned actor field outright.
	f.call("POST", f.path("/issues/"+iss.S("id")+"/comments"), core.Object{"body": "imp", "authorType": "agent"}, "s-1", 400)
	// The public run API likewise rejects trigger provenance fields.
	f.call("POST", f.path("/issues/"+iss.S("id")+"/runs"), core.Object{"executorType": "agent", "executorId": collab.BackendAgentID, "triggerEvidenceKind": "comment"}, "s-2", 400)

	// Manual run stays queued (no dispatcher target path). The inbound observer seam transitions it.
	manual := f.call("POST", f.path("/issues/"+iss.S("id")+"/runs"), core.Object{"executorType": "agent", "executorId": collab.ReviewAgentID}, "s-3", 200).O("resource")
	if manual.S("status") != "queued" {
		t.Fatalf("manual run should stay queued: %v", manual)
	}
	failed, err := f.store.ObserveFailed(context.Background(), f.tid, manual.S("id"), "boom")
	must(t, err)
	if failed.S("status") != "failed" || failed.S("failureReason") != "boom" {
		t.Fatalf("failed transition wrong: %v", failed)
	}

	// A second manual run can be cancelled from queued.
	manual2 := f.call("POST", f.path("/issues/"+iss.S("id")+"/runs"), core.Object{"executorType": "team", "executorId": collab.PlatformTeamID}, "s-4", 200).O("resource")
	cancelled, err := f.store.ObserveCancelled(context.Background(), f.tid, manual2.S("id"))
	must(t, err)
	if cancelled.S("status") != "cancelled" {
		t.Fatalf("cancelled transition wrong: %v", cancelled)
	}
}

// TestPendingRunDedup covers the frozen pending-run rule (Issue collaboration D4): at most one queued/dispatched run
// per (issue, executor_type, executor_id); a terminal run frees the slot. This constraint must not
// change this wave.
func TestPendingRunDedup(t *testing.T) {
	f := setup(t)
	iss := f.call("POST", f.path("/issues"), core.Object{"title": "Pending"}, "pd-issue", 200).O("resource")

	first := f.call("POST", f.path("/issues/"+iss.S("id")+"/runs"), core.Object{"executorType": "agent", "executorId": collab.BackendAgentID}, "pd-1", 200).O("resource")
	if first.S("status") != "queued" {
		t.Fatalf("manual run should stay queued: %v", first)
	}
	// A second pending run for the same executor is rejected with the stable 409.
	f.call("POST", f.path("/issues/"+iss.S("id")+"/runs"), core.Object{"executorType": "agent", "executorId": collab.BackendAgentID}, "pd-2", 409)

	// Once the first reaches a terminal state the slot is free again.
	cancelled, err := f.store.ObserveCancelled(context.Background(), f.tid, first.S("id"))
	must(t, err)
	if cancelled.S("status") != "cancelled" {
		t.Fatalf("cancelled transition wrong: %v", cancelled)
	}
	second := f.call("POST", f.path("/issues/"+iss.S("id")+"/runs"), core.Object{"executorType": "agent", "executorId": collab.BackendAgentID}, "pd-3", 200).O("resource")
	if second.S("id") == first.S("id") || second.S("status") != "queued" {
		t.Fatalf("run not re-created after terminal: %v", second)
	}
}
