package credentialstore

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/internal/cloneauth"
)

// mirrorBinding returns a valid `mirror_prepare` binding for one credential version at the fixture's
// current instant.
//
// It is deliberately not derived from the clone fixture's binding: the two purposes carry disjoint
// identity fields, so a mirror binding built by clearing fields would hide exactly the difference
// these tests exist to exercise. Everything a mirror binding shares with a clone — the credential, the
// version, the repository, the generation — is set to the same value, so a case that fails does so
// because of the purpose and not because some unrelated field disagreed.
func (f *resolveFixture) mirrorBinding(credentialID string, version int64) cloneauth.Binding {
	issued := f.clock.Now()
	return cloneauth.Binding{
		AuthorizationID:       testAuthorizationID(1),
		Operation:             cloneauth.OperationMirrorPrepare,
		TenantID:              "tenant-1",
		OwnerUserID:           "user-1",
		ProjectID:             "project-1",
		WorkspaceID:           "workspace-1",
		OperationID:           "operation-1",
		PreparationGeneration: 1,
		ExecutorIdentity:      testResolverIdentity,
		CredentialID:          credentialID,
		CredentialVersion:     version,
		Repository:            "https://github.com/ora-space/private",
		Ref:                   "refs/heads/main",
		Capabilities:          []string{string(cloneauth.CapabilityClone)},
		RuntimeGeneration:     testRuntimeGeneration,
		ControlEpoch:          testControlEpoch,
		IssuedAt:              issued,
		ExpiresAt:             issued.Add(time.Minute),
	}
}

// mirrorRequest is what a correctly behaved executor presents: the token, and nothing else.
func mirrorRequest(authorization string) MirrorResolveRequest {
	return MirrorResolveRequest{Authorization: authorization}
}

// TestResolveMirrorDeliversTheBoundToken proves the happy path, and that the consumption record says
// which purpose was spent and how the caller was identified.
func TestResolveMirrorDeliversTheBoundToken(t *testing.T) {
	fixture := newResolveFixture(t)
	id := testCredentialID(1)
	storeCredential(t, fixture.store, id, "ghp_mirror_token")
	authorization := fixture.issue(t, fixture.mirrorBinding(id, 1))

	resolved, err := fixture.resolver.ResolveMirror(context.Background(), id, testResolverIdentity, mirrorRequest(authorization))
	if err != nil {
		t.Fatalf("ResolveMirror: %v", err)
	}
	if resolved.Token != "ghp_mirror_token" {
		t.Fatalf("token = %q, want %q", resolved.Token, "ghp_mirror_token")
	}
	if resolved.Version != 1 {
		t.Fatalf("version = %d, want 1", resolved.Version)
	}
	record, found := readConsumption(t, fixture, testAuthorizationID(1))
	if !found {
		t.Fatal("no consumption record was written")
	}
	if record.Result != consumptionDelivered {
		t.Fatalf("consumption result = %q, want %q", record.Result, consumptionDelivered)
	}
	if record.Operation != cloneauth.OperationMirrorPrepare {
		t.Fatalf("consumption record names operation %q, want %q", record.Operation, cloneauth.OperationMirrorPrepare)
	}
	if record.ExecutorIdentity != testResolverIdentity {
		t.Fatalf("consumption record names executor %q, want %q", record.ExecutorIdentity, testResolverIdentity)
	}
	// A mirror preparation runs on no Node, so the record must not claim a runtime: a record that
	// named one would be evidence of a check that did not happen.
	if record.NodeID != "" || record.NodeInstanceID != "" || record.NodeIncarnationID != "" ||
		record.RuntimeGeneration != 0 || record.ControlEpoch != 0 {
		t.Fatalf("a mirror consumption record claims a runtime: %+v", record)
	}
}

// TestTheTwoPurposesCannotBeSpentOnEachOther is the separation from the Store's side, in both
// directions.
//
// The clone direction is the one that matters most, and it is not hypothetical: a mirror binding
// carries no Node runtime, so a caller presenting empty runtime values would match it field for field
// if the credential and the version agreed. Without the operation check, the single use a preparation
// is given would be consumed on a clone.
func TestTheTwoPurposesCannotBeSpentOnEachOther(t *testing.T) {
	t.Run("a mirror authorization on the clone path", func(t *testing.T) {
		fixture := newResolveFixture(t)
		id := testCredentialID(1)
		storeCredential(t, fixture.store, id, "ghp_mirror_token")
		authorization := fixture.issue(t, fixture.mirrorBinding(id, 1))

		// The request is built by the clone fixture's own helper, so it declares the runtime a clone
		// would — and it is refused before any of those values are compared.
		_, err := fixture.resolver.Resolve(context.Background(), id, fixture.request(authorization))
		if !errors.Is(err, ErrAuthorizationPurpose) {
			t.Fatalf("a mirror authorization on the clone path returned %v, want %v", err, ErrAuthorizationPurpose)
		}
		assertUnspent(t, fixture)
	})
	t.Run("a clone authorization on the mirror path", func(t *testing.T) {
		fixture := newResolveFixture(t)
		id := testCredentialID(1)
		storeCredential(t, fixture.store, id, "ghp_clone_token")
		authorization := fixture.issue(t, fixture.binding(id, 1))

		_, err := fixture.resolver.ResolveMirror(context.Background(), id, testResolverIdentity, mirrorRequest(authorization))
		if !errors.Is(err, ErrAuthorizationPurpose) {
			t.Fatalf("a clone authorization on the mirror path returned %v, want %v", err, ErrAuthorizationPurpose)
		}
		assertUnspent(t, fixture)
	})
}

// TestResolveMirrorRefusesAnIdentityTheBindingDidNotName is the identity check, and the reason the
// request carries no identity of its own.
//
// The value compared against the binding comes from the transport's reading of the client certificate,
// so a caller cannot choose it. The second case proves the comparison is not satisfiable by omission:
// a caller whose identity could not be established must not match a binding by presenting nothing.
func TestResolveMirrorRefusesAnIdentityTheBindingDidNotName(t *testing.T) {
	cases := []struct {
		name      string
		presented string
	}{
		{"another service identity", testIngressIdentity},
		{"an identity nobody holds", "spiffe://ora/node/someone-else"},
		{"no identity at all", ""},
		{"whitespace", "   "},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newResolveFixture(t)
			id := testCredentialID(1)
			storeCredential(t, fixture.store, id, "ghp_mirror_token")
			authorization := fixture.issue(t, fixture.mirrorBinding(id, 1))

			_, err := fixture.resolver.ResolveMirror(context.Background(), id, testCase.presented, mirrorRequest(authorization))
			if !errors.Is(err, ErrAuthorizationBinding) {
				t.Fatalf("presented %q returned %v, want %v", testCase.presented, err, ErrAuthorizationBinding)
			}
			assertUnspent(t, fixture)
		})
	}
}

// TestResolveMirrorRefusesWhatItCannotDeliver covers every refusal that must leave the authorization
// unspent, so a caller can retry the same token once the cause is repaired.
func TestResolveMirrorRefusesWhatItCannotDeliver(t *testing.T) {
	cases := []struct {
		name    string
		prepare func(t *testing.T, fixture *resolveFixture, id string) string
		wantErr error
		// retention shortens the Store's window for the one case that has to outlast a rotation.
		// An authorization lives at most fifteen minutes, so a closed retention window is only
		// reachable at all if the fixture's window is shorter than that.
		retention time.Duration
	}{
		{
			name: "an authorization for another credential",
			prepare: func(t *testing.T, fixture *resolveFixture, id string) string {
				storeCredential(t, fixture.store, id, "ghp_mirror_token")
				return fixture.issue(t, fixture.mirrorBinding(testCredentialID(2), 1))
			},
			wantErr: ErrAuthorizationBinding,
		},
		{
			name: "an expired authorization",
			prepare: func(t *testing.T, fixture *resolveFixture, id string) string {
				storeCredential(t, fixture.store, id, "ghp_mirror_token")
				binding := fixture.mirrorBinding(id, 1)
				token := fixture.issue(t, binding)
				fixture.clock.Advance(2 * time.Minute)
				return token
			},
			wantErr: ErrAuthorizationExpired,
		},
		{
			name: "a credential the Store does not hold",
			prepare: func(t *testing.T, fixture *resolveFixture, id string) string {
				return fixture.issue(t, fixture.mirrorBinding(id, 1))
			},
			wantErr: ErrNotFound,
		},
		{
			name: "a revoked credential",
			prepare: func(t *testing.T, fixture *resolveFixture, id string) string {
				storeCredential(t, fixture.store, id, "ghp_mirror_token")
				token := fixture.issue(t, fixture.mirrorBinding(id, 1))
				if err := fixture.store.Revoke(context.Background(), id); err != nil {
					t.Fatalf("Revoke: %v", err)
				}
				return token
			},
			wantErr: ErrRevoked,
		},
		{
			name: "a version the Store can no longer read",
			prepare: func(t *testing.T, fixture *resolveFixture, id string) string {
				storeCredential(t, fixture.store, id, "ghp_first")
				if _, err := fixture.store.Rotate(context.Background(), id, "ghp_second"); err != nil {
					t.Fatalf("Rotate: %v", err)
				}
				// The authorization names the version that was current when it was issued; the clock then
				// moves past the retention window, so the Store can no longer open it. This is the
				// "version mismatch" outcome D7 requires to be distinct from "revoked".
				token := fixture.issue(t, fixture.mirrorBinding(id, 1))
				fixture.clock.Advance(31 * time.Second)
				return token
			},
			wantErr:   ErrVersionUnavailable,
			retention: 30 * time.Second,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newResolveFixture(t)
			if testCase.retention > 0 {
				fixture = newResolveFixtureWith(t, testCase.retention)
			}
			id := testCredentialID(1)
			authorization := testCase.prepare(t, fixture, id)
			_, err := fixture.resolver.ResolveMirror(context.Background(), id, testResolverIdentity, mirrorRequest(authorization))
			if !errors.Is(err, testCase.wantErr) {
				t.Fatalf("ResolveMirror returned %v, want %v", err, testCase.wantErr)
			}
			assertUnspent(t, fixture)
		})
	}
}

// TestResolveMirrorRefusesAForgedAuthorization proves the Store decides authenticity itself rather
// than trusting the caller: the token below is signed by a key the deployment did not mount.
func TestResolveMirrorRefusesAForgedAuthorization(t *testing.T) {
	_, other, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate a second key: %v", err)
	}
	fixture := newResolveFixture(t)
	id := testCredentialID(1)
	storeCredential(t, fixture.store, id, "ghp_mirror_token")
	authorization, err := cloneauth.Issue(other, fixture.mirrorBinding(id, 1))
	if err != nil {
		t.Fatalf("sign the forged authorization: %v", err)
	}
	_, err = fixture.resolver.ResolveMirror(context.Background(), id, testResolverIdentity, mirrorRequest(authorization))
	if !errors.Is(err, ErrAuthorizationInvalid) {
		t.Fatalf("a forged authorization returned %v, want %v", err, ErrAuthorizationInvalid)
	}
	assertUnspent(t, fixture)
}

// TestAMirrorAuthorizationIsSpentExactlyOnce is the replay refusal, including across a restart of the
// Store on the same volume — the consumption record is a directory entry, so it outlives the process
// that wrote it (D10).
func TestAMirrorAuthorizationIsSpentExactlyOnce(t *testing.T) {
	fixture := newResolveFixture(t)
	id := testCredentialID(1)
	storeCredential(t, fixture.store, id, "ghp_mirror_token")
	authorization := fixture.issue(t, fixture.mirrorBinding(id, 1))

	if _, err := fixture.resolver.ResolveMirror(context.Background(), id, testResolverIdentity, mirrorRequest(authorization)); err != nil {
		t.Fatalf("first redemption: %v", err)
	}
	_, err := fixture.resolver.ResolveMirror(context.Background(), id, testResolverIdentity, mirrorRequest(authorization))
	if !errors.Is(err, ErrAuthorizationConsumed) {
		t.Fatalf("replay returned %v, want %v", err, ErrAuthorizationConsumed)
	}

	reopened, err := Open(Config{
		Volume:                     fixture.volume,
		KeyFile:                    fixture.keyFile,
		Retention:                  DefaultRetention,
		Now:                        fixture.clock.Now,
		AuthorizationPublicKeyFile: fixture.publicFile,
	})
	if err != nil {
		t.Fatalf("reopen the Store: %v", err)
	}
	resolver, ok := reopened.Resolver()
	if !ok {
		t.Fatal("the reopened Store reported no resolve capability")
	}
	_, err = resolver.ResolveMirror(context.Background(), id, testResolverIdentity, mirrorRequest(authorization))
	if !errors.Is(err, ErrAuthorizationConsumed) {
		t.Fatalf("replay after a restart returned %v, want %v", err, ErrAuthorizationConsumed)
	}
}

// mirrorResolveBody builds the transport payload a mirror executor sends. It carries the
// authorization and nothing else, which is the point: every other input the decision needs comes from
// the certificate or from the Store's own state.
func mirrorResolveBody(t *testing.T, authorization string) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"authorization": authorization})
	if err != nil {
		t.Fatalf("encode mirror resolve body: %v", err)
	}
	return string(raw)
}

// TestServerResolveMirrorDeliversThenRefusesReplay proves the whole path over real mutual TLS: the
// executor identity receives the token once, and the same authorization is then refused as consumed.
func TestServerResolveMirrorDeliversThenRefusesReplay(t *testing.T) {
	fixture := newResolveFixture(t)
	server, pki := newTestServer(t, fixture.store, nil)
	resolverCert := pki.issue(t, 50, testResolverIdentity, false)
	client := pki.client(t, &resolverCert)
	id := testCredentialID(30)
	storeCredential(t, fixture.store, id, "ghp_mirror_over_transport")
	body := mirrorResolveBody(t, fixture.issue(t, fixture.mirrorBinding(id, 1)))

	status, raw := doJSON(t, client, http.MethodPost, server.URL+"/v1/credentials/"+id+"/resolve-mirror", body)
	if status != http.StatusOK {
		t.Fatalf("resolve-mirror status = %d, want 200 (body %s)", status, raw)
	}
	if token, _ := decodeBody(t, raw)["token"].(string); token != "ghp_mirror_over_transport" {
		t.Fatalf("resolved token = %q, want %q", token, "ghp_mirror_over_transport")
	}
	status, raw = doJSON(t, client, http.MethodPost, server.URL+"/v1/credentials/"+id+"/resolve-mirror", body)
	if status != http.StatusConflict {
		t.Fatalf("replay status = %d, want 409 (body %s)", status, raw)
	}
	if code := decodeBody(t, raw)["code"]; code != "authorization_consumed" {
		t.Fatalf("replay code = %v, want authorization_consumed", code)
	}
}

// TestServerRefusesEachPurposeAtTheOtherEndpoint proves the separation survives the transport
// boundary, in both directions, with both tokens genuinely signed.
//
// This is the case a route-only separation would miss: the two endpoints are distinct, but an executor
// that called the wrong one would still be delivering a token it was not issued for. What refuses it
// is the operation inside the signed payload, so the refusal is the same 403 the resolver itself
// returns rather than a routing accident.
func TestServerRefusesEachPurposeAtTheOtherEndpoint(t *testing.T) {
	fixture := newResolveFixture(t)
	server, pki := newTestServer(t, fixture.store, nil)
	resolverCert := pki.issue(t, 51, testResolverIdentity, false)
	client := pki.client(t, &resolverCert)
	id := testCredentialID(31)
	storeCredential(t, fixture.store, id, "ghp_cross_purpose")

	// Each case mints its own authorization id, because the second half of each case spends the
	// authorization on the endpoint that does accept it — and a shared id would make the second case
	// fail on the first case's consumption rather than on anything the case is about.
	cases := []struct {
		name          string
		path          string
		authorization func(fixture *resolveFixture) string
	}{
		{
			name: "a mirror authorization on the clone endpoint",
			path: "/resolve",
			authorization: func(fixture *resolveFixture) string {
				binding := fixture.mirrorBinding(id, 1)
				binding.AuthorizationID = testAuthorizationID(2)
				return fixture.issue(t, binding)
			},
		},
		{
			name: "a clone authorization on the mirror endpoint",
			path: "/resolve-mirror",
			authorization: func(fixture *resolveFixture) string {
				binding := fixture.binding(id, 1)
				binding.AuthorizationID = testAuthorizationID(3)
				return fixture.issue(t, binding)
			},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			authorization := testCase.authorization(fixture)
			body := mirrorResolveBody(t, authorization)
			if testCase.path == "/resolve" {
				body = resolveBody(t, authorization)
			}
			status, raw := doJSON(t, client, http.MethodPost, server.URL+"/v1/credentials/"+id+testCase.path, body)
			if status != http.StatusForbidden {
				t.Fatalf("status = %d, want 403 (body %s)", status, raw)
			}
			if code := decodeBody(t, raw)["code"]; code != "authorization_purpose_mismatch" {
				t.Fatalf("code = %v, want authorization_purpose_mismatch", code)
			}
			// The refusal must not have cost the caller the authorization: the right endpoint still works.
			right, retryBody := "/resolve-mirror", mirrorResolveBody(t, authorization)
			if testCase.path == "/resolve-mirror" {
				right, retryBody = "/resolve", resolveBody(t, authorization)
			}
			if status, raw := doJSON(t, client, http.MethodPost, server.URL+"/v1/credentials/"+id+right, retryBody); status != http.StatusOK {
				t.Fatalf("the authorization was spent by the refusal: %s returned %d (body %s)", right, status, raw)
			}
		})
	}
}

// TestServerResolveMirrorRefusesAnUnknownField proves the mirror endpoint strict-decodes like every
// other endpoint: a body that tried to supply its own identity must not be silently ignored, because a
// caller that believed it set the executor identity would be getting a decision made on a different
// value than it thought.
func TestServerResolveMirrorRefusesAnUnknownField(t *testing.T) {
	fixture := newResolveFixture(t)
	server, pki := newTestServer(t, fixture.store, nil)
	resolverCert := pki.issue(t, 52, testResolverIdentity, false)
	client := pki.client(t, &resolverCert)
	id := testCredentialID(32)
	storeCredential(t, fixture.store, id, "ghp_mirror_token")

	body := `{"authorization":"x","executorIdentity":"` + testResolverIdentity + `"}`
	status, raw := doJSON(t, client, http.MethodPost, server.URL+"/v1/credentials/"+id+"/resolve-mirror", body)
	if status != http.StatusBadRequest {
		t.Fatalf("status with a caller-supplied identity = %d, want 400 (body %s)", status, raw)
	}
	if code := decodeBody(t, raw)["code"]; code != "invalid_request" {
		t.Fatalf("code = %v, want invalid_request", code)
	}
}

// TestServerResolveMirrorKeepsSecretsOutOfTheAuditLog proves the mirror route audits under its own
// operation name without recording either the authorization or the token. The operation name matters
// beyond tidiness: it is how a reader tells a mirror redemption from a clone one when reconstructing
// who touched a credential.
func TestServerResolveMirrorKeepsSecretsOutOfTheAuditLog(t *testing.T) {
	fixture := newResolveFixture(t)
	logger, buffer := bufferedLogger()
	server, pki := newTestServer(t, fixture.store, logger)
	resolverCert := pki.issue(t, 53, testResolverIdentity, false)
	client := pki.client(t, &resolverCert)
	id := testCredentialID(33)
	storeCredential(t, fixture.store, id, "ghp_mirror_never_logged")
	authorization := fixture.issue(t, fixture.mirrorBinding(id, 1))

	status, raw := doJSON(t, client, http.MethodPost, server.URL+"/v1/credentials/"+id+"/resolve-mirror", mirrorResolveBody(t, authorization))
	if status != http.StatusOK {
		t.Fatalf("resolve-mirror status = %d, want 200 (body %s)", status, raw)
	}
	logged := buffer.String()
	if strings.Contains(logged, authorization) {
		t.Fatalf("the audit log contains the authorization: %s", logged)
	}
	if strings.Contains(logged, "ghp_mirror_never_logged") {
		t.Fatalf("the audit log contains the token: %s", logged)
	}
	if !strings.Contains(logged, `"operation":"resolve-mirror"`) || !strings.Contains(logged, `"result":"succeeded"`) {
		t.Fatalf("the audit log is missing the mirror resolve record: %s", logged)
	}
}

// assertUnspent proves a refusal left no consumption record, which is what makes the refusal
// retryable with the same authorization rather than costing the caller a new one.
func assertUnspent(t *testing.T, fixture *resolveFixture) {
	t.Helper()
	if _, found := readConsumption(t, fixture, testAuthorizationID(1)); found {
		t.Fatal("a refused redemption wrote a consumption record, so the authorization was spent")
	}
}
