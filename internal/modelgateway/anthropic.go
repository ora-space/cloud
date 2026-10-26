package modelgateway

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/wanglongan587/cloud/internal/core"
)

const anthropicVersion = "2023-06-01"

// These protocol capabilities were observed with the pinned Claude 2.1.303 CLI, including when
// it is started through Agent SDK 0.3.303 / ACP adapter 0.94.0, and, for Claude models over
// Anthropic Messages, the pinned OpenCode 1.19.3 CLI. A CLI, SDK or adapter upgrade must
// exercise its requests before extending this set.
var claudeBetaHeaders = map[string]bool{
	"claude-code-20250219":                     true,
	"interleaved-thinking-2025-05-14":          true,
	"thinking-token-count-2026-05-13":          true,
	"context-management-2025-06-27":            true,
	"prompt-caching-scope-2026-01-05":          true,
	"mid-conversation-tool-changes-2026-07-01": true,
	"effort-2025-11-24":                        true,
	"structured-outputs-2025-12-15":            true,
	"thinking-display-updates-2026-08-18":      true,
	"per-turn-control-2026-07-01":              true,
	"thinking-binding-controls-2026-08-01":     true,
	"structured-outputs-2025-11-13":            true,
}

// runtimeModelRoute accepts only inference and the frozen binding's local model listing.
// The listing is not upstream discovery and cannot expand the authorized model set.
func runtimeModelRoute(r *http.Request) (protocol string, listing bool) {
	if r.URL.ForceQuery || r.URL.RawPath != "" {
		return "", false
	}
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/runtime/openai/v1/chat/completions" && r.URL.RawQuery == "":
		return "openai-completions", false
	case r.Method == http.MethodPost && r.URL.Path == "/runtime/anthropic/v1/messages" &&
		(r.URL.RawQuery == "" || r.URL.RawQuery == "beta=true"):
		return "anthropic-messages", false
	case r.Method == http.MethodGet && r.URL.Path == "/runtime/anthropic/v1/models" &&
		(r.URL.RawQuery == "" || r.URL.RawQuery == "limit=1000"):
		return "anthropic-messages", true
	default:
		return "", false
	}
}

// anthropicProtocolHeaders owns the bounded non-authentication protocol surface. Generic
// client headers are dropped by the caller; an unknown Anthropic extension fails explicitly.
func anthropicProtocolHeaders(in http.Header) (http.Header, bool) {
	out := http.Header{"Anthropic-Version": []string{anthropicVersion}}
	for key, values := range in {
		name := strings.ToLower(key)
		if !strings.HasPrefix(name, "anthropic-") {
			continue
		}
		if len(values) != 1 {
			return nil, false
		}
		value := values[0]
		switch name {
		case "anthropic-version":
			if value != anthropicVersion {
				return nil, false
			}
		case "anthropic-dangerous-direct-browser-access":
			// The CLI sends this SDK marker. This server-side proxy never forwards it.
			if value != "true" {
				return nil, false
			}
		case "anthropic-beta":
			if value == "" || len(value) > 1024 {
				return nil, false
			}
			seen := map[string]bool{}
			betas := strings.Split(value, ",")
			for i, beta := range betas {
				beta = strings.TrimSpace(beta)
				if !claudeBetaHeaders[beta] || seen[beta] {
					return nil, false
				}
				seen[beta], betas[i] = true, beta
			}
			out.Set("Anthropic-Beta", strings.Join(betas, ","))
		default:
			return nil, false
		}
	}
	return out, true
}

// frozenModelList returns only the model already authorized by ResolveModelGrant. It neither
// decrypts a key nor performs an upstream attempt, and always rechecks current authorization.
func frozenModelList(w http.ResponseWriter, grant *core.ModelGrant) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(core.Object{
		"data":     []core.Object{{"type": "model", "id": grant.Model.ID, "display_name": grant.Model.Name}},
		"has_more": false, "first_id": grant.Model.ID, "last_id": grant.Model.ID,
	})
}
