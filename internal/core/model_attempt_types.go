package core

// ModelAttemptUsage preserves the provider's accounting scope instead of treating context
// estimates or absent counters as measured zero. Counters are observed snapshots, never deltas.
// InputSemantics is includes_cache, excludes_cache, or unknown. Reasoning is a detail of the
// reported output and must not be added to it. Availability is complete, partial, or unavailable.
type ModelAttemptUsage struct {
	InputTokens      *int64 `json:"inputTokens"`
	OutputTokens     *int64 `json:"outputTokens"`
	TotalTokens      *int64 `json:"totalTokens"`
	CacheReadTokens  *int64 `json:"cacheReadTokens"`
	CacheWriteTokens *int64 `json:"cacheWriteTokens"`
	ReasoningTokens  *int64 `json:"reasoningTokens"`
	InputSemantics   string `json:"inputSemantics"`
	Availability     string `json:"availability"`
}

// ModelAttemptFinish is a bounded, content-free observation of one outbound HTTP attempt.
// DurationMS and FirstOutputMS are measured by the gateway's monotonic clock; first output
// excludes keepalives and empty role/usage chunks. TerminalObserved records protocol completion
// independently of downstream delivery and usage availability. Finalization is idempotent.
type ModelAttemptFinish struct {
	Outcome          string            `json:"outcome"`
	ErrorCode        string            `json:"errorCode"`
	HTTPStatus       *int              `json:"httpStatus"`
	DurationMS       int64             `json:"durationMs"`
	FirstOutputMS    *int64            `json:"firstOutputMs"`
	TerminalObserved bool              `json:"terminalObserved"`
	Usage            ModelAttemptUsage `json:"usage"`
}
