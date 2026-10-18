package gateway

import (
	"net/http"
	"net/url"
	"strings"
)

// malformedSessionQuery checks syntax before ReverseProxy's Rewrite mode silently removes
// unparsable parameters. Only the strict session reads opt in; other routes retain their
// existing query contract. Cloud still owns parameter allowlists, duplicates, values and scope.
func malformedSessionQuery(r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}
	path, ok := strings.CutPrefix(r.URL.Path, "/api/v1/tenants/")
	if !ok {
		return false
	}
	parts := strings.Split(path, "/")
	if len(parts) < 2 || parts[0] == "" {
		return false
	}
	list := len(parts) == 2 && parts[1] == "agent-sessions"
	history := len(parts) == 6 && parts[1] == "issues" && parts[2] != "" && parts[3] == "runs" && parts[4] != "" && (parts[5] == "history" || parts[5] == "statistics" || parts[5] == "model-calls")
	if !list && !history {
		return false
	}
	_, err := url.ParseQuery(r.URL.RawQuery)
	return err != nil
}
