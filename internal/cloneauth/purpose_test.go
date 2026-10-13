package cloneauth

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// TestTheTwoPurposesRefuseEachOthersFields is the two-way separation the mirror purpose depends on.
//
// The operation is only a boundary if a payload cannot carry both identities at once. A binding naming
// an execution *and* a Node runtime *and* a preparation attempt would be readable as either work, so
// the check is symmetric and rejects rather than ignores: each operation names the other's fields as
// forbidden, and a mixed payload fails on the first one it carries.
//
// Each case changes exactly one field of an otherwise valid binding, so a failure names the rule that
// held rather than the coincidence that something was wrong somewhere.
func TestTheTwoPurposesRefuseEachOthersFields(t *testing.T) {
	cases := []struct {
		name  string
		base  func() Binding
		apply func(*Binding)
	}{
		{
			name:  "a clone that carries a ref",
			base:  testBinding,
			apply: func(b *Binding) { b.Ref = "refs/heads/main" },
		},
		{
			name:  "a clone that carries a preparation attempt",
			base:  testBinding,
			apply: func(b *Binding) { b.PreparationGeneration = 1 },
		},
		{
			name:  "a clone that carries an operation id",
			base:  testBinding,
			apply: func(b *Binding) { b.OperationID = "operation-1" },
		},
		{
			name:  "a clone that carries an executor identity",
			base:  testBinding,
			apply: func(b *Binding) { b.ExecutorIdentity = "spiffe://ora.test/executor/mirror" },
		},
		{
			name:  "a clone with no execution id",
			base:  testBinding,
			apply: func(b *Binding) { b.ExecutionID = "" },
		},
		{
			// The exact shape the previous round could not express: filling the execution field with the
			// operation's id, so a mirror preparation would masquerade as a clone execution.
			name:  "a mirror preparation that names itself as an execution",
			base:  mirrorBinding,
			apply: func(b *Binding) { b.ExecutionID = b.OperationID },
		},
		{
			name:  "a mirror preparation that carries a node id",
			base:  mirrorBinding,
			apply: func(b *Binding) { b.NodeID = "node-1" },
		},
		{
			name:  "a mirror preparation that carries a node instance",
			base:  mirrorBinding,
			apply: func(b *Binding) { b.NodeInstanceID = "node-instance-1" },
		},
		{
			name:  "a mirror preparation that carries a node incarnation",
			base:  mirrorBinding,
			apply: func(b *Binding) { b.NodeIncarnationID = "incarnation-1" },
		},
		{
			name:  "a mirror preparation with no operation id",
			base:  mirrorBinding,
			apply: func(b *Binding) { b.OperationID = "" },
		},
		{
			name:  "a mirror preparation with no preparation generation",
			base:  mirrorBinding,
			apply: func(b *Binding) { b.PreparationGeneration = 0 },
		},
		{
			name:  "a mirror preparation with no executor identity",
			base:  mirrorBinding,
			apply: func(b *Binding) { b.ExecutorIdentity = "" },
		},
		{
			name:  "a mirror preparation with no ref",
			base:  mirrorBinding,
			apply: func(b *Binding) { b.Ref = "" },
		},
		{
			name:  "a mirror preparation pinned to a tag",
			base:  mirrorBinding,
			apply: func(b *Binding) { b.Ref = "refs/tags/v1.0.0" },
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, private := testKeys(t)
			binding := c.base()
			c.apply(&binding)
			if _, err := Issue(private, binding); !errors.Is(err, ErrIncomplete) {
				t.Fatalf("Issue accepted a binding it must refuse: %v", err)
			}
		})
	}
}

// TestAForbiddenFieldIsRefusedEvenWhenTheSignatureIsValid is why the rules live in validate() rather
// than in Issue() alone.
//
// A payload reaches the Store as bytes someone else produced. Signing the binding below by hand
// skips Issue entirely and keeps a genuine signature, which is the shape of a token minted by a build
// whose issuance rules were laxer than this one's — or by whoever holds the signing key. Verification
// has to refuse it, because the alternative is that the purpose separation is only as strong as the
// issuer that happens to be deployed.
func TestAForbiddenFieldIsRefusedEvenWhenTheSignatureIsValid(t *testing.T) {
	public, private := testKeys(t)

	// One mixed payload per direction, plus the substitution the previous round could not express.
	mixed := testBinding()
	mixed.Ref = "refs/heads/main"
	substituted := mirrorBinding()
	substituted.ExecutionID = substituted.OperationID
	unknown := testBinding()
	unknown.Operation = "push"

	for _, binding := range []Binding{mixed, substituted, unknown} {
		payload, err := json.Marshal(binding)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		signature := ed25519.Sign(private, payload)
		token := base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(signature)
		if _, err := verifyOne(public, token, testIssued()); !errors.Is(err, ErrIncomplete) {
			t.Fatalf("operation %q with a valid signature verified: %v", binding.Operation, err)
		}
	}
}

// TestAMirrorPreparationCannotBeSpentAsAClone pins the consumer side of the separation: the two
// purposes are distinguishable from the verified binding alone, so a consumer must compare the
// operation rather than infer it from which route the request arrived on.
func TestAMirrorPreparationCannotBeSpentAsAClone(t *testing.T) {
	public, private := testKeys(t)

	token, err := Issue(private, mirrorBinding())
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	got, err := verifyOne(public, token, testIssued())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got.Operation != OperationMirrorPrepare {
		t.Fatalf("a mirror preparation verified as %q, want %q", got.Operation, OperationMirrorPrepare)
	}
	// The identity fields are the reason the two are not interchangeable: a mirror carries none of the
	// clone's runtime, and a clone carries none of the mirror's.
	if got.ExecutionID != "" || got.NodeID != "" || got.NodeInstanceID != "" || got.NodeIncarnationID != "" {
		t.Fatalf("a mirror preparation carried the clone's identity: %+v", got)
	}
	if got.OperationID == "" || got.ExecutorIdentity == "" || got.Ref == "" {
		t.Fatalf("a mirror preparation lost its own identity: %+v", got)
	}

	clone, err := Issue(private, testBinding())
	if err != nil {
		t.Fatalf("Issue clone: %v", err)
	}
	verifiedClone, err := verifyOne(public, clone, testIssued())
	if err != nil {
		t.Fatalf("Verify clone: %v", err)
	}
	if verifiedClone.Operation != OperationClone {
		t.Fatalf("a clone verified as %q, want %q", verifiedClone.Operation, OperationClone)
	}
	if verifiedClone.ExecutorIdentity != "" || verifiedClone.OperationID != "" || verifiedClone.Ref != "" {
		t.Fatalf("a clone carried the mirror's identity: %+v", verifiedClone)
	}
}

// TestAnUnknownOperationIsNotReadAsAClone pins the direction the operation resolution takes: a payload
// naming an operation this build does not define is refused rather than defaulted to the one that is.
// The empty operation is the case that matters most, because a zero value is what a payload omitting
// the field carries.
func TestAnUnknownOperationIsNotReadAsAClone(t *testing.T) {
	_, private := testKeys(t)
	for _, operation := range []string{"", "push", "CLONE", "mirror-prepare", "clone "} {
		binding := testBinding()
		binding.Operation = operation
		_, err := Issue(private, binding)
		if !errors.Is(err, ErrIncomplete) {
			t.Fatalf("operation %q was accepted: %v", operation, err)
		}
		if operation != "" && !strings.Contains(err.Error(), "not one this package defines") {
			t.Fatalf("operation %q was refused for the wrong reason: %v", operation, err)
		}
	}
}
