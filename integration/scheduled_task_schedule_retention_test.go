package integration

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/wanglongan587/cloud/internal/core"
)

// seedDailyScheduledTask seeds an enabled daily 09:00 task in the given IANA timezone.
func seedDailyScheduledTask(t *testing.T, store *core.Store, s skeletonSeed, agentID, name, timezone string, enabledAgoDays int) string {
	t.Helper()
	taskID := uuid.NewString()
	_, err := store.Pool.Exec(`INSERT INTO scheduled_tasks
		(id,tenant_id,issue_id,creator_user_id,agent_id,name,prompt,schedule,timezone,revision,version,enabled,enabled_at,unattended_policy,unattended_consented_at)
		VALUES($1,$2,$3,$4,$5,$6,'Sync','{"kind":"daily","hour":9,"minute":0}'::jsonb,$7,1,1,true,now() - $8 * interval '1 day','full_auto_in_cloud_workspace',now())`,
		taskID, s.tenantID, s.issueID, s.userID, agentID, name, timezone, enabledAgoDays)
	must(t, err)
	return taskID
}

// TestScheduledTaskDisabledIntervalIsNotEvaluated pins D2 停用区间不评估、不补记：停用期间被 worker 跳过，
// 重新启用时 re-anchor enabled_at，因此被吞掉的到期点永远不会回填成 missed。
func TestScheduledTaskDisabledIntervalIsNotEvaluated(t *testing.T) {
	store, s, agentID := scheduledTaskStore(t)
	taskID := seedEnabledScheduledTask(t, store, s, agentID, "Disabled interval", 150)
	_, err := store.Pool.Exec(`UPDATE scheduled_tasks SET enabled=false, enabled_at=NULL WHERE id=$1`, taskID)
	must(t, err)

	must(t, store.EvaluateScheduledTasksOnce(context.Background()))
	if count, _ := occurrenceCount(t, store, taskID); count != 0 {
		t.Fatalf("a disabled task must evaluate to no occurrence, got %d", count)
	}

	_, err = store.Pool.Exec(`UPDATE scheduled_tasks SET enabled=true, enabled_at=now() WHERE id=$1`, taskID)
	must(t, err)
	must(t, store.EvaluateScheduledTasksOnce(context.Background()))
	if count, outcome := occurrenceCount(t, store, taskID); count != 0 {
		t.Fatalf("re-enable must not back-fill missed for the disabled window, got %d %q", count, outcome)
	}
}

// TestScheduledTaskDailyScheduleUsesTaskTimezone pins D2 时区语义：worker 从任务行加载 IANA 时区，daily 到期点
// 落在该时区的 09:00，而不是连接/服务器的本地时区。
func TestScheduledTaskDailyScheduleUsesTaskTimezone(t *testing.T) {
	store, s, agentID := scheduledTaskStore(t)
	taskID := seedDailyScheduledTask(t, store, s, agentID, "Daily tz", "Asia/Shanghai", 2)
	must(t, store.EvaluateScheduledTasksOnce(context.Background()))

	var scheduledAt time.Time
	must(t, store.Pool.QueryRow(`SELECT scheduled_at FROM scheduled_task_occurrences WHERE task_id=$1 ORDER BY scheduled_at DESC LIMIT 1`, taskID).Scan(&scheduledAt))
	shanghai, err := time.LoadLocation("Asia/Shanghai")
	must(t, err)
	local := scheduledAt.In(shanghai)
	if local.Hour() != 9 || local.Minute() != 0 {
		t.Fatalf("daily due point must be 09:00 in the task timezone, got %s", local.Format("15:04"))
	}
}

// TestScheduledTaskHistoryRetentionPrunesOldestNonActive pins D6 100 条保留：第 101 个 occurrence 写入后在同一
// 事务从最旧的非活跃记录开始清理——恰好删除最旧那条，其余历史与新接纳都保留，任务侧记录保持有界。
func TestScheduledTaskHistoryRetentionPrunesOldestNonActive(t *testing.T) {
	store, s, agentID := scheduledTaskStore(t)
	taskID := seedEnabledScheduledTask(t, store, s, agentID, "Retention", 65)
	_, err := store.Pool.Exec(`INSERT INTO scheduled_task_occurrences
		(tenant_id, task_id, scheduled_at, outcome, task_revision, observed_at)
		SELECT $1, $2, now() - (g * interval '1 hour'), 'missed', 1, now()
		FROM generate_series(1, 100) AS g`, s.tenantID, taskID)
	must(t, err)

	var oldest string
	must(t, store.Pool.QueryRow(`SELECT min(scheduled_at)::text FROM scheduled_task_occurrences WHERE task_id=$1`, taskID).Scan(&oldest))

	must(t, store.EvaluateScheduledTasksOnce(context.Background()))

	var total, admitted, gone int
	must(t, store.Pool.QueryRow(`SELECT count(*), count(*) FILTER (WHERE outcome='admitted') FROM scheduled_task_occurrences WHERE task_id=$1`, taskID).Scan(&total, &admitted))
	must(t, store.Pool.QueryRow(`SELECT count(*) FROM scheduled_task_occurrences WHERE task_id=$1 AND scheduled_at::text=$2`, taskID, oldest).Scan(&gone))
	if total != 100 || admitted != 1 {
		t.Fatalf("retention must keep 100 occurrences and admit exactly one, got total=%d admitted=%d", total, admitted)
	}
	if gone != 0 {
		t.Fatalf("retention must prune the oldest non-active occurrence, but it is still present")
	}
}

// TestScheduledTaskHistoryRetentionSkipsActiveRun pins D6 的活跃运行保护：最旧 occurrence 关联一个仍未终态的
// IssueRun 时，清理跳过它、改删次旧的非活跃记录，且该 IssueRun 本身保留。fixture 里的骨架 run 处于 queued，
// 正是一个非终态运行。第 101 个 occurrence 写入后，即便 worker 随后因该活跃运行判 busy，写入与顺延清理也已
// 发生，因此断言必须证明「次旧被删 + 新 occurrence 记为 skipped_busy」，而不能只断言评估前就成立的事实。
func TestScheduledTaskHistoryRetentionSkipsActiveRun(t *testing.T) {
	store, s, agentID := scheduledTaskStore(t)
	taskID := seedEnabledScheduledTask(t, store, s, agentID, "Retention active", 65)
	_, err := store.Pool.Exec(`INSERT INTO scheduled_task_occurrences
		(tenant_id, task_id, scheduled_at, outcome, task_revision, observed_at)
		SELECT $1, $2, now() - (g * interval '1 hour'), 'missed', 1, now()
		FROM generate_series(1, 100) AS g`, s.tenantID, taskID)
	must(t, err)
	_, err = store.Pool.Exec(`UPDATE scheduled_task_occurrences SET run_id=$1
		WHERE task_id=$2 AND scheduled_at=(SELECT min(scheduled_at) FROM scheduled_task_occurrences WHERE task_id=$2)`, s.runID, taskID)
	must(t, err)

	// 最旧 occurrence 已挂到活跃 run；次旧（最旧的非活跃记录）就是第 101 条写入后顺延清理要删的那条。
	var secondOldest string
	must(t, store.Pool.QueryRow(`SELECT min(scheduled_at)::text FROM scheduled_task_occurrences WHERE task_id=$1 AND run_id IS NULL`, taskID).Scan(&secondOldest))

	must(t, store.EvaluateScheduledTasksOnce(context.Background()))

	var total, activeLinked, skippedBusy, secondOldestGone, runStillThere int
	must(t, store.Pool.QueryRow(`SELECT count(*), count(*) FILTER (WHERE run_id=$1) FROM scheduled_task_occurrences WHERE task_id=$2`, s.runID, taskID).Scan(&total, &activeLinked))
	must(t, store.Pool.QueryRow(`SELECT count(*) FROM scheduled_task_occurrences WHERE task_id=$1 AND outcome='skipped_busy'`, taskID).Scan(&skippedBusy))
	must(t, store.Pool.QueryRow(`SELECT count(*) FROM scheduled_task_occurrences WHERE task_id=$1 AND scheduled_at::text=$2`, taskID, secondOldest).Scan(&secondOldestGone))
	must(t, store.Pool.QueryRow(`SELECT count(*) FROM issue_runs WHERE id=$1`, s.runID).Scan(&runStillThere))

	if total != 100 {
		t.Fatalf("retention must keep 100 occurrences, got %d", total)
	}
	if activeLinked != 1 {
		t.Fatalf("the occurrence backed by an active run must be skipped by retention, got %d linked rows", activeLinked)
	}
	if skippedBusy != 1 {
		t.Fatalf("the newly admitted due point must be recorded skipped_busy, got %d", skippedBusy)
	}
	if secondOldestGone != 0 {
		t.Fatalf("retention must prune the oldest non-active occurrence, but it is still present")
	}
	if runStillThere != 1 {
		t.Fatalf("the active IssueRun must survive retention pruning")
	}
}
