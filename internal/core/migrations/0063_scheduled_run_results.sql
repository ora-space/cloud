-- Cloud 定时运行的业务 verdict（specs decisions/cloud/scheduled-task/20261022-run-verdict-and-artifacts.md，
-- approved 的 D1/D2）。verdict 与执行层状态分开存放：只有定时触发且正常收尾的运行才有结果行，写入者是
-- Node 执行端经会话终态上报的结构化值，Cloud 只校验枚举、detail 长度与运行身份，没有任何从
-- issue_runs.status 或 Thread 文本推导的路径。
--
-- issue_run_id 是结果的唯一身份（一个运行至多一条，单次写入、重放幂等）；task_id 与 scheduled_at 是写入时
-- 从 occurrence 冻结的不可变任务归属与计划时间事实，随后不随 occurrence 的 100 条历史裁剪改变。
-- occurrence 引用只作审计：裁剪删除 occurrence 行时置空，不级联删除运行级 verdict。

-- occurrence 原本只以 (task_id, scheduled_at) 为身份。结果与被拒清单需要一条可空、可置空的审计引用，
-- 这里给它一个代理 id；不复制复合键，否则 ON DELETE SET NULL 会连冻结的 task_id 一起置空。
ALTER TABLE scheduled_task_occurrences ADD COLUMN id uuid;
UPDATE scheduled_task_occurrences SET id = gen_random_uuid() WHERE id IS NULL;
-- 既有接纳事务不写 id，所以列必须自带默认值，否则需要在接纳路径上再多传一个与业务无关的字段。
ALTER TABLE scheduled_task_occurrences ALTER COLUMN id SET DEFAULT gen_random_uuid();
ALTER TABLE scheduled_task_occurrences ALTER COLUMN id SET NOT NULL;
ALTER TABLE scheduled_task_occurrences ADD CONSTRAINT scheduled_task_occurrences_id_key UNIQUE (id);

CREATE TABLE scheduled_run_results (
 issue_run_id uuid PRIMARY KEY REFERENCES issue_runs(id),
 tenant_id uuid NOT NULL REFERENCES tenants(id),
 task_id uuid NOT NULL REFERENCES scheduled_tasks(id),
 occurrence_id uuid REFERENCES scheduled_task_occurrences(id) ON DELETE SET NULL,
 scheduled_at timestamptz NOT NULL,
 verdict text NOT NULL CHECK(verdict IN ('pass','warn','fail','blocked','idle','range_unresolved')),
 verdict_detail text CHECK(verdict_detail IS NULL OR char_length(verdict_detail) <= 2000),
 verdict_at timestamptz NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now()
);
-- 每任务的保留与历史读取都按冻结的 scheduled_at 从新到旧排序，不依赖可能已被裁剪的 occurrence 行。
CREATE INDEX scheduled_run_results_task ON scheduled_run_results(task_id, scheduled_at DESC);
