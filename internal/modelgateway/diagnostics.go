package modelgateway

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
)

// transportCode is a safe reason, never an address, URL, response body or source diagnostic.
type transportCode string

const (
	endpointInvalid           transportCode = "model_endpoint_invalid"
	endpointForbidden         transportCode = "model_endpoint_forbidden"
	endpointResolutionFailed  transportCode = "model_endpoint_resolution_failed"
	endpointUnavailable       transportCode = "model_upstream_unavailable"
	endpointRedirectForbidden transportCode = "model_redirect_forbidden"
)

func (c transportCode) Error() string { return string(c) }

// upstreamFailureCode unwraps only typed categories; it never inspects provider-controlled text.
func upstreamFailureCode(err error) string {
	var code transportCode
	if errors.As(err, &code) {
		return string(code)
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return string(endpointResolutionFailed)
	}
	var certificate *tls.CertificateVerificationError
	if errors.As(err, &certificate) {
		return "model_upstream_tls_failed"
	}
	var timeout net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &timeout) && timeout.Timeout()) {
		return "model_upstream_timeout"
	}
	if errors.Is(err, context.Canceled) {
		return "model_request_cancelled"
	}
	return string(endpointUnavailable)
}
