-- Cloud 定时运行的产物交付、登记与被拒清单（specs
-- decisions/cloud/scheduled-task/20261022-run-verdict-and-artifacts.md，approved 的 D3/D4/D5）。
--
-- 产物声明本身留在会话终态的 node_executions.result 里（不可变），这里只存交付状态与已经核对过的登记索引：
-- 对象键由 Cloud 生成、登记前按 size/SHA-256 复核，所以 scheduled_run_artifacts 里的每一行都代表一个
-- 存在且已核对的对象，而不是 Node 的声明。
--
-- task_id 与 scheduled_at 在写入时从 occurrence 冻结，occurrence_id 只作审计：100 条历史裁剪把它置空，
-- 不级联删除运行级事实。每任务的 10 个运行淘汰按冻结的 scheduled_at 从新到旧，不依赖 occurrence 行。

-- 一个运行现在可以同时持有一个 Revision 交付工作项和一个产物上传工作项（ADR D3 要求产物是独立工作项）。
-- 未登记工作的唯一性因此从"每运行一个"收紧为"每运行每类一个"；RecordDispatch 本来就按
-- input->>'kind' 定位未登记项，领取仍是先到先领，所以这条索引是唯一的改动点。工作流侧保持不变。
DROP INDEX one_unregistered_execution_work;
CREATE UNIQUE INDEX one_unregistered_execution_work ON execution_work(run_id, kind)
 WHERE execution_id IS NULL AND workflow_run_id IS NULL;
-- The kind itself is a closed set; the artifact upload is the third kind of IssueRun work.
ALTER TABLE execution_work DROP CONSTRAINT execution_work_kind_check;
ALTER TABLE execution_work ADD CONSTRAINT execution_work_kind_check
 CHECK (kind IN ('agent_session','deliver_revision','deliver_scheduled_artifacts'));

CREATE TABLE scheduled_run_artifact_deliveries (
 issue_run_id uuid PRIMARY KEY REFERENCES issue_runs(id),
 tenant_id uuid NOT NULL REFERENCES tenants(id),
 task_id uuid NOT NULL REFERENCES scheduled_tasks(id),
 occurrence_id uuid REFERENCES scheduled_task_occurrences(id) ON DELETE SET NULL,
 scheduled_at timestamptz NOT NULL,
 state text NOT NULL CHECK(state IN ('delivering','delivered','partial','failed','skipped')),
 -- 以运行身份唯一、不可续期：登记时刻 + 10 分钟，重启、重放与重派都不重算。
 deadline_at timestamptz NOT NULL,
 settled_at timestamptz,
 -- 收敛原因：skipped 记 object_store_unconfigured，failed 记窗口到期或上传失败。
 settled_reason text,
 -- 前缀清理扫过这一刻之后就不再重扫：清理按确定性对象键删除已上传未登记的对象，
 -- 不需要列举桶，但需要记住"这个运行已经清过"，否则每一轮都会重删。
 artifacts_gc_at timestamptz,
 created_at timestamptz NOT NULL DEFAULT now()
);
-- 到期结算扫描只需要仍在交付中的行。
CREATE INDEX scheduled_run_artifact_deliveries_due ON scheduled_run_artifact_deliveries(deadline_at)
 WHERE state='delivering';
-- 前缀清理只关心尚未清过、且已经收敛的交付。
CREATE INDEX scheduled_run_artifact_deliveries_gc ON scheduled_run_artifact_deliveries(settled_at)
 WHERE state IN ('failed','partial','skipped') AND artifacts_gc_at IS NULL;

CREATE TABLE scheduled_run_artifacts (
 tenant_id uuid NOT NULL REFERENCES tenants(id),
 issue_run_id uuid NOT NULL REFERENCES issue_runs(id),
 task_id uuid NOT NULL REFERENCES scheduled_tasks(id),
 occurrence_id uuid REFERENCES scheduled_task_occurrences(id) ON DELETE SET NULL,
 scheduled_at timestamptz NOT NULL,
 artifact_index integer NOT NULL CHECK(artifact_index >= 0 AND artifact_index < 64),
 object_key text NOT NULL,
 display_name text NOT NULL,
 kind text NOT NULL CHECK(kind IN ('markdown','json','text','log','xml','html')),
 size_bytes bigint NOT NULL CHECK(size_bytes > 0 AND size_bytes <= 10485760),
 sha256 text NOT NULL CHECK(sha256 ~ '^[0-9a-f]{64}$'),
 created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY (issue_run_id, artifact_index)
);
CREATE INDEX scheduled_run_artifacts_task ON scheduled_run_artifacts(task_id, scheduled_at DESC);

CREATE TABLE scheduled_run_artifact_rejections (
 tenant_id uuid NOT NULL REFERENCES tenants(id),
 issue_run_id uuid NOT NULL REFERENCES issue_runs(id),
 task_id uuid NOT NULL REFERENCES scheduled_tasks(id),
 occurrence_id uuid REFERENCES scheduled_task_occurrences(id) ON DELETE SET NULL,
 scheduled_at timestamptz NOT NULL,
 display_name text NOT NULL,
 reason text NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY (issue_run_id, display_name)
);
CREATE INDEX scheduled_run_artifact_rejections_task ON scheduled_run_artifact_rejections(task_id, scheduled_at DESC);
