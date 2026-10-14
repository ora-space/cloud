package cloneexec

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// The refusal codes the Credential Service reports for a redemption. They are duplicated here rather
// than imported so the executor depends on the Store's wire vocabulary and not on the Store's
// implementation: the two are separate components with separate binaries, and the code the executor
// reports has to keep meaning the same thing if the Store is ever replaced behind its endpoints (the
// Store decision's D1).
const (
	// ReasonCodeAuthorizationUnverifiable is the code a Store with no verification key answers with.
	// It means the deployment never established the redemption boundary, which is a deployment fault
	// rather than a refusal of this clone.
	ReasonCodeAuthorizationUnverifiable = "authorization_verification_unavailable"
	// ReasonCodeAuthorizationInvalid reports an authorization that is not a Cloud-issued one.
	ReasonCodeAuthorizationInvalid = "authorization_invalid"
	// ReasonCodeAuthorizationExpired reports an authorization whose lifetime closed before redemption.
	ReasonCodeAuthorizationExpired = "authorization_expired"
	// ReasonCodeAuthorizationBinding reports an authorization that does not match what was presented.
	ReasonCodeAuthorizationBinding = "authorization_binding_mismatch"
	// ReasonCodeAuthorizationConsumed reports an authorization that was already spent.
	ReasonCodeAuthorizationConsumed = "authorization_consumed"
	// ReasonCodeVersionUnavailable reports a bound credential version the Store can no longer deliver.
	ReasonCodeVersionUnavailable = "credential_version_unavailable"
	// ReasonCodeRevoked reports a credential the Store has destroyed.
	ReasonCodeRevoked = "credential_revoked"
	// ReasonCodeNotFound reports a credential the Store does not hold.
	ReasonCodeNotFound = "credential_not_found"
	// ReasonCodeStoreUnavailable reports that the Store answered without a usable reason code.
	ReasonCodeStoreUnavailable = "store_unavailable"
)

// StoreRefusal is a refusal the Credential Service itself reported.
//
// It is separate from a transport error because the two call for different responses: a refusal is an
// outcome of this clone and is reported to the caller as such, while an unreachable Store is an
// infrastructure failure that must not be dressed up as a decision about the credential.
type StoreRefusal struct {
	// Code is the Store's stable reason code.
	Code string
	// Status is the HTTP status the Store answered with.
	Status int
}

// Error reports the refusal without carrying any response body: peer text travels into logs, and
// nothing the Store returns beyond the code is safe to repeat.
func (r *StoreRefusal) Error() string {
	return fmt.Sprintf("credential service refused the redemption with %q (status %d)", r.Code, r.Status)
}

// ReasonCode names the refusal in a form the executor can map to its own outcome.
func (r *StoreRefusal) ReasonCode() string { return r.Code }

// StoreClientConfig wires the executor's redemption path.
type StoreClientConfig struct {
	// Endpoint is the Credential Service's HTTPS base URL. Plain HTTP is refused: the token crosses
	// this connection, and the client certificate is what carries the `broker-resolver` identity.
	Endpoint string
	// TLS must present the executor's `broker-resolver` certificate and trust the Store's certificate
	// authority. The executor holds no other identity's key.
	TLS *tls.Config
	// Timeout bounds one redemption. It is deliberately shorter than a clone: a redemption that has
	// not answered by then is an unreachable Store, and waiting longer only delays a failure the
	// caller has to see.
	Timeout time.Duration
}

// DefaultRedemptionTimeout bounds one redemption against the Credential Service.
const DefaultRedemptionTimeout = 10 * time.Second

// maxStoreResponse bounds a redemption response. The only variable-length field is a token, already
// bounded by the Store, so anything larger is a broken peer rather than a large secret.
const maxStoreResponse = 16 << 10

// StoreClient is the executor's `broker-resolver` client.
//
// It has exactly one method, and that is the design: the credential-executing identity may redeem a
// secret behind a single-use authorization and may do nothing else. It cannot write, rotate, revoke,
// or enumerate, because the deployment's capability table grants it `resolve` alone and this client
// has no code for the others to call.
//
// There is deliberately no retry. A redemption that times out may have consumed the authorization, so
// re-sending it would either fail as already-consumed or, worse, look like a transient error and hide
// that the secret may already have been delivered. Recovery is a new authorization, which is what the
// Store's `consumed_not_delivered` outcome is for.
type StoreClient struct {
	endpoint string
	http     *http.Client
}

// NewStoreClient validates the configuration and returns a ready redemption client.
func NewStoreClient(cfg StoreClientConfig) (*StoreClient, error) {
	parsed, err := url.Parse(strings.TrimSpace(cfg.Endpoint))
	if err != nil || parsed.Host == "" {
		return nil, errors.New("credential service endpoint is not a valid URL")
	}
	if parsed.Scheme != "https" {
		return nil, errors.New("credential service endpoint must use https")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("credential service endpoint must not carry a query or fragment")
	}
	if cfg.TLS == nil || len(cfg.TLS.Certificates) == 0 || cfg.TLS.RootCAs == nil {
		return nil, errors.New("credential service requires a client certificate and a trusted certificate authority")
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultRedemptionTimeout
	}
	return &StoreClient{
		endpoint: strings.TrimSuffix(parsed.String(), "/"),
		http:     &http.Client{Timeout: timeout, Transport: &http.Transport{TLSClientConfig: cfg.TLS}},
	}, nil
}

// Credential is what one redemption delivers: the token, and the version it came from.
//
// It is the executor's whole view of a secret, and it is deliberately the only shape a credential
// takes inside this package — there is no type here that can hold a credential id together with a
// token, so no log line and no error value can carry both.
type Credential struct {
	Token   string `json:"token"`
	Version int64  `json:"version"`
}

// RedemptionClient is the boundary toward the Credential Service, declared here because this is where
// it is consumed.
//
// It is an interface rather than the concrete client so the executor's own behavior — the ordering of
// verification, redemption, materialization, cloning and destruction — can be exercised without a
// live Store, and so a deployment can put the Store behind its own endpoint (the Store decision's D1)
// without the executor changing. It stays one method wide: the credential-executing identity may
// redeem behind a single-use authorization and may do nothing else.
type RedemptionClient interface {
	// Resolve redeems one authorization for the credential it names. It returns a refusal rather than
	// an empty credential when the Store declines, and an error rather than a refusal when the Store
	// cannot be reached.
	Resolve(ctx context.Context, credentialID, authorization string, runtime Runtime) (Credential, error)
}

// MirrorRedemptionClient is the redemption boundary for the mirror purpose.
//
// It is a second interface rather than a second method on RedemptionClient because the two carry
// different claims. A clone's redemption states the runtime it is executing under, and the Store
// compares that statement against the authorization's binding. A mirror preparation has no runtime to
// state: it runs on no Node, and what the Store checks instead is the service identity on the client
// certificate against the `executorIdentity` the authorization named. A client with one method taking
// both shapes would have to accept a runtime it is not going to check, which is exactly how a request
// field becomes decorative.
//
// Keeping them separate also keeps each executor's dependency narrow: the clone path cannot call the
// mirror endpoint and the mirror path cannot state a runtime, because neither has the method.
type MirrorRedemptionClient interface {
	// ResolveMirror redeems one `mirror_prepare` authorization for the credential it names. The
	// caller is identified by the certificate this client presents, never by anything in the call.
	ResolveMirror(ctx context.Context, credentialID, authorization string) (Credential, error)
}

// PushRedemptionClient is the redemption boundary for the push purpose.
//
// It is a third interface rather than a third method on either of the others for the reason
// MirrorRedemptionClient is a second one: the claims differ, and the Store checks a different set for
// each. A push states neither a runtime nor a purpose — the purpose is in the signed authorization and
// the presenter's identity is on the client certificate — so its request is the authorization alone.
//
// It reaches the *same* route a clone's redemption does. The push decision (D5) settled that: the
// authorization's operation is what tells the two apart at the Store, so a second URL would be a second
// surface to authenticate for no capability the first one lacks. The separation this interface provides
// is on the executor's side, where it belongs — the push path cannot state a runtime it is not going to
// have checked, and a client holding only this method cannot present a push authorization to the clone
// route or the reverse.
type PushRedemptionClient interface {
	// ResolvePush redeems one `push` authorization for the credential it names. The caller is identified
	// by the certificate this client presents, never by anything in the call.
	ResolvePush(ctx context.Context, credentialID, authorization string) (Credential, error)
}

// Resolve redeems one authorization for the token it names.
//
// The request carries the authorization and the executor's own declaration of which runtime it is.
// The Store checks that declaration against the authorization's binding, so this is a fencing value
// rather than an identity claim: what proves the caller is the credential-executing identity is the
// mutual-TLS certificate, and what proves the authorization is genuine is Cloud's signature.
func (c *StoreClient) Resolve(ctx context.Context, credentialID, authorization string, runtime Runtime) (Credential, error) {
	return c.redeem(ctx, credentialID, "/resolve", "redemption", map[string]any{
		"authorization":     authorization,
		"nodeId":            runtime.NodeID,
		"nodeInstanceId":    runtime.NodeInstanceID,
		"nodeIncarnationId": runtime.NodeIncarnationID,
		"runtimeGeneration": runtime.RuntimeGeneration,
		"controlEpoch":      runtime.ControlEpoch,
	})
}

// ResolveMirror redeems one `mirror_prepare` authorization for the token it names.
//
// The request is the authorization and nothing else, and that is the whole difference from `Resolve`:
// a clone states the runtime it is executing under, while a mirror preparation has none to state and
// the Store takes the presenter's identity from the client certificate instead. Sending a runtime field
// here would be sending a value the Store would have to ignore, which is how a request field becomes
// something a later reader believes is checked.
//
// It is the same endpoint vocabulary — the same refusal codes, the same single-use semantics, the
// authorization spent exactly once by the Store — reached on the Store's mirror route.
func (c *StoreClient) ResolveMirror(ctx context.Context, credentialID, authorization string) (Credential, error) {
	return c.redeem(ctx, credentialID, "/resolve-mirror", "mirror redemption",
		map[string]any{"authorization": authorization})
}

// ResolvePush redeems one `push` authorization for the token it names.
//
// The request is the authorization and nothing else, and it is sent to the same route a clone's
// redemption uses, because the authorization is what says which purpose it is for: the Store verifies
// the signature, reads the operation out of the signed payload, and refuses anything that is not a push
// on this path. The executor does not declare a purpose here for the same reason it does not declare
// its own identity — a value the other side has to compare against a signed one can only ever be
// redundant or wrong, and a wrong one would be a refusal this executor caused.
//
// No runtime is stated either. A push runs on no Node and holds no generation to fence; the executor
// identity on the client certificate is what the Store compares against the authorization's binding.
func (c *StoreClient) ResolvePush(ctx context.Context, credentialID, authorization string) (Credential, error) {
	return c.redeem(ctx, credentialID, "/resolve", "push redemption",
		map[string]any{"authorization": authorization})
}

// redeem performs one redemption request and reads the Store's answer.
//
// The three purposes reach the Store with different request bodies and, for the mirror purpose, on a
// different route, but the answer vocabulary is one vocabulary, and reading it is where a mistake is
// expensive: a non-200 whose body cannot be read, or whose code is blank, has to become the Store's own
// unreachable code rather than an empty refusal. An empty code would travel up as an unrecognized
// failure, and the caller would then have to decide what an unrecognized refusal means — which is
// exactly the decision that lets an infrastructure failure be recorded as a decision about a
// credential. One copy of that rule, applied to all three purposes.
func (c *StoreClient) redeem(ctx context.Context, credentialID, path, what string, body map[string]any) (Credential, error) {
	encoded, err := json.Marshal(body)
	if err != nil {
		return Credential{}, fmt.Errorf("encode %s request: %w", what, err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.endpoint+"/v1/credentials/"+url.PathEscape(credentialID)+path, bytes.NewReader(encoded))
	if err != nil {
		return Credential{}, fmt.Errorf("build %s request: %w", what, err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := c.http.Do(request)
	if err != nil {
		// The URL is in the error and a URL never carries a token, because tokens only ever travel in a
		// response body. The authorization is in the request body and is not repeated here.
		return Credential{}, fmt.Errorf("credential service is unreachable: %w", err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxStoreResponse))
	if err != nil {
		return Credential{}, fmt.Errorf("read %s response: %w", what, err)
	}
	if response.StatusCode != http.StatusOK {
		var refusal struct {
			Code string `json:"code"`
		}
		if err := json.Unmarshal(raw, &refusal); err != nil || refusal.Code == "" {
			refusal.Code = ReasonCodeStoreUnavailable
		}
		return Credential{}, &StoreRefusal{Code: refusal.Code, Status: response.StatusCode}
	}
	var resolved Credential
	if err := json.Unmarshal(raw, &resolved); err != nil {
		return Credential{}, errors.New("credential service returned an unreadable response")
	}
	if strings.TrimSpace(resolved.Token) == "" {
		return Credential{}, errors.New("credential service returned no credential")
	}
	return resolved, nil
}
