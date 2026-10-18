-- Request facts are independent from chat history and contain no prompt, response or credential.
ALTER TABLE run_model_bindings ADD CONSTRAINT model_binding_run_identity UNIQUE(id,run_id);
ALTER TABLE model_access_grants ADD CONSTRAINT model_grant_attempt_identity UNIQUE(id,binding_id,execution_id);
CREATE TABLE model_request_attempts (
 id uuid PRIMARY KEY,
 tenant_id uuid NOT NULL,
 run_id uuid NOT NULL,
 binding_id uuid NOT NULL,
 grant_id uuid NOT NULL,
 execution_id text NOT NULL,
 model_id text NOT NULL,
 protocol text NOT NULL CHECK(protocol IN ('openai-completions','anthropic-messages')),
 streamed boolean NOT NULL,
 started_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 finished_at timestamptz,
 outcome text NOT NULL DEFAULT 'started' CHECK(outcome IN ('started','succeeded','failed','cancelled','interrupted','incomplete')),
 error_code text CHECK(error_code ~ '^[a-z][a-z0-9_]{0,79}$'),
 http_status integer CHECK(http_status BETWEEN 100 AND 599),
 duration_ms bigint CHECK(duration_ms BETWEEN 0 AND 9007199254740991),
 first_output_ms bigint CHECK(first_output_ms BETWEEN 0 AND duration_ms),
 terminal_observed boolean NOT NULL DEFAULT false,
 input_tokens bigint CHECK(input_tokens BETWEEN 0 AND 9007199254740991),
 output_tokens bigint CHECK(output_tokens BETWEEN 0 AND 9007199254740991),
 total_tokens bigint CHECK(total_tokens BETWEEN 0 AND 9007199254740991),
 cache_read_tokens bigint CHECK(cache_read_tokens BETWEEN 0 AND 9007199254740991),
 cache_write_tokens bigint CHECK(cache_write_tokens BETWEEN 0 AND 9007199254740991),
 reasoning_tokens bigint CHECK(reasoning_tokens BETWEEN 0 AND 9007199254740991),
 input_semantics text NOT NULL DEFAULT 'unknown' CHECK(input_semantics IN ('includes_cache','excludes_cache','unknown')),
 usage_availability text NOT NULL DEFAULT 'unavailable' CHECK(usage_availability IN ('complete','partial','unavailable')),
 observation jsonb CHECK(jsonb_typeof(observation)='object'),
 FOREIGN KEY(run_id,tenant_id) REFERENCES issue_runs(id,tenant_id),
 FOREIGN KEY(binding_id,run_id) REFERENCES run_model_bindings(id,run_id),
 FOREIGN KEY(grant_id,binding_id,execution_id) REFERENCES model_access_grants(id,binding_id,execution_id),
 CHECK ((outcome='started' AND finished_at IS NULL AND observation IS NULL AND duration_ms IS NULL)
   OR (outcome<>'started' AND finished_at IS NOT NULL AND observation IS NOT NULL AND duration_ms IS NOT NULL)),
 CHECK (outcome<>'succeeded' OR terminal_observed),
 CHECK (usage_availability<>'complete' OR (terminal_observed AND input_tokens IS NOT NULL AND output_tokens IS NOT NULL)),
 CHECK (usage_availability<>'unavailable' OR num_nonnulls(input_tokens,output_tokens,total_tokens,cache_read_tokens,cache_write_tokens,reasoning_tokens)=0)
);
CREATE INDEX model_attempt_run_order ON model_request_attempts(tenant_id,run_id,started_at,id);
