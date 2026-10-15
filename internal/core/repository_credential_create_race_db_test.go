package core

// Direct PostgreSQL evidence for the cross-store create under a concurrent same-key race
// (`git_credentials.go:331` `createGitCredential`). The create is two writes — a handoff to the
// Credential Service, then the metadata commit — and two callers sharing one idempotency key can both
// pass the read-only `prepareGitCredential` before either commits. The property under test is what the
// aggregate must never do in that window: seal two credential rows, mint two ids under one key, report
// a second creation, or lose a handoff whose material was already written.
//
// The race is staged on the real Store handoff seam (`Store.Secrets.Write`), never with a sleep. Both
// callers are held *inside* the handoff by the double, one is released to commit its metadata while the
// other is still in flight, and only then is the second released — the exact interleaving the decision
// describes. Nothing here holds a transaction across the handoff: the block is in the store call, which
// is outside every transaction, exactly as the product orders it.

import (
	"context"
	"sync"
	"testing"
)

// handoffRaceSecrets is the Credential Service's ingress as a test double that lets a test hold both
// callers inside the handoff. It answers with one reference per credential id — the store's own
// idempotency (D4), which is why a repeated write for the same id returns the reference it already
// holds rather than sealing a second material — and signals the order in which callers entered.
type handoffRaceSecrets struct {
	mu       sync.Mutex
	refs     map[string]string
	writeIDs []string
	entered  chan int
	gate     [2]chan struct{}
}

func newHandoffRaceSecrets() *handoffRaceSecrets {
	return &handoffRaceSecrets{
		refs:    map[string]string{},
		entered: make(chan int, 2),
		gate:    [2]chan struct{}{make(chan struct{}), make(chan struct{})},
	}
}

func (s *handoffRaceSecrets) Write(_ context.Context, credentialID, token string) (string, error) {
	s.mu.Lock()
	s.writeIDs = append(s.writeIDs, credentialID)
	order := len(s.writeIDs)
	ref, ok := s.refs[credentialID]
	if !ok {
		ref = "cs_" + credentialID
		s.refs[credentialID] = ref
	}
	s.mu.Unlock()
	// Announce that this caller is inside the handoff, then wait for the test to release it. The
	// second caller is the one that commits *during* the first caller's handoff.
	s.entered <- order
	<-s.gate[order-1]
	return ref, nil
}

// Rotate and Revoke are not part of the scene: the race is on the create handoff, and a double that
// answered a rotation would have to model the store's version counter to be believed.
func (s *handoffRaceSecrets) Rotate(context.Context, string, string) (int64, error) { return 0, nil }
func (s *handoffRaceSecrets) Revoke(context.Context, string) error                  { return nil }

func (s *handoffRaceSecrets) handoffIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.writeIDs...)
}

func (s *handoffRaceSecrets) reference(credentialID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refs[credentialID]
}

func (s *handoffRaceSecrets) materialCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.refs)
}

// createOutcome is one create's terminal shape, so the assertions can name the forbidden shapes
// rather than only accepting the observed one.
type createOutcome struct {
	result GitCredentialResult
	err    error
}

// TestTwoConcurrentCreatesUnderOneKeySealExactlyOneCredential pins the whole permitted outcome set of
// a same-key create race. The deterministic interleaving is: both callers pass prepare (neither sees an
// idempotency record), both enter the handoff, the first commits its metadata while the second is still
// in the handoff, and the second then completes against the already-committed create.
//
// Non-vacuity is by the arrangement, not by a mutation: the second caller is provably inside its store
// handoff (the double blocks it) at the instant the first commits, so this is the real window and not a
// sequential replay dressed up as one. The assertions reject every forbidden shape — a second
// credential row, a second distinct id under the key, a second recorded response, or a handoff whose
// row is missing — and the equality of the two response documents shows the second caller received the
// *recorded* create rather than minting another: "success reported twice" is only ever the same single
// creation observed by both callers.
func TestTwoConcurrentCreatesUnderOneKeySealExactlyOneCredential(t *testing.T) {
	store := dispatcherDB(t)
	secrets := newHandoffRaceSecrets()
	store.Secrets = secrets
	tenant, user, claims := bootstrapCaller(t, store, "race")
	key := "race-" + newID()
	id := gitCredentialID(tenant, user, key)

	request := func() *PublicRequest {
		return &PublicRequest{
			Method: "POST", Path: "/api/v1/tenants/" + tenant + "/git-credentials",
			TenantID: tenant, Key: key,
			Body:     Object{"name": "racing", "repositoryScope": admittedScope},
			Identity: claims,
		}
	}
	create := func() createOutcome {
		result, err := store.WriteGitCredential(context.Background(), request(), fakeGitToken)
		return createOutcome{result: result, err: err}
	}

	first := make(chan createOutcome, 1)
	go func() { first <- create() }()
	if order := <-secrets.entered; order != 1 {
		t.Fatalf("the first caller must be the one inside the handoff, got order %d", order)
	}

	second := make(chan createOutcome, 1)
	go func() { second <- create() }()
	if order := <-secrets.entered; order != 2 {
		t.Fatalf("the second caller must be inside the handoff before the first commits, got order %d", order)
	}

	// Release the first caller so its metadata commit lands while the second is still in the handoff.
	close(secrets.gate[0])
	early := <-first
	if early.err != nil {
		t.Fatalf("the first concurrent create must commit, got %v", early.err)
	}

	// Now let the second handoff return. Its metadata transaction must adopt the committed create.
	close(secrets.gate[1])
	late := <-second

	// The permitted set: both callers observe the one committed create. Anything else is a violation
	// of the create's cross-store idempotency, and each forbidden shape is named here.
	ids := map[string]bool{}
	for name, outcome := range map[string]createOutcome{"first": early, "second": late} {
		if outcome.err != nil {
			t.Fatalf("the %s caller must observe the committed create, not an error (%v): the aggregate must never lose a handoff whose material is already written", name, outcome.err)
		}
		if outcome.result.Status != 201 {
			t.Fatalf("the %s caller must observe 201 Created for the committed create, got %d", name, outcome.result.Status)
		}
		if got := outcome.result.Resource.S("id"); got != id {
			t.Fatalf("the %s caller reported credential %q, want the derived id %q: never two distinct ids under one key", name, got, id)
		}
		ids[outcome.result.Resource.S("id")] = true
	}
	if len(ids) != 1 {
		t.Fatalf("one idempotency key produced %d distinct credential ids: %v", len(ids), ids)
	}
	// The second caller received the first's recorded response, not a freshly-created one. This is the
	// difference between "the same creation observed twice" and "a success minted twice".
	if jsonText(early.result.Resource) != jsonText(late.result.Resource) {
		t.Fatalf("the second caller minted its own response instead of replaying the recorded one:\n first=%s\nsecond=%s",
			jsonText(early.result.Resource), jsonText(late.result.Resource))
	}

	// The durable aggregate: exactly one row, one recorded outcome, one material.
	if n := scalarOf(t, store, `SELECT count(*) FROM credential_refs WHERE tenant_id=$1 AND owner_user_id=$2`, tenant, user); n != 1 {
		t.Fatalf("one key under a race sealed %d credential rows, want 1", n)
	}
	if n := scalarOf(t, store, `SELECT count(*) FROM credential_refs WHERE id=$1`, id); n != 1 {
		t.Fatal("the derived credential id does not identify exactly one row")
	}
	if n := scalarOf(t, store, `SELECT count(*) FROM idempotency_records WHERE tenant_id=$1 AND user_id=$2 AND key=$3`, tenant, user, key); n != 1 {
		t.Fatalf("one key recorded %d idempotency outcomes, want 1", n)
	}
	if ref := textOf(t, store, `SELECT secret_ref FROM credential_refs WHERE id=$1`, id); ref != secrets.reference(id) {
		t.Fatalf("the committed row locates %q, want the reference the store holds %q", ref, secrets.reference(id))
	}

	// Both callers reached the handoff (the double is the witness), and both used the same id — the
	// store's idempotency, not Cloud's, is what makes the second write harmless. One material exists.
	if got := secrets.handoffIDs(); len(got) != 2 || got[0] != id || got[1] != id {
		t.Fatalf("both handoffs must use the one derived id %q, got %v", id, got)
	}
	if n := secrets.materialCount(); n != 1 {
		t.Fatalf("the store holds %d materials for one credential, want 1", n)
	}
}
