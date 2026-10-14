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
// repository, the executor identity — so a case that fails does so because of the check under test
// rather than because some unrelated field disagreed. Everything a push does not carry is left at its
// zero value, which is the shape the binding rules require rather than a convenience: a push that
// carried a Node runtime or a preparation attempt would not be a push binding at all, and
// `cloneauth.Issue` would refuse to sign it.
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

// pushRequest is what a push redemption presents on the wire: the authorization and nothing else.
//
// The emptiness is the design, not an omission. A push runs on no Node, so there is no runtime for the
// caller to state, and the party permitted to spend it is resolved from the verified client certificate
// rather than from anything the caller writes down. A request that stated a runtime would be stating a
// value no check consults, which is how a field comes to look like a fence.
func pushRequest(authorization string) ResolveRequest {
	return ResolveRequest{Authorization: authorization}
}

// TestResolveDeliversTheTokenAPushAuthorizationNames is the happy path of the write purpose: a push
// authorization presented by the one identity it names, with no runtime declared, yields the token and
// spends the authorization.
//
// The consumption record is read back from the volume rather than from anything the resolver kept, and
// its Operation is asserted: this is the field that tells a later reader whether one delivered secret
// was spent on a read or on a write, and a record that could not distinguish the two would make the
// audit useless for exactly the question it exists to answer.
func TestResolveDeliversTheTokenAPushAuthorizationNames(t *testing.T) {
	fixture := newResolveFixture(t)
	id := testCredentialID(20)
	storeCredential(t, fixture.store, id, "ghp_push_token")
	authorization := fixture.issue(t, fixture.pushBinding(id, 1))

	resolved, err := fixture.resolver.Resolve(context.Background(), id, testResolverIdentity, pushRequest(authorization))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resolved.Token != "ghp_push_token" {
		t.Fatalf("token = %q, want %q", resolved.Token, "ghp_push_token")
	}
	if resolved.Version != 1 {
		t.Fatalf("version = %d, want 1", resolved.Version)
	}
	record, found := readConsumption(t, fixture, testAuthorizationID(1))
	if !found {
		t.Fatal("a delivered push wrote no consumption record")
	}
	if record.Operation != cloneauth.OperationPush {
		t.Fatalf("recorded operation = %q, want %q", record.Operation, cloneauth.OperationPush)
	}
	if record.ExecutorIdentity != testResolverIdentity {
		t.Fatalf("recorded executor identity = %q, want %q", record.ExecutorIdentity, testResolverIdentity)
	}
	if record.NodeID != "" || record.RuntimeGeneration != 0 {
		t.Fatalf("a push consumption recorded a runtime: node %q generation %d", record.NodeID, record.RuntimeGeneration)
	}
	if record.Result != consumptionDelivered {
		t.Fatalf("recorded result = %q, want %q", record.Result, consumptionDelivered)
	}
}

// TestResolveRefusesAPushAuthorizationThatFailsOneOfItsChecks is the negative matrix of the write
// purpose, and every case must leave the authorization unspent.
//
// Unspent is the part that matters. A refusal reached before the credential is looked at is a caller
// that asked the wrong question or presented the wrong values, and a Store that recorded consumption on
// the way to that refusal would destroy a write grant as a side effect of an error — turning a
// correctable mistake into one that costs a new approval.
//
// The last case is the one the purpose separation exists for: an authorization minted for a *clone*,
// presented with exactly the empty runtime a push request carries. It has to be refused on its binding
// and not merely on its purpose, because a clone's binding carries no commit pair to disagree with —
// so if this route did not compare the operation, the clone's single use would be spent on a write.
func TestResolveRefusesAPushAuthorizationThatFailsOneOfItsChecks(t *testing.T) {
	cases := []struct {
		name string
		// build returns the request to present and the identity the transport proved alongside it.
		build func(fixture *resolveFixture, id, authorization string) (ResolveRequest, string)
		// want is the sentinel the refusal must be. Every case refuses: a push authorization presented
		// with anything other than the empty runtime and the named identity is refused before the
		// credential is looked at.
		want error
		// path selects which credential id the request is addressed to. It defaults to the one the
		// authorization names; the mismatch case points it at another credential so the disagreement is
		// between the path and the signature.
		path func(fixture *resolveFixture, id string) string
	}{
		{
			name: "an identity the authorization does not name",
			build: func(_ *resolveFixture, _, authorization string) (ResolveRequest, string) {
				return pushRequest(authorization), "spiffe://ora/node/some-other-workload"
			},
			want: ErrAuthorizationBinding,
		},
		{
			name: "no established identity",
			build: func(_ *resolveFixture, _, authorization string) (ResolveRequest, string) {
				return pushRequest(authorization), ""
			},
			want: ErrAuthorizationBinding,
		},
		{
			name: "a runtime the push binding cannot carry",
			build: func(_ *resolveFixture, _, authorization string) (ResolveRequest, string) {
				request := pushRequest(authorization)
				request.NodeID = testNodeID
				request.NodeInstanceID = testNodeInstanceID
				request.NodeIncarnationID = testNodeIncarnationID
				request.RuntimeGeneration = testRuntimeGeneration
				request.ControlEpoch = testControlEpoch
				return request, testResolverIdentity
			},
			want: ErrAuthorizationBinding,
		},
		{
			name: "a credential id other than the one in the path",
			build: func(_ *resolveFixture, _, authorization string) (ResolveRequest, string) {
				return pushRequest(authorization), testResolverIdentity
			},
			want: ErrAuthorizationBinding,
			path: func(fixture *resolveFixture, _ string) string {
				other := testCredentialID(22)
				storeCredential(t, fixture.store, other, "ghp_other")
				return other
			},
		},
		{
			name: "a clone authorization presented as a push",
			build: func(fixture *resolveFixture, id, _ string) (ResolveRequest, string) {
				return pushRequest(fixture.issue(t, fixture.binding(id, 1))), testResolverIdentity
			},
			want: ErrAuthorizationBinding,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newResolveFixture(t)
			id := testCredentialID(21)
			storeCredential(t, fixture.store, id, "ghp_push_negative")
			authorization := fixture.issue(t, fixture.pushBinding(id, 1))
			request, identity := testCase.build(fixture, id, authorization)
			path := id
			if testCase.path != nil {
				path = testCase.path(fixture, id)
			}

			if _, err := fixture.resolver.Resolve(context.Background(), path, identity, request); !errors.Is(err, testCase.want) {
				t.Fatalf("Resolve error = %v, want %v", err, testCase.want)
			}
			assertUnspent(t, fixture)
		})
	}
}

// TestAPushAuthorizationIsRefusedOnTheMirrorPath proves the purpose separation in the direction that
// still holds after push gained a redemption path: the operation inside the signed payload is what
// decides which route may spend it, and a write authorization carries no preparation to be spent on.
//
// It is asserted here rather than left to the mirror tests because the interesting failure is not "the
// mirror route exists" but "adding a push redemption did not add a second route a push can be spent
// on". The refusal must also leave the authorization unspent.
func TestAPushAuthorizationIsRefusedOnTheMirrorPath(t *testing.T) {
	fixture := newResolveFixture(t)
	id := testCredentialID(23)
	storeCredential(t, fixture.store, id, "ghp_push_on_mirror")
	authorization := fixture.issue(t, fixture.pushBinding(id, 1))

	_, err := fixture.resolver.ResolveMirror(context.Background(), id, testResolverIdentity, mirrorRequest(authorization))
	if !errors.Is(err, ErrAuthorizationPurpose) {
		t.Fatalf("ResolveMirror error = %v, want %v", err, ErrAuthorizationPurpose)
	}
	assertUnspent(t, fixture)
}

// TestResolveSpendsAPushAuthorizationExactlyOnce proves the single-use rule for the write purpose,
// including the case that matters most: a second attempt from the same, correct identity.
//
// The second refusal must be `consumed` rather than a binding error, because the two say different
// things to the caller: a binding error invites it to fix its request and try again, while `consumed`
// tells it the grant is gone and a new approval is needed. For a write, sending the caller back to fix
// its request would be sending it back to write again.
func TestResolveSpendsAPushAuthorizationExactlyOnce(t *testing.T) {
	fixture := newResolveFixture(t)
	id := testCredentialID(24)
	storeCredential(t, fixture.store, id, "ghp_push_replay")
	authorization := fixture.issue(t, fixture.pushBinding(id, 1))

	if _, err := fixture.resolver.Resolve(context.Background(), id, testResolverIdentity, pushRequest(authorization)); err != nil {
		t.Fatalf("first Resolve: %v", err)
	}
	_, err := fixture.resolver.Resolve(context.Background(), id, testResolverIdentity, pushRequest(authorization))
	if !errors.Is(err, ErrAuthorizationConsumed) {
		t.Fatalf("replay error = %v, want %v", err, ErrAuthorizationConsumed)
	}
}

// TestResolveRefusesAPushAuthorizationForRevokedOrUnavailableMaterial proves the two Store-side states
// that outrank any authorization: a version this Store cannot read, and a credential it has destroyed.
//
// A push is the case where getting this wrong is least recoverable — the write would go through with
// material the owner has already withdrawn — so it is asserted for the write purpose on its own rather
// than assumed from the clone path's coverage of the same two checks, which runs through a different
// branch of `deliver`'s callers.
func TestResolveRefusesAPushAuthorizationForRevokedOrUnavailableMaterial(t *testing.T) {
	t.Run("revoked", func(t *testing.T) {
		fixture := newResolveFixture(t)
		id := testCredentialID(25)
		storeCredential(t, fixture.store, id, "ghp_push_revoked")
		authorization := fixture.issue(t, fixture.pushBinding(id, 1))
		if err := fixture.store.Revoke(context.Background(), id); err != nil {
			t.Fatalf("Revoke: %v", err)
		}
		if _, err := fixture.resolver.Resolve(context.Background(), id, testResolverIdentity, pushRequest(authorization)); !errors.Is(err, ErrRevoked) {
			t.Fatalf("Resolve error = %v, want %v", err, ErrRevoked)
		}
		assertUnspent(t, fixture)
	})

	t.Run("version unavailable", func(t *testing.T) {
		fixture := newResolveFixtureWith(t, 30*time.Second)
		id := testCredentialID(26)
		storeCredential(t, fixture.store, id, "ghp_push_first")
		if _, err := fixture.store.Rotate(context.Background(), id, "ghp_push_second"); err != nil {
			t.Fatalf("Rotate: %v", err)
		}
		authorization := fixture.issue(t, fixture.pushBinding(id, 1))
		fixture.clock.Advance(31 * time.Second)
		if _, err := fixture.resolver.Resolve(context.Background(), id, testResolverIdentity, pushRequest(authorization)); !errors.Is(err, ErrVersionUnavailable) {
			t.Fatalf("Resolve error = %v, want %v", err, ErrVersionUnavailable)
		}
		assertUnspent(t, fixture)
	})
}

// pushResolveBody builds the transport payload a push redemption sends. It is byte-for-byte the shape a
// mirror redemption sends — the authorization and nothing else — because neither states a runtime; the
// two differ in the operation inside the signed payload, not in what they put on the wire. It is named
// separately so a reader of the push tests does not have to work out that the mirror helper is the
// right one to call.
func pushResolveBody(t *testing.T, authorization string) string {
	t.Helper()
	return mirrorResolveBody(t, authorization)
}

// TestServerResolveDeliversAPushTokenThenRefusesReplayAndForeignIdentity proves the write purpose over
// real mutual TLS, because the identity this route compares against the binding is the one the
// transport proves from the certificate — and a test that called the resolver directly would be
// asserting the comparison while assuming the value.
//
// Three requests, in the order that makes each one meaningful: the named identity is delivered the
// token, the same identity is then refused as consumed, and a *fresh* authorization for the same
// credential is refused when it arrives under an identity the deployment did not give the resolve
// capability to.
//
// The third case is the ingress writer rather than a second resolve-capable identity, and the reason is
// a property of the deployment rather than a shortcut: the identity table issues exactly one resolve
// capability (D3), so "a different identity that may redeem" does not exist to be tested. What the
// boundary really provides there is the capability check the route performs before the resolver is
// reached, and that is what the case asserts. The comparison against the authorization's own
// `executorIdentity` is exercised where it can be isolated — in the resolver-level matrix above, where
// a caller can present any identity it likes.
func TestServerResolveDeliversAPushTokenThenRefusesReplayAndForeignIdentity(t *testing.T) {
	fixture := newResolveFixture(t)
	server, pki := newTestServer(t, fixture.store, nil)
	resolverCert := pki.issue(t, 60, testResolverIdentity, false)
	client := pki.client(t, &resolverCert)
	id := testCredentialID(27)
	storeCredential(t, fixture.store, id, "ghp_push_over_transport")
	authorization := fixture.issue(t, fixture.pushBinding(id, 1))
	target := server.URL + "/v1/credentials/" + id + "/resolve"

	status, raw := doJSON(t, client, http.MethodPost, target, pushResolveBody(t, authorization))
	if status != http.StatusOK {
		t.Fatalf("push resolve status = %d, want 200 (body %s)", status, raw)
	}
	body := decodeBody(t, raw)
	if body["token"] != "ghp_push_over_transport" {
		t.Fatalf("token = %v, want the stored token", body["token"])
	}

	status, raw = doJSON(t, client, http.MethodPost, target, pushResolveBody(t, authorization))
	if status != http.StatusConflict {
		t.Fatalf("replay status = %d, want 409 (body %s)", status, raw)
	}
	if code := decodeBody(t, raw)["code"]; code != "authorization_consumed" {
		t.Fatalf("replay code = %v, want authorization_consumed", code)
	}

	fresh := fixture.pushBinding(id, 1)
	fresh.AuthorizationID = testAuthorizationID(2)
	writerCert := pki.issue(t, 61, testIngressIdentity, false)
	writer := pki.client(t, &writerCert)
	status, raw = doJSON(t, writer, http.MethodPost, target, pushResolveBody(t, fixture.issue(t, fresh)))
	if status != http.StatusForbidden {
		t.Fatalf("writer status = %d, want 403 (body %s)", status, raw)
	}
	if code := decodeBody(t, raw)["code"]; code != "capability_denied" {
		t.Fatalf("writer code = %v, want capability_denied", code)
	}
	// The refused request spent nothing: its authorization is a different one from the spent above, and
	// the refusal happened before the resolver was reached.
	if _, found := readConsumption(t, fixture, testAuthorizationID(2)); found {
		t.Fatal("a request refused for holding the wrong capability recorded a consumption")
	}
}
