package gateway

import (
	"fmt"
	"net/url"
	"strings"
)

// IDaaSEndpoint validates a deployment-owned override without reflecting its value in errors.
// Queries are excluded so credentials and protocol parameters can only be constructed by the adapter.
func IDaaSEndpoint(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || strings.TrimSpace(raw) != raw || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(raw, "#") || u.Opaque != "" {
		return nil, fmt.Errorf("IDaaS endpoint must be an absolute HTTPS URL without credentials, query or fragment")
	}
	return u, nil
}
