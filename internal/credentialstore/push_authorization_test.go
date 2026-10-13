package credentialstore

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/internal/cloneauth"
)

// The commit pair the push fixture is pinned to. The values are shape-valid and otherwise arbitrary:
// whether one descends from the other is a question about a repository, and the Store never asks it —
// what the Store decides is whether this authorization may be spent at all.
const (
	testOldCommit = "0a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b"
	testNewCommit = "9f8e7d6c5b4a39281706f5e4d3c2b1a09f8e7d6c"
)

// pushBinding returns a valid `push` binding for one credential version at the fixture's current
// instant.
//
// It shares everything a push shares with the other purposes — the credential, the version, the
// repository, the executor identity — so a case that fails does so because of the purpose rather than
// because some unrelated field disagreed. Everything a push does not carry is left at its zero value,
// which is the shape the binding rules require rather than a convenience: a push that carried a Node
// runtime or a preparation attempt would not be a push binding at all.
func (f *resolveFixture) pushBinding(credentialID string, version int64) cloneauth.Binding {
	issued := f.clock.Now()
	return cloneauth.Binding{
		AuthorizationID:   testAuthorizationID(1),
		Operation:         cloneauth.OperationPush,
		TenantID:          "tenant-1",
		OwnerUserID:       "user-1",
		ProjectID:         "project-1",
		WorkspaceID:       "workspace-1",
		OperationID:       "operation-1",
		ExecutorIdentity:  testResolverIdentity,
		CredentialID:      credentialID,
		CredentialVersion: version,
		Repository:        "https://github.com/ora-space/private",
		Ref:               "refs/heads/main",
		OldCommit:         testOldCommit,
		NewCommit:         testNewCommit,
		Capabilities:      []string{string(cloneauth.CapabilityPush)},
		IssuedAt:          issued,
		ExpiresAt:         issued.Add(time.Minute),
	}
}

// TestAPushAuthorizationIsRefusedOnEveryStorePath is the Store's half of the push decision's D5: a
// write authorization is not a new capability or a new exchange surface — it rides the existing
// `resolve` route, and what keeps it from being spent on a read is the operation inside the signed
// payload.
//
// Both routes are exercised, because "there is no push route yet" is not by itself the reason the
// Store is safe. A push authorization that a read route happened to accept would be a token that
// reads, which is exactly what the purpose separation exists to make unexpressible — and the refusal
// has to be the same one the Store gives any other purpose it does not serve, reached before the
// signature's subject is compared against anything.
//
// The refusal must also leave the authorization unspent. A caller that was refused for asking the
// wrong question has not spent anything, and a Store that recorded consumption on the way to a
// purpose refusal would destroy a credential grant as a side effect of an error.
func TestAPushAuthorizationIsRefusedOnEveryStorePath(t *testing.T) {
	cases := []struct {
		name string
		// present performs one redemption attempt against a Store whose credential really exists at the
		// version the authorization names, so the refusal cannot be a version or a lookup failure wearing
		// the wrong name.
		present func(fixture *resolveFixture, id, authorization string) error
	}{
		{
			name: "the clone path",
			present: func(fixture *resolveFixture, id, authorization string) error {
				_, err := fixture.resolver.Resolve(context.Background(), id, fixture.request(authorization))
				return err
			},
		},
		{
			name: "the mirror path",
			present: func(fixture *resolveFixture, id, authorization string) error {
				_, err := fixture.resolver.ResolveMirror(context.Background(), id, testResolverIdentity, mirrorRequest(authorization))
				return err
			},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newResolveFixture(t)
			id := testCredentialID(1)
			storeCredential(t, fixture.store, id, "ghp_write_token")
			authorization := fixture.issue(t, fixture.pushBinding(id, 1))

			err := testCase.present(fixture, id, authorization)
			if !errors.Is(err, ErrAuthorizationPurpose) {
				t.Fatalf("a push authorization returned %v, want %v", err, ErrAuthorizationPurpose)
			}
			assertUnspent(t, fixture)
		})
	}
}

// TestAServerRefusesAPushAuthorizationAtBothReadEndpoints proves the refusal survives the transport
// boundary and is the same 403 a wrong-purpose token gets there.
//
// The status and the code are what a caller actually sees, and they are asserted rather than the
// resolver's error, because the mapping is part of the contract: a deployment that answered 500 would
// be telling an executor to retry a request that can never succeed.
func TestAServerRefusesAPushAuthorizationAtBothReadEndpoints(t *testing.T) {
	fixture := newResolveFixture(t)
	server, pki := newTestServer(t, fixture.store, nil)
	resolverCert := pki.issue(t, 54, testResolverIdentity, false)
	client := pki.client(t, &resolverCert)
	id := testCredentialID(34)
	storeCredential(t, fixture.store, id, "ghp_write_over_transport")
	authorization := fixture.issue(t, fixture.pushBinding(id, 1))

	for _, path := range []string{"/resolve", "/resolve-mirror"} {
		t.Run(path, func(t *testing.T) {
			body := mirrorResolveBody(t, authorization)
			if path == "/resolve" {
				body = resolveBody(t, authorization)
			}
			status, raw := doJSON(t, client, http.MethodPost, server.URL+"/v1/credentials/"+id+path, body)
			if status != http.StatusForbidden {
				t.Fatalf("%s status = %d, want 403 (body %s)", path, status, raw)
			}
			if code := decodeBody(t, raw)["code"]; code != "authorization_purpose_mismatch" {
				t.Fatalf("%s code = %v, want authorization_purpose_mismatch", path, code)
			}
		})
	}
	assertUnspent(t, fixture)
}
