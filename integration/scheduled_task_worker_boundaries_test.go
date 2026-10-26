package integration

import (
	"context"
	"testing"
)

// TestScheduledTaskCreatorInactiveAutoDisables pins D1/D5 自动停用：一个在保存时资格有效、但接纳前
// 创建者已失去有效租户成员资格的任务，必须在评估 pass 中被自动停用（enabled=false 且
// disabled_reason='creator_inactive'），且不写入任何 occurrence。失效后的任务不转移、不改绑。
func TestScheduledTaskCreatorInactiveAutoDisables(t *testing.T) {
	store, s, agentID := scheduledTaskStore(t)
	taskID := seedEnabledScheduledTask(t, store, s, agentID, "Creator inactive", 65)
	// Keep a second active administrator before disabling the creator's membership. The fixture
	// otherwise has exactly one administrator, and the last-admin aggregate guard correctly
	// refuses a membership transition that production could not make.
	_, err := store.Pool.Exec(`
		WITH backup AS (
			INSERT INTO users(id, display_name, status)
			VALUES(gen_random_uuid(), 'Scheduled-task backup administrator', 'active')
			RETURNING id
		)
		INSERT INTO tenant_memberships(tenant_id, user_id, role, status)
		SELECT $1, id, 'admin', 'active' FROM backup
	`, s.tenantID)
	must(t, err)
	_, err = store.Pool.Exec(`UPDATE tenant_memberships SET status='disabled' WHERE tenant_id=$1 AND user_id=$2`, s.tenantID, s.userID)
	must(t, err)

	must(t, store.EvaluateScheduledTasksOnce(context.Background()))

	var enabled bool
	var reason string
	must(t, store.Pool.QueryRow(`SELECT enabled, COALESCE(disabled_reason,'') FROM scheduled_tasks WHERE id=$1`, taskID).Scan(&enabled, &reason))
	if enabled || reason != "creator_inactive" {
		t.Fatalf("creator membership loss must auto-disable the task, got enabled=%v reason=%q", enabled, reason)
	}
	if count, _ := occurrenceCount(t, store, taskID); count != 0 {
		t.Fatalf("an auto-disabled task must record no occurrence, got %d", count)
	}
}

// TestScheduledTaskIssueDeletedAutoDisables pins D1 自动停用：绑定 issue 被删除后，任务在评估 pass 中
// 自动停用（disabled_reason='issue_deleted'），不自动解绑或改绑，也不写入 occurrence。
func TestScheduledTaskIssueDeletedAutoDisables(t *testing.T) {
	store, s, agentID := scheduledTaskStore(t)
	taskID := seedEnabledScheduledTask(t, store, s, agentID, "Issue deleted", 65)
	_, err := store.Pool.Exec(`UPDATE issues SET deleted_at=now() WHERE id=$1`, s.issueID)
	must(t, err)

	must(t, store.EvaluateScheduledTasksOnce(context.Background()))

	var enabled bool
	var reason string
	must(t, store.Pool.QueryRow(`SELECT enabled, COALESCE(disabled_reason,'') FROM scheduled_tasks WHERE id=$1`, taskID).Scan(&enabled, &reason))
	if enabled || reason != "issue_deleted" {
		t.Fatalf("a deleted bound issue must auto-disable the task, got enabled=%v reason=%q", enabled, reason)
	}
	if count, _ := occurrenceCount(t, store, taskID); count != 0 {
		t.Fatalf("an auto-disabled task must record no occurrence, got %d", count)
	}
}

// TestScheduledTaskDeadlineWriterCancelsDueRunAndIsIdempotent pins D4 截止写入：接纳时持久化的
// deadline 到期后，写入 pass 在同一事务里写 cancel_requested_at 与 deadline_cancelled_at；事实以
// occurrence 身份幂等，重复 pass 不重写。
func TestScheduledTaskDeadlineWriterCancelsDueRunAndIsIdempotent(t *testing.T) {
	store, s, agentID := scheduledTaskStore(t)
	taskID := seedEnabledScheduledTask(t, store, s, agentID, "Deadline due", 65)
	must(t, store.EvaluateScheduledTasksOnce(context.Background()))

	var runID string
	must(t, store.Pool.QueryRow(`SELECT run_id FROM scheduled_task_occurrences WHERE task_id=$1`, taskID).Scan(&runID))
	if runID == "" {
		t.Fatalf("admission must create a run")
	}
	_, err := store.Pool.Exec(`UPDATE scheduled_task_occurrences SET deadline_at = now() - interval '1 minute' WHERE task_id=$1`, taskID)
	must(t, err)

	must(t, store.WriteScheduledTaskDeadlinesOnce(context.Background()))

	var cancelRequested, deadlineCancelled bool
	must(t, store.Pool.QueryRow(`SELECT r.cancel_requested_at IS NOT NULL, o.deadline_cancelled_at IS NOT NULL
		FROM issue_runs r JOIN scheduled_task_occurrences o ON o.run_id=r.id WHERE o.task_id=$1`, taskID).Scan(&cancelRequested, &deadlineCancelled))
	if !cancelRequested || !deadlineCancelled {
		t.Fatalf("deadline pass must write the cancel request and deadline fact, got cancel=%v fact=%v", cancelRequested, deadlineCancelled)
	}

	// Idempotency: the guard (IS NULL) must make a second pass a no-op on the already-fact'd run.
	var firstCancel, firstFact string
	must(t, store.Pool.QueryRow(`SELECT r.cancel_requested_at::text, o.deadline_cancelled_at::text FROM issue_runs r JOIN scheduled_task_occurrences o ON o.run_id=r.id WHERE o.task_id=$1`, taskID).Scan(&firstCancel, &firstFact))
	must(t, store.WriteScheduledTaskDeadlinesOnce(context.Background()))
	var secondCancel, secondFact string
	must(t, store.Pool.QueryRow(`SELECT r.cancel_requested_at::text, o.deadline_cancelled_at::text FROM issue_runs r JOIN scheduled_task_occurrences o ON o.run_id=r.id WHERE o.task_id=$1`, taskID).Scan(&secondCancel, &secondFact))
	if firstCancel != secondCancel || firstFact != secondFact {
		t.Fatalf("deadline fact must be written exactly once, got cancel %q -> %q and fact %q -> %q", firstCancel, secondCancel, firstFact, secondFact)
	}
}

// TestScheduledTaskDeadlineWriterSkipsTerminalRun pins the D4 boundary：截止扫描不得追改已经终态或
// 会话已结束的运行——即便 deadline_at 已到期。
func TestScheduledTaskDeadlineWriterSkipsTerminalRun(t *testing.T) {
	store, s, agentID := scheduledTaskStore(t)
	taskID := seedEnabledScheduledTask(t, store, s, agentID, "Deadline terminal", 65)
	must(t, store.EvaluateScheduledTasksOnce(context.Background()))

	var runID string
	must(t, store.Pool.QueryRow(`SELECT run_id FROM scheduled_task_occurrences WHERE task_id=$1`, taskID).Scan(&runID))
	_, err := store.Pool.Exec(`UPDATE scheduled_task_occurrences SET deadline_at = now() - interval '1 minute' WHERE task_id=$1`, taskID)
	must(t, err)
	_, err = store.Pool.Exec(`UPDATE issue_runs SET status='completed' WHERE id=$1`, runID)
	must(t, err)

	must(t, store.WriteScheduledTaskDeadlinesOnce(context.Background()))

	var cancelRequested, deadlineCancelled bool
	must(t, store.Pool.QueryRow(`SELECT r.cancel_requested_at IS NOT NULL, o.deadline_cancelled_at IS NOT NULL
		FROM issue_runs r JOIN scheduled_task_occurrences o ON o.run_id=r.id WHERE o.task_id=$1`, taskID).Scan(&cancelRequested, &deadlineCancelled))
	if cancelRequested || deadlineCancelled {
		t.Fatalf("deadline pass must not write a fact for a terminal run, got cancel=%v fact=%v", cancelRequested, deadlineCancelled)
	}
}
