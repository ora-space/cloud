-- Cloud 首期 Scheduled Task（specs decisions/cloud/scheduled-task/0-cloud-owned-scheduled-prompt-runs.md，
-- approved）。定义持久化在租户内；occurrence 是接纳与历史的记录，(task_id, scheduled_at) 唯一。
-- 截止取消事实（deadline_cancelled_at）由调度层在写取消请求的同一事务里持久化，投影不得从
-- issue_runs.cancel_requested_at 反推。插件版本不在定义上冻结：既有 IssueRun 用例在接纳时按
-- Space 当时值快照（agent_target.go）。

CREATE TABLE scheduled_tasks (
 id uuid PRIMARY KEY,
 tenant_id uuid NOT NULL REFERENCES tenants(id),
 issue_id uuid NOT NULL REFERENCES issues(id),
 creator_user_id uuid NOT NULL REFERENCES users(id),
 agent_id uuid NOT NULL REFERENCES space_agents(id),
 name text NOT NULL CHECK(length(name) BETWEEN 1 AND 200),
 prompt text NOT NULL CHECK(length(prompt) BETWEEN 1 AND 64000),
 schedule jsonb NOT NULL CHECK(jsonb_typeof(schedule)='object'),
 timezone text NOT NULL CHECK(length(timezone) BETWEEN 1 AND 64),
 revision bigint NOT NULL CHECK(revision > 0),
 version bigint NOT NULL CHECK(version > 0),
 enabled boolean NOT NULL DEFAULT false,
 enabled_at timestamptz,
 disabled_reason text,
 unattended_policy text,
 unattended_consented_at timestamptz,
 deleted_at timestamptz,
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now(),
 CHECK((enabled = false) OR (unattended_policy IS NOT NULL AND unattended_consented_at IS NOT NULL)),
 CHECK((enabled = false) OR (enabled_at IS NOT NULL)),
 UNIQUE(tenant_id, issue_id, creator_user_id, name)
);
CREATE INDEX scheduled_tasks_tenant_list ON scheduled_tasks(tenant_id, created_at, id);
CREATE INDEX scheduled_tasks_due ON scheduled_tasks(enabled, deleted_at) WHERE enabled AND deleted_at IS NULL;

CREATE TABLE scheduled_task_occurrences (
 tenant_id uuid NOT NULL REFERENCES tenants(id),
 task_id uuid NOT NULL REFERENCES scheduled_tasks(id),
 scheduled_at timestamptz NOT NULL,
 outcome text NOT NULL CHECK(outcome IN ('admitted','missed','skipped_busy','failed')),
 failure_reason text CHECK(failure_reason IN ('target_unavailable','model_unavailable','approval_policy_invalid')),
 run_id uuid REFERENCES issue_runs(id),
 task_revision bigint NOT NULL CHECK(task_revision > 0),
 deadline_at timestamptz,
 deadline_cancelled_at timestamptz,
 observed_at timestamptz NOT NULL,
 PRIMARY KEY(task_id, scheduled_at)
);
CREATE INDEX scheduled_task_occurrences_run ON scheduled_task_occurrences(run_id) WHERE run_id IS NOT NULL;
CREATE INDEX scheduled_task_deadlines_due ON scheduled_task_occurrences(deadline_at)
 WHERE deadline_cancelled_at IS NULL AND deadline_at IS NOT NULL;
