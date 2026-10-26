package egress

import "net/http"

// HealthMetadata exposes only schema and content identity on readiness responses.
// Deployment verification compares both processes; it is not an activation protocol.
func HealthMetadata(next http.Handler, digest string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/healthz" {
			w.Header().Set("X-Ora-Egress-Schema", "1")
			w.Header().Set("X-Ora-Egress-Digest", digest)
		}
		next.ServeHTTP(w, r)
	})
}
