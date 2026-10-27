-- D5/D7: 绑定 issue 的任务对话已暂停或等待交接时，接纳被拒并记明确原因，不建运行。
-- 只放宽 failure_reason 的取值集合，既有行不改写。
ALTER TABLE scheduled_task_occurrences DROP CONSTRAINT IF EXISTS scheduled_task_occurrences_failure_reason_check;
ALTER TABLE scheduled_task_occurrences ADD CONSTRAINT scheduled_task_occurrences_failure_reason_check
  CHECK(failure_reason IN ('target_unavailable','model_unavailable','approval_policy_invalid','task_paused','task_handoff_pending'));
