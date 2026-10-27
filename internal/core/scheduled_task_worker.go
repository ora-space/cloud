package core

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Cloud Scheduled Task 的运行时半部：到期评估（D2/D3/D5）、60 分钟截止写入（D4）与 100 条保留
// （D6）。所有窗口与阈值都用数据库时间判断；每个任务的评估在串行链的独立短事务里完成，
// occurrence 的 `(task_id, scheduled_at)` 主键使多副本、重启与重复扫描天然幂等（D2 不变量 2）。

const (
	// scheduledTaskEvalBatchSize bounds each database page; a pass advances a stable keyset cursor
	// through every enabled task so a fixed first page cannot starve later definitions.
	scheduledTaskEvalBatchSize = 100
)

type scheduledTaskPageEntry struct {
	id        string
	updatedAt time.Time
}

// scheduledTaskDefinitionError marks an invalid persisted definition. New writes are validated at
// the public boundary, but an old or manually corrupted row must not starve unrelated due tasks.
// It is kept distinct from database and context errors so those still stop the pass.
type scheduledTaskDefinitionError struct{ err error }

func (e *scheduledTaskDefinitionError) Error() string { return e.err.Error() }
func (e *scheduledTaskDefinitionError) Unwrap() error { return e.err }

// scanEnabledScheduledTaskPage owns one short rows lifetime. Keeping Close in this helper makes
// every error path close the cursor before the next page or transaction begins.
func (s *Store) scanEnabledScheduledTaskPage(ctx context.Context, cursorAt *time.Time, cursorID *string) ([]scheduledTaskPageEntry, error) {
	rows, err := s.Pool.QueryContext(ctx, `SELECT id, updated_at FROM scheduled_tasks
		WHERE enabled AND deleted_at IS NULL
		  AND ($1::timestamptz IS NULL OR (updated_at, id) > ($1, $2::uuid))
		ORDER BY updated_at, id LIMIT $3`, cursorAt, cursorID, scheduledTaskEvalBatchSize)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var page []scheduledTaskPageEntry
	for rows.Next() {
		var entry scheduledTaskPageEntry
		if err := rows.Scan(&entry.id, &entry.updatedAt); err != nil {
			return nil, err
		}
		page = append(page, entry)
	}
	return page, rows.Err()
}

// EvaluateScheduledTasksOnce 是一个完整的到期评估 pass：自动停用失效任务、为每个到期任务求值
// 最新到期点并幂等接纳。由 cmd/server 的 RunSyncLoop 驱动；返回的错误由 RunSyncLoop 记录，
// 循环本身不停止。
func (s *Store) EvaluateScheduledTasksOnce(ctx context.Context) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	// The accepted domain implementation stays dormant until its end-to-end acceptance is
	// explicitly enabled by deployment configuration. This is the worker half of the same gate the
	// public save/enable endpoints enforce; otherwise an old enabled row could still be admitted
	// while the Web surface is intentionally unavailable.
	if !s.ScheduledTaskAcceptanceEnabled {
		return nil
	}
	var cursorAt *time.Time
	var cursorID *string
	var definitionErrors []error
	for {
		page, err := s.scanEnabledScheduledTaskPage(ctx, cursorAt, cursorID)
		if err != nil {
			return err
		}
		if len(page) == 0 {
			break
		}
		for _, entry := range page {
			_, err := s.transact(ctx, func(t *transaction) Object {
				admitScheduledTaskDue(t, entry.id)
				return nil
			})
			if err == nil {
				continue
			}
			var fault *Fault
			if errors.As(err, &fault) {
				// 单个任务的业务性拒绝（资格失败已映射到 occurrence）不影响其它任务。
				continue
			}
			var definitionErr *scheduledTaskDefinitionError
			if errors.As(err, &definitionErr) {
				// 已持久化的损坏定义只回滚本任务的事务；保留错误供 RunSyncLoop
				// 记录，但继续评估同批及后续页的健康任务。
				definitionErrors = append(definitionErrors, err)
				continue
			}
			// 系统级故障（数据库不可用、上下文取消）停止本轮，交给 RunSyncLoop 记录。
			return err
		}
		last := page[len(page)-1]
		cursorAt, cursorID = &last.updatedAt, &last.id
	}
	return errors.Join(definitionErrors...)
}

// admitScheduledTaskDue 求值一个任务的最新到期点，并在同一事务里完成：自动停用检查 → 幂等
// occurrence 写入 → 资格校验（失败记原因）→ skipped_busy → agent IssueRun 创建 + 截止时刻
// 持久化（D3/D5）。任何一步的失败都只落到 occurrence 的结果上，绝不回退成其它执行位置。
func admitScheduledTaskDue(t *transaction, taskID string) {
	task := t.one("SELECT * FROM scheduled_tasks WHERE id=$1 AND deleted_at IS NULL", taskID)
	if task == nil || !task.B("enabled") {
		return
	}
	// D1/D5：创建者失去有效租户成员资格，或绑定 issue 被删除 → 自动停用（不转移、不改绑）。
	if !creatorMembershipActive(t, taskID, task.S("tenantId"), task.S("creatorUserId")) {
		disableScheduledTask(t, taskID, "creator_inactive")
		return
	}
	issue := scheduledTaskIssue(t, task.S("tenantId"), task.S("issueId"))
	if issue == nil {
		disableScheduledTask(t, taskID, "issue_deleted")
		return
	}

	loc := scheduledTaskLocation(task, taskID)
	now := dbNow(t)
	schedule, err := parseScheduledTaskSchedule(task["schedule"])
	if err != nil {
		panic(databaseFailure{&scheduledTaskDefinitionError{err: fmt.Errorf("scheduled task %s has a corrupt schedule: %w", taskID, err)}})
	}

	// D2：直接落到 `now` 之前的最新到期点。停机期间更早的到期点一律不写——一次停机至多一条
	// missed，窗口内的最新点照常接纳。幂等由 occurrence 的 (task_id, scheduled_at) 主键保证。
	// transaction.list reads rows through row_to_json, so timestamptz values arrive as
	// RFC3339 strings rather than time.Time. Treating that string as a time.Time silently
	// produced a zero anchor and made every enabled definition look not-yet-due.
	anchor, err := time.Parse(time.RFC3339Nano, task.S("enabledAt"))
	if err != nil {
		panic(databaseFailure{&scheduledTaskDefinitionError{err: fmt.Errorf("scheduled task %s has an invalid enabled_at: %w", taskID, err)}})
	}
	due := schedule.latestDue(anchor, now, loc)
	if due.IsZero() {
		return
	}
	dueText := due.UTC().Format(time.RFC3339Nano)

	// 幂等写入：先到的事务接纳，后到的读取既有结果原样返回（D2 不变量 2）。
	if inserted := t.execRows(`INSERT INTO scheduled_task_occurrences
		(tenant_id, task_id, scheduled_at, outcome, task_revision, observed_at)
		VALUES($1,$2,$3,'admitted',$4,$5)
		ON CONFLICT (task_id, scheduled_at) DO NOTHING`,
		task.S("tenantId"), taskID, due, task.N("revision"), now); inserted != 1 {
		return
	}
	// 100 条保留（D6）：超出后同事务从最旧开始清理（活跃运行顺延），只动任务侧记录。
	pruneScheduledTaskHistory(t, taskID)

	// 迟到阈值（D2）：观察晚于 scheduled_at 超过 10 分钟的到期点记 missed，不执行也不补跑。
	// 只求值最新到期点，因此一次停机对每个任务至多产生一条 missed。
	if now.Sub(due) > maxScheduledOccurrenceLateness {
		t.exec(`UPDATE scheduled_task_occurrences SET outcome='missed'
			WHERE task_id=$1 AND scheduled_at=$2`, taskID, due)
		return
	}

	// 资格校验（D5）：失败映射到根决策 D7 的原因集并终止该 occurrence，不重试不回退。
	if !creatorMembershipActive(t, taskID, task.S("tenantId"), task.S("creatorUserId")) {
		failScheduledOccurrenceWithReason(t, taskID, due, "target_unavailable")
		return
	}
	agent, ready := scheduledTaskAgentReady(t, task.S("tenantId"), task.S("agentId"))
	if !ready {
		failScheduledOccurrenceWithReason(t, taskID, due, "target_unavailable")
		return
	}
	if !scheduledTaskSpaceActive(t, task.S("tenantId"), agent.S("spaceId")) {
		failScheduledOccurrenceWithReason(t, taskID, due, "target_unavailable")
		return
	}
	if !agentRunGateOK(t, issue, agent) {
		failScheduledOccurrenceWithReason(t, taskID, due, "target_unavailable")
		return
	}
	if !creatorModelReady(t, task.S("creatorUserId"), agent.S("pluginId")) {
		failScheduledOccurrenceWithReason(t, taskID, due, "model_unavailable")
		return
	}
	if !scheduledTaskModelReady(t, task.S("creatorUserId"), task.S("model")) {
		failScheduledOccurrenceWithReason(t, taskID, due, "model_unavailable")
		return
	}
	// bindRunModel rejects after enqueueing for an invalid default or declared configuration. Probe
	// the same read-only policy first so that rejection becomes this occurrence's durable D5 result
	// instead of rolling back the ledger row and later being mistaken for a missed schedule point.
	if personal, gap := taskAgentModelGap(t, task.S("creatorUserId"), agent, task.S("model")); personal && gap != "" {
		failScheduledOccurrenceWithReason(t, taskID, due, scheduledAgentBindingReason(gap))
		return
	}

	// 任务对话准入（D5）：enqueueAdmittedRun 不经过 enqueueRun 的对话守卫，这里按同一规则预检。
	// 对话暂停或有未解决的 failed 消息时运行永远领不到，有 waiting 消息时定时运行会插到它前面。
	if gap := taskConversationInputGap(t, task.S("tenantId"), task.S("issueId")); gap != "" {
		failScheduledOccurrenceWithReason(t, taskID, due, gap)
		return
	}

	// skipped_busy（D6）：同一任务上一次接纳的运行尚未到达终态时跳过，不排队不补跑。
	if scheduledTaskBusy(t, taskID, task.S("issueId"), task.S("agentId"), dueText) {
		t.exec(`UPDATE scheduled_task_occurrences SET outcome='skipped_busy'
			WHERE task_id=$1 AND scheduled_at=$2`, taskID, due)
		return
	}

	// 运行创建复用既有编排：buildRunContext + snapshotAgentRunInput + enqueueAdmittedRun
	// （内部完成 bindRunModel 的模型冻结与 409 pending_run_exists 兜底）。触发证据指向本任务。
	runContext := buildRunContext(t, issue, task.S("prompt"), "agent", task.S("agentId"), nil, nil)
	input := snapshotAgentRunInput(runContext, agent)
	// This is an internal handoff, not caller-controlled execution input. enqueueRunWithInvocation
	// removes it before persistence and bindRunModel freezes the resolved model binding instead.
	input["scheduledTaskModel"] = task.S("model")
	run := enqueueAdmittedRun(t, task.S("tenantId"), task.S("issueId"), "agent", task.S("agentId"),
		input, "scheduled_task", taskID, "user", task.S("creatorUserId"))

	// 截止时刻随接纳持久化（D4）；deadline_cancelled_at 留给截止写入者。
	t.exec(`UPDATE scheduled_task_occurrences
		SET run_id=$1, deadline_at=$2
		WHERE task_id=$3 AND scheduled_at=$4`,
		run.S("id"), now.Add(scheduledTaskDeadline), taskID, due)
}

// disableScheduledTask 记录自动停用（D1/D5）。既有运行事实不改写，重新启用需创建者显式操作。
func disableScheduledTask(t *transaction, taskID, reason string) {
	t.exec(`UPDATE scheduled_tasks
		SET enabled=false, enabled_at=NULL, disabled_reason=$1, updated_at=now()
		WHERE id=$2 AND enabled`, reason, taskID)
}

// failScheduledOccurrence 把资格校验失败落到 occurrence 上，原因映射见 scheduledFailureReason。
func failScheduledOccurrence(t *transaction, taskID string, due time.Time, reason string) {
	t.exec(`UPDATE scheduled_task_occurrences SET outcome='failed', failure_reason=$1
		WHERE task_id=$2 AND scheduled_at=$3`, reason, taskID, due)
}

// dbNow reads the database clock once per pass: every window and threshold comparison is the
// database's, never a Go process clock (Thread D4 invariant 5 的同源要求).
func dbNow(t *transaction) time.Time {
	row := t.one("SELECT now() AS now")
	at, err := time.Parse(time.RFC3339Nano, row.S("now"))
	if err != nil {
		panic(databaseFailure{fmt.Errorf("read database clock: %w", err)})
	}
	return at
}

// WriteScheduledTaskDeadlinesOnce 是 60 分钟截止的写入 pass（D4）：对已到截止时刻、会话尚未进入
// `ending | ended` 且 IssueRun 未到终态的运行，在同一事务里写 `cancel_requested_at` 与截止取消
// 事实。事实以 occurrence 身份幂等（deadline_cancelled_at IS NULL 的守卫使重复扫描只写一次）。
// 之后的会话收尾、交付与释放由既有的 ReactToCancelledAgentRunsOnce 与交付循环按其自身证据推进。
func (s *Store) WriteScheduledTaskDeadlinesOnce(ctx context.Context) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	type dueRun struct {
		taskID, scheduledAt, runID string
	}
	rows, err := s.Pool.QueryContext(ctx, `
		SELECT o.task_id, o.scheduled_at::text, o.run_id
		FROM scheduled_task_occurrences o
		JOIN issue_runs r ON r.id = o.run_id
		WHERE o.deadline_cancelled_at IS NULL
		  AND o.deadline_at IS NOT NULL AND o.deadline_at <= now()
		  AND r.status NOT IN ('completed','failed','cancelled')
		  AND (r.thread_state IS NULL OR r.thread_state NOT IN ('ending','ended'))
		  AND r.cancel_requested_at IS NULL
		LIMIT $1`, scheduledTaskEvalBatchSize)
	if err != nil {
		return err
	}
	defer rows.Close()
	var due []dueRun
	for rows.Next() {
		var entry dueRun
		if err := rows.Scan(&entry.taskID, &entry.scheduledAt, &entry.runID); err != nil {
			return err
		}
		due = append(due, entry)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, entry := range due {
		if _, err := s.transact(ctx, func(t *transaction) Object {
			run := t.one(`SELECT * FROM issue_runs
				WHERE id=$1 AND status NOT IN ('completed','failed','cancelled')
				  AND (thread_state IS NULL OR thread_state NOT IN ('ending','ended'))
				  AND cancel_requested_at IS NULL`, entry.runID)
			if run == nil {
				// 会话已结束或运行已终态：按 D4 的边界这是空操作，不补写截止事实——
				// 运行按自身终态投影，即便截止扫描随后发生也不追改。
				return nil
			}
			if moved := t.execRows(`UPDATE issue_runs
				SET cancel_requested_at=now(), version=version+1, updated_at=now()
				WHERE id=$1 AND cancel_requested_at IS NULL`, entry.runID); moved != 1 {
				panic(databaseFailure{fmt.Errorf("scheduled deadline: run %s was not marked cancelled (rows affected %d)", entry.runID, moved)})
			}
			if moved := t.execRows(`UPDATE scheduled_task_occurrences
				SET deadline_cancelled_at=now()
				WHERE task_id=$1 AND scheduled_at=$2 AND deadline_cancelled_at IS NULL`,
				entry.taskID, entry.scheduledAt); moved != 1 {
				panic(databaseFailure{fmt.Errorf("scheduled deadline: occurrence %s/%s already carried a deadline fact", entry.taskID, entry.scheduledAt)})
			}
			return nil
		}); err != nil {
			// 与评估 pass 同一循环契约：单条失败由 RunSyncLoop 记录，下一 pass 重试。
			return err
		}
	}
	return nil
}

// pruneScheduledTaskHistory keeps the newest 100 occurrences per task (D6), deleting the single
// oldest non-active record when the bound is exceeded. Active runs (and the occurrence that names
// them) are skipped so a live run is never unlinked, and the IssueRun it references stays on the
// issue as the durable audit fact regardless of this cleanup.
func pruneScheduledTaskHistory(t *transaction, taskID string) {
	t.exec(`DELETE FROM scheduled_task_occurrences o
		WHERE o.task_id=$1
		  AND o.scheduled_at = (
		      SELECT o2.scheduled_at
		      FROM scheduled_task_occurrences o2
		      LEFT JOIN issue_runs r ON r.id = o2.run_id
		      WHERE o2.task_id = $1
		        AND (r.id IS NULL OR r.status IN ('completed','failed','cancelled'))
		      ORDER BY o2.scheduled_at ASC
		      LIMIT 1)
		  AND (SELECT COUNT(*) FROM scheduled_task_occurrences WHERE task_id=$1) > 100`, taskID)
}
