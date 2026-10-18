package core

import (
	"context"
	"strings"
)

// Session model reads have no identity provisioning or runtime lifecycle side effects.
func (s *Store) sessionModelRead(ctx context.Context, r *PublicRequest) (Object, int, error) {
	out, err := s.transact(ctx, func(t *transaction) Object {
		uid := sessionReadIdentity(t, r.Identity)
		membership(t, r.TenantID, uid, false)
		issue(t, r.TenantID, r.IssueID)
		require(validID(r.RunID), 404, "not_found")
		run := t.one("SELECT executor_type FROM issue_runs WHERE id=$1 AND tenant_id=$2 AND issue_id=$3 AND deleted_at IS NULL", r.RunID, r.TenantID, r.IssueID)
		require(run != nil && run.S("executorType") == "agent", 404, "not_found")
		if strings.HasSuffix(r.Path, "/model-calls") {
			return sessionModelCalls(t, r)
		}
		return sessionModelStatistics(t, r)
	})
	return out, 200, err
}

func sessionModelCalls(t *transaction, r *PublicRequest) Object {
	require(r.Limit >= 0 && r.Limit <= 100 && r.Before == "" && (r.After == "" || validID(r.After)), 400, "invalid_pagination")
	query := `SELECT id,started_at,finished_at,model_id,protocol,streamed,outcome,error_code,http_status,
	 duration_ms,first_output_ms,terminal_observed,jsonb_build_object('inputTokens',input_tokens,
	 'outputTokens',output_tokens,'totalTokens',total_tokens,'cacheReadTokens',cache_read_tokens,
	 'cacheWriteTokens',cache_write_tokens,'reasoningTokens',reasoning_tokens,
	 'inputSemantics',input_semantics,'availability',usage_availability) AS usage
	 FROM model_request_attempts WHERE tenant_id=$1 AND run_id=$2`
	args := []any{r.TenantID, r.RunID}
	if r.After != "" {
		anchor := t.one("SELECT started_at FROM model_request_attempts WHERE id=$1 AND tenant_id=$2 AND run_id=$3", r.After, r.TenantID, r.RunID)
		require(anchor != nil, 400, "invalid_pagination")
		args = append(args, anchor["startedAt"], r.After)
		query += " AND (started_at,id)>($3::timestamptz,$4::uuid)"
	}
	return window(t, query+" ORDER BY started_at,id", args, r)
}

type modelMetricColumn struct {
	name, column, operation string
}

var modelUsageColumns = []modelMetricColumn{
	{"inputTokens", "input_tokens", "sum"},
	{"outputTokens", "output_tokens", "sum"},
	{"totalTokens", "total_tokens", "sum"},
	{"cacheReadTokens", "cache_read_tokens", "sum"},
	{"cacheWriteTokens", "cache_write_tokens", "sum"},
	{"reasoningTokens", "reasoning_tokens", "sum"},
}

var modelTimingColumns = []modelMetricColumn{
	{"meanDurationMs", "duration_ms", "avg"},
	{"p90DurationMs", "duration_ms", "p90"},
	{"meanFirstOutputMs", "first_output_ms", "avg"},
}

// Aggregate in PostgreSQL across all recorded attempts, never the visible page or archived
// message count. Each counter carries its own coverage; unavailable values stay SQL NULL.
func sessionModelStatistics(t *transaction, r *PublicRequest) Object {
	parts := []string{
		"COUNT(*) AS total", "MIN(started_at) AS first_recorded_at", "MAX(started_at) AS last_recorded_at",
		"CASE WHEN COUNT(DISTINCT input_semantics)=1 THEN MIN(input_semantics) ELSE 'unknown' END AS input_semantics",
	}
	states := []string{"started", "succeeded", "failed", "cancelled", "interrupted", "incomplete"}
	for _, state := range states {
		parts = append(parts, "COUNT(*) FILTER (WHERE outcome='"+state+"') AS "+state)
	}
	for _, cols := range [][]modelMetricColumn{modelUsageColumns, modelTimingColumns} {
		for _, col := range cols {
			operation := col.operation + "(" + col.column + ")"
			if col.operation == "p90" {
				operation = "percentile_disc(0.9) WITHIN GROUP (ORDER BY " + col.column + ")"
			}
			if col.operation == "sum" {
				parts = append(parts, "COUNT("+col.column+") FILTER (WHERE terminal_observed AND usage_availability='complete') AS \""+col.name+"Final\"")
			}
			// Values beyond JavaScript's exact integer range remain unavailable rather than rounded.
			parts = append(parts, "COUNT("+col.column+") AS \""+col.name+"Known\"",
				"CASE WHEN "+operation+"<=9007199254740991 THEN "+operation+" ELSE NULL END AS \""+col.name+"Value\"")
		}
	}
	row := t.one("SELECT "+strings.Join(parts, ",")+" FROM model_request_attempts WHERE tenant_id=$1 AND run_id=$2", r.TenantID, r.RunID)
	total := row.N("total")
	counts := Object{"total": total}
	for _, state := range states {
		counts[state] = row.N(state)
	}
	return Object{
		"schemaVersion": 1, "source": "model_gateway", "scope": "recorded_http_attempts",
		"inputSemantics": row.S("inputSemantics"), "counts": counts,
		"firstRecordedAt": row["firstRecordedAt"], "lastRecordedAt": row["lastRecordedAt"],
		"usage": modelMetricGroup(row, total, modelUsageColumns), "timing": modelMetricGroup(row, total, modelTimingColumns),
	}
}

func modelMetricGroup(row Object, total int64, columns []modelMetricColumn) Object {
	group := Object{}
	for _, col := range columns {
		known, value := row.N(col.name+"Known"), row[col.name+"Value"]
		availability := "unavailable"
		if value != nil && known > 0 {
			availability = "partial"
			if known == total && (col.operation != "sum" || row.N(col.name+"Final") == total) {
				availability = "complete"
			}
		}
		group[col.name] = Object{"value": value, "knownCount": known, "totalCount": total, "availability": availability}
	}
	return group
}
