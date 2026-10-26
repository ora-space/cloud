-- A manual stop pauses the task independently of whether its Agent had already ended normally
-- while delivery was still settling. Only explicit resume/discard clears this durable barrier.
ALTER TABLE task_conversations ADD COLUMN paused_run_id uuid REFERENCES issue_runs(id);
