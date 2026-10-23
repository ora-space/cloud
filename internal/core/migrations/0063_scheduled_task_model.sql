-- D1: 创建者的模型选择随任务定义持久化（可空——缺省走创建者个人默认/agent 默认）。修改模型选择
-- 增加 revision（见 scheduled_task_api.go）。Additive；不改写已应用的迁移。
ALTER TABLE scheduled_tasks ADD COLUMN model text;
