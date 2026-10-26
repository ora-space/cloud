package modelgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"

	"go.uber.org/zap"
)

const maxModelRequestBytes = 8 << 20

var errModelRequestComplete = errors.New("model request complete")

func runtimeToken(r *http.Request, protocol string) string {
	if protocol == "anthropic-messages" {
		if token := r.Header.Get("X-Api-Key"); token != "" {
			return token
		}
	}
	v := r.Header.Get("Authorization")
	if !strings.HasPrefix(v, "Bearer ") {
		return ""
	}
	return strings.TrimPrefix(v, "Bearer ")
}

// ModelHandler forwards only the exact protocol endpoint and frozen model authorized for this
// runtime. Client-supplied authentication and URLs never reach the upstream model service.
func (s *Service) ModelHandler(w http.ResponseWriter, r *http.Request) {
	protocol, listing := runtimeModelRoute(r)
	if protocol == "" {
		reject(w, 404, "not_found")
		return
	}
	token := runtimeToken(r, protocol)
	if len(token) < 32 || len(token) > 256 {
		reject(w, 401, "invalid_model_token")
		return
	}
	digest := tokenDigest(token)
	grant, err := s.options.Store.ResolveModelGrant(r.Context(), digest)
	if err != nil {
		fail(w, err)
		return
	}
	if grant.Protocol != protocol {
		reject(w, 403, "model_protocol_forbidden")
		return
	}
	var protocolHeaders http.Header
	if protocol == "anthropic-messages" {
		var valid bool
		protocolHeaders, valid = anthropicProtocolHeaders(r.Header)
		if !valid {
			reject(w, 400, "model_protocol_header_forbidden")
			return
		}
	}
	if listing {
		frozenModelList(w, &grant)
		return
	}
	ctx, cancel := context.WithCancelCause(r.Context())
	var attempt *modelAttempt
	responseIO := newResponseIO(ctx, w)
	w = &guardedWriter{ResponseWriter: w, io: responseIO}
	finished := make(chan struct{})
	go s.watchAuthorization(ctx, digest, cancel, responseIO, finished)
	defer func() {
		if attempt != nil {
			attempt.result.DurationMS = attempt.elapsed()
		}
		cancel(errModelRequestComplete)
		<-finished
		if errors.Is(context.Cause(ctx), errModelRequestComplete) {
			responseIO.complete()
		}
		if attempt != nil {
			s.finishAttempt(ctx, attempt)
		}
	}()
	if err = responseIO.readDeadline(); err != nil {
		return
	}
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		reject(w, 400, "invalid_json")
		return
	}
	reader := http.MaxBytesReader(w, r.Body, maxModelRequestBytes)
	var request map[string]json.RawMessage
	decoder := json.NewDecoder(reader)
	if err = decoder.Decode(&request); err != nil {
		reject(w, 400, "invalid_json")
		return
	}
	var extra any
	if !errors.Is(decoder.Decode(&extra), io.EOF) {
		reject(w, 400, "invalid_json")
		return
	}
	responseIO.clearRead()
	var model string
	if err = json.Unmarshal(request["model"], &model); err != nil || model != grant.Model.ID {
		reject(w, 403, "model_forbidden")
		return
	}
	body, err := json.Marshal(request)
	if err != nil {
		reject(w, 400, "invalid_json")
		return
	}
	target, err := upstreamURL(grant.BaseURL, protocol)
	if err != nil {
		reject(w, 503, "model_endpoint_unavailable")
		return
	}
	if r.URL.RawQuery == "beta=true" {
		target += "?beta=true"
	}
	plaintext, err := s.options.Cipher.Open(grant.CredentialID, grant.CredentialKeyID, grant.Ciphertext)
	if err != nil {
		reject(w, 503, "model_credential_unavailable")
		return
	}
	defer clear(plaintext)
	out, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		reject(w, 503, "model_endpoint_unavailable")
		return
	}
	out.Header.Set("Content-Type", "application/json")
	out.Header.Set("Accept", "application/json, text/event-stream")
	if protocol == "anthropic-messages" {
		for key, values := range protocolHeaders {
			out.Header[key] = values
		}
	}
	switch grant.AuthMode {
	case "bearer":
		out.Header.Set("Authorization", "Bearer "+string(plaintext))
	case "x-api-key":
		out.Header.Set("X-Api-Key", string(plaintext))
	default:
		reject(w, 503, "model_credential_unavailable")
		return
	}
	var streamed bool
	_ = json.Unmarshal(request["stream"], &streamed)
	attempt, err = s.beginAttempt(ctx, &grant, streamed)
	if err != nil {
		reject(w, 503, "model_metrics_unavailable")
		return
	}
	response, err := s.options.Upstream.Do(out)
	if err != nil {
		code := upstreamFailureCode(err)
		attempt.end("failed", code)
		fields := []zap.Field{zap.String("code", code)}
		if detail := upstreamFailureDetail(err); detail != "" {
			fields = append(fields, zap.String("detail", detail))
		}
		s.options.Logger.Warn("model upstream request rejected", fields...)
		reject(w, 502, code)
		return
	}
	defer response.Body.Close()
	attempt.result.HTTPStatus = &response.StatusCode
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		code := "model_upstream_failed"
		if response.StatusCode == 401 || response.StatusCode == 403 {
			code = "model_authentication_failed"
		}
		if response.StatusCode == 429 {
			code = "model_rate_limited"
		}
		attempt.end("failed", code)
		reject(w, 502, code)
		return
	}
	mediaType, _, mediaErr := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if mediaErr != nil || (mediaType != "application/json" && mediaType != "text/event-stream") {
		attempt.end("failed", "model_upstream_invalid_response")
		reject(w, 502, "model_upstream_invalid_response")
		return
	}
	if mediaType == "application/json" {
		data, readErr := io.ReadAll(io.LimitReader(response.Body, maxModelRequestBytes+1))
		if readErr != nil || len(data) > maxModelRequestBytes || !json.Valid(data) {
			attempt.end("failed", "model_upstream_invalid_response")
			if readErr != nil {
				attempt.end("interrupted", "model_upstream_unavailable")
			}
			reject(w, 502, "model_upstream_invalid_response")
			return
		}
		if responseHasError(data) || !validProtocolJSON(data, protocol) {
			attempt.end("failed", "model_upstream_failed")
			reject(w, 502, "model_upstream_failed")
			return
		}
		attempt.output(attempt.observer.observeJSON(data))
		if err = responseIO.prepareWrite(); err != nil {
			attempt.end("interrupted", "model_downstream_unavailable")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(response.StatusCode)
		_, err = w.Write(data)
		if err != nil {
			attempt.end("interrupted", "model_downstream_unavailable")
			cancel(err)
			responseIO.interrupt()
		} else {
			attempt.end("succeeded", "")
		}
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	if err = responseIO.prepareWrite(); err != nil {
		attempt.end("interrupted", "model_downstream_unavailable")
		return
	}
	w.WriteHeader(response.StatusCode)
	if err = forwardObservedEvents(w, response.Body, protocol, responseIO, attempt.observeEvent); err != nil {
		var streamError *streamFailure
		if errors.As(err, &streamError) {
			attempt.end(streamError.outcome, streamError.code)
		} else {
			attempt.end("interrupted", "model_stream_unavailable")
		}
		cancel(err)
		responseIO.interrupt()
	} else if attempt.observer.terminal {
		attempt.end("succeeded", "")
	} else {
		attempt.end("incomplete", "model_stream_incomplete")
	}
}

// watchAuthorization has exactly one owner (its HTTP request), a cancellation path and a join.
// Every interval rereads durable authorization rather than trusting a process-local token cache.
func (s *Service) watchAuthorization(ctx context.Context, digest string, cancel context.CancelCauseFunc, responseIO *responseIO, finished chan<- struct{}) {
	defer close(finished)
	ticker := time.NewTicker(s.options.RecheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			if !errors.Is(context.Cause(ctx), errModelRequestComplete) {
				responseIO.interrupt()
			}
			return
		case <-ticker.C:
			if _, err := s.options.Store.ResolveModelGrant(ctx, digest); err != nil {
				cancel(errors.Join(errAuthorizationWatch, err))
				if !errors.Is(context.Cause(ctx), errModelRequestComplete) {
					responseIO.interrupt()
				}
				return
			}
		}
	}
}
