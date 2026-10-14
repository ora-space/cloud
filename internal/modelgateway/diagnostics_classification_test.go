package modelgateway

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/url"
	"testing"
)

// Classification survives HTTP wrapping and never derives a code from arbitrary diagnostic text.
func TestUpstreamFailureClassification(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		code string
	}{
		{"certificate", &tls.CertificateVerificationError{Err: errors.New("private TLS detail")}, "model_upstream_tls_failed"},
		{"timeout", context.DeadlineExceeded, "model_upstream_timeout"},
		{"cancelled", context.Canceled, "model_request_cancelled"},
		{"DNS", &net.DNSError{Err: "private resolver detail"}, "model_endpoint_resolution_failed"},
		{"redirect", endpointRedirectForbidden, "model_redirect_forbidden"},
		{"unknown", errors.New("private upstream detail"), "model_upstream_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := &url.Error{Op: "Post", URL: "https://provider.example.invalid", Err: tc.err}
			if code := upstreamFailureCode(err); code != tc.code {
				t.Fatalf("code=%s, want %s", code, tc.code)
			}
		})
	}
}
