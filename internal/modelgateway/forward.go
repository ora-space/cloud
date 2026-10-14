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
	protocol := ""
	switch r.URL.Path {
	case "/runtime/openai/v1/chat/completions":
		protocol = "openai-completions"
	case "/runtime/anthropic/v1/messages":
		protocol = "anthropic-messages"
	}
	if r.Method != http.MethodPost || protocol == "" || r.URL.RawQuery != "" {
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
	ctx, cancel := context.WithCancelCause(r.Context())
	responseIO := newResponseIO(ctx, w)
	w = &guardedWriter{ResponseWriter: w, io: responseIO}
	finished := make(chan struct{})
	go s.watchAuthorization(ctx, digest, cancel, responseIO, finished)
	defer func() {
		cancel(errModelRequestComplete)
		<-finished
		if errors.Is(context.Cause(ctx), errModelRequestComplete) {
			responseIO.complete()
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
		out.Header.Set("Anthropic-Version", "2023-06-01")
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
	response, err := s.options.Upstream.Do(out)
	if err != nil {
		code := upstreamFailureCode(err)
		s.options.Logger.Warn("model upstream request rejected", zap.String("code", code))
		reject(w, 502, code)
		return
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		code := "model_upstream_failed"
		if response.StatusCode == 401 || response.StatusCode == 403 {
			code = "model_authentication_failed"
		}
		if response.StatusCode == 429 {
			code = "model_rate_limited"
		}
		reject(w, 502, code)
		return
	}
	mediaType, _, mediaErr := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if mediaErr != nil || (mediaType != "application/json" && mediaType != "text/event-stream") {
		reject(w, 502, "model_upstream_invalid_response")
		return
	}
	if mediaType == "application/json" {
		data, readErr := io.ReadAll(io.LimitReader(response.Body, maxModelRequestBytes+1))
		if readErr != nil || len(data) > maxModelRequestBytes || !json.Valid(data) {
			reject(w, 502, "model_upstream_invalid_response")
			return
		}
		if responseHasError(data) || !validProtocolJSON(data, protocol) {
			reject(w, 502, "model_upstream_failed")
			return
		}
		if err = responseIO.prepareWrite(); err != nil {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(response.StatusCode)
		_, err = w.Write(data)
		if err != nil {
			cancel(err)
			responseIO.interrupt()
		}
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	if err = responseIO.prepareWrite(); err != nil {
		return
	}
	w.WriteHeader(response.StatusCode)
	if err = forwardEvents(w, response.Body, protocol, responseIO); err != nil {
		cancel(err)
		responseIO.interrupt()
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
				cancel(err)
				if !errors.Is(context.Cause(ctx), errModelRequestComplete) {
					responseIO.interrupt()
				}
				return
			}
		}
	}
}
