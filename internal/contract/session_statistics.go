package contract

import "strings"

// Keep nullable counters explicit: absent provider data is not a measured zero.
func modelStatisticsSchemas(s obj) {
	availability := func() obj { return enumeration("complete", "partial", "unavailable") }
	semantics := func() obj { return enumeration("includes_cache", "excludes_cache", "unknown") }
	s["MetricAggregate"] = object(obj{
		"value":      optional(obj{"type": "number", "minimum": 0, "maximum": 9007199254740991}),
		"knownCount": number(), "totalCount": number(), "availability": availability(),
	}, "value", "knownCount", "totalCount", "availability")
	tokenNames := strings.Fields("inputTokens outputTokens totalTokens cacheReadTokens cacheWriteTokens reasoningTokens")
	usage, usageAggregates := obj{}, obj{}
	for _, name := range tokenNames {
		usage[name] = optional(obj{"type": "integer", "format": "int64", "minimum": 0, "maximum": 9007199254740991})
		usageAggregates[name] = ref("MetricAggregate")
	}
	usage["inputSemantics"], usage["availability"] = semantics(), availability()
	s["ModelAttemptUsage"] = object(usage, append(tokenNames, "inputSemantics", "availability")...)
	asObject(s["ModelAttemptUsage"])["description"] = "Provider-reported counters only. OpenAI input includes cache reads; Anthropic input excludes cache read/write. Reasoning is an output subset. Never add every field together; a missing field stays null. Complete means a terminal observation with input/output, not that every optional field exists."
	s["SessionModelAttempt"] = object(obj{
		"id": uuid(), "startedAt": timestamp(), "finishedAt": optional(timestamp()),
		"modelId": str(), "protocol": enumeration("openai-completions", "anthropic-messages"),
		"streamed": boolean(), "outcome": enumeration("started", "succeeded", "failed", "cancelled", "interrupted", "incomplete"),
		"errorCode": optional(str()), "httpStatus": optional(number()), "durationMs": optional(number()),
		"firstOutputMs": optional(number()), "terminalObserved": boolean(), "usage": ref("ModelAttemptUsage"),
	}, strings.Fields("id startedAt finishedAt modelId protocol streamed outcome errorCode httpStatus durationMs firstOutputMs terminalObserved usage")...)
	counts := obj{}
	for _, name := range strings.Fields("total started succeeded failed cancelled interrupted incomplete") {
		counts[name] = number()
	}
	timing := obj{}
	for _, name := range strings.Fields("meanDurationMs p90DurationMs meanFirstOutputMs") {
		timing[name] = ref("MetricAggregate")
	}
	s["SessionModelStatistics"] = object(obj{
		"schemaVersion": obj{"type": "integer", "enum": []int{1}}, "source": enumeration("model_gateway"),
		"scope": enumeration("recorded_http_attempts"), "inputSemantics": semantics(),
		"counts":          object(counts, strings.Fields("total started succeeded failed cancelled interrupted incomplete")...),
		"firstRecordedAt": optional(timestamp()), "lastRecordedAt": optional(timestamp()),
		"usage":  object(usageAggregates, tokenNames...),
		"timing": object(timing, "meanDurationMs", "p90DurationMs", "meanFirstOutputMs"),
	}, strings.Fields("schemaVersion source scope inputSemantics counts firstRecordedAt lastRecordedAt usage timing")...)
}
