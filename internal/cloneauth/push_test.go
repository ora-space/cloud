package cloneauth

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// testOldCommit and testNewCommit are a plausible commit pair: 40 lowercase hexadecimal characters,
// which is the only shape this package accepts. They are not a real object graph — whether one
// descends from the other is a question about a repository, and the executor answers it against its
// own copy before any network write.
const (
	testOldCommit = "0a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b"
	testNewCommit = "9f8e7d6c5b4a39281706f5e4d3c2b1a09f8e7d6c"
)

// pushBinding returns one complete, valid push binding. Tests mutate a single field at a time so each
// field's contribution is visible in the failure it causes.
//
// It is built on testBinding and then has every clone-only field cleared, which is the shape the push
// operation requires rather than a convenience: the fields a push must not carry are exactly the ones
// the clone helper sets.
func pushBinding() Binding {
	binding := testBinding()
	binding.Operation = OperationPush
	binding.ExecutionID = ""
	binding.NodeID = ""
	binding.NodeInstanceID = ""
	binding.NodeIncarnationID = ""
	binding.RuntimeGeneration = 0
	binding.ControlEpoch = 0
	binding.PreparationGeneration = 0
	binding.OperationID = "operation-1"
	binding.ExecutorIdentity = "spiffe://ora.test/executor/writer"
	binding.Ref = "refs/heads/main"
	binding.OldCommit = testOldCommit
	binding.NewCommit = testNewCommit
	binding.Capabilities = []string{string(CapabilityPush)}
	return binding
}

// TestPushBindingRoundTrip proves a write authorization survives the wire with its whole subject
// intact: the upstream, the ref, and both commits. A consumer that could only recover some of them
// would be deciding for itself what the write was allowed to do.
func TestPushBindingRoundTrip(t *testing.T) {
	public, private := testKeys(t)
	want := pushBinding()
	token, err := Issue(private, want)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	got, err := verifyOne(public, token, testIssued())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Verify returned\n%+v\nwant\n%+v", got, want)
	}
	if got.Capabilities[0] != string(CapabilityPush) {
		t.Fatalf("a push carried %v, want exactly [%s]", got.Capabilities, CapabilityPush)
	}
}

// TestPushRefusesTheOtherOperationsFields is the push half of the purpose separation, in both
// directions.
//
// A payload that named an execution and a runtime as well as a ref and a commit pair would be
// readable as either work, and the reader that chose wrongly would be acting on an authority nobody
// granted it. Each case changes exactly one field, so a failure names the rule that held.
func TestPushRefusesTheOtherOperationsFields(t *testing.T) {
	cases := []struct {
		name  string
		base  func() Binding
		apply func(*Binding)
	}{
		{
			name:  "a push with no operation id",
			base:  pushBinding,
			apply: func(b *Binding) { b.OperationID = "" },
		},
		{
			name:  "a push with no executor identity",
			base:  pushBinding,
			apply: func(b *Binding) { b.ExecutorIdentity = "" },
		},
		{
			name:  "a push with no ref",
			base:  pushBinding,
			apply: func(b *Binding) { b.Ref = "" },
		},
		{
			name:  "a push pinned to a short branch name",
			base:  pushBinding,
			apply: func(b *Binding) { b.Ref = "main" },
		},
		{
			name:  "a push pinned to a tag",
			base:  pushBinding,
			apply: func(b *Binding) { b.Ref = "refs/tags/v1.0.0" },
		},
		{
			name:  "a push pinned to a ref pattern",
			base:  pushBinding,
			apply: func(b *Binding) { b.Ref = "refs/heads/*" },
		},
		{
			name:  "a push with no observed commit",
			base:  pushBinding,
			apply: func(b *Binding) { b.OldCommit = "" },
		},
		{
			name:  "a push with no published commit",
			base:  pushBinding,
			apply: func(b *Binding) { b.NewCommit = "" },
		},
		{
			name:  "a push whose observed commit is abbreviated",
			base:  pushBinding,
			apply: func(b *Binding) { b.OldCommit = "1111111" },
		},
		{
			name:  "a push whose published commit is not hexadecimal",
			base:  pushBinding,
			apply: func(b *Binding) { b.NewCommit = strings.Repeat("z", 40) },
		},
		{
			name:  "a push whose published commit is upper case",
			base:  pushBinding,
			apply: func(b *Binding) { b.NewCommit = strings.ToUpper(testNewCommit) },
		},
		{
			// The delete spelling. The execution authority refuses deleting a ref outright, and the null
			// object id is the one spelling of "no commit" Git's own protocol would accept for it, so the
			// refusal has to be here rather than only in a rule a formatter could skip.
			name:  "a push whose published commit is the null object id",
			base:  pushBinding,
			apply: func(b *Binding) { b.NewCommit = strings.Repeat("0", 40) },
		},
		{
			// The creation spelling: `old` as the null oid is how a ref update says the ref does not
			// exist yet, which is the other half of what the authority refuses.
			name:  "a push whose observed commit is the null object id",
			base:  pushBinding,
			apply: func(b *Binding) { b.OldCommit = strings.Repeat("0", 40) },
		},
		{
			name:  "a push that names an execution",
			base:  pushBinding,
			apply: func(b *Binding) { b.ExecutionID = "execution-1" },
		},
		{
			name:  "a push that carries a node id",
			base:  pushBinding,
			apply: func(b *Binding) { b.NodeID = "node-1" },
		},
		{
			name:  "a push that carries a node instance",
			base:  pushBinding,
			apply: func(b *Binding) { b.NodeInstanceID = "node-instance-1" },
		},
		{
			name:  "a push that carries a node incarnation",
			base:  pushBinding,
			apply: func(b *Binding) { b.NodeIncarnationID = "incarnation-1" },
		},
		{
			// The fencing value the execution-authority decision had to withdraw for pushes: C runs on
			// no Node, so there is no generation to bind and no check that would read one.
			name:  "a push that carries a runtime generation",
			base:  pushBinding,
			apply: func(b *Binding) { b.RuntimeGeneration = 3 },
		},
		{
			name:  "a push that carries a control epoch",
			base:  pushBinding,
			apply: func(b *Binding) { b.ControlEpoch = 7 },
		},
		{
			name:  "a push that carries a preparation attempt",
			base:  pushBinding,
			apply: func(b *Binding) { b.PreparationGeneration = 1 },
		},
		{
			name:  "a push that carries no credential",
			base:  pushBinding,
			apply: func(b *Binding) { b.CredentialID = "" },
		},
		{
			name:  "a push that carries no upstream",
			base:  pushBinding,
			apply: func(b *Binding) { b.Repository = "" },
		},
		{
			name:  "a push that asks for the read capability as well",
			base:  pushBinding,
			apply: func(b *Binding) { b.Capabilities = []string{"clone", "push"} },
		},
		{
			name:  "a push that asks for the read capability instead",
			base:  pushBinding,
			apply: func(b *Binding) { b.Capabilities = []string{"clone"} },
		},
		{
			name:  "a clone that carries a commit pair",
			base:  testBinding,
			apply: func(b *Binding) { b.OldCommit, b.NewCommit = testOldCommit, testNewCommit },
		},
		{
			name:  "a mirror preparation that carries a commit pair",
			base:  mirrorBinding,
			apply: func(b *Binding) { b.OldCommit, b.NewCommit = testOldCommit, testNewCommit },
		},
	}

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			_, private := testKeys(t)
			binding := test.base()
			test.apply(&binding)
			if _, err := Issue(private, binding); !errors.Is(err, ErrIncomplete) {
				t.Fatalf("Issue accepted a binding it must refuse: %v", err)
			}
		})
	}
}

// TestAPushAuthorizationIsNotReadableAsEitherReadOperation pins what the operation is for: a consumer
// distinguishes the three purposes from the verified binding alone, so a write authorization cannot
// be spent by a path that reads — and cannot be mistaken for one by a path that writes.
func TestAPushAuthorizationIsNotReadableAsEitherReadOperation(t *testing.T) {
	public, private := testKeys(t)
	token, err := Issue(private, pushBinding())
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	got, err := verifyOne(public, token, testIssued())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got.Operation != OperationPush {
		t.Fatalf("a push verified as %q, want %q", got.Operation, OperationPush)
	}
	// Everything a read operation would need to identify its own work is absent, and everything the
	// write needs is present. This is the property the Store and the executor rest on: neither has to
	// infer the purpose from which route the request arrived on.
	if got.ExecutionID != "" || got.NodeID != "" || got.NodeInstanceID != "" ||
		got.NodeIncarnationID != "" || got.RuntimeGeneration != 0 || got.ControlEpoch != 0 {
		t.Fatalf("a push carried a runtime: %+v", got)
	}
	if got.PreparationGeneration != 0 {
		t.Fatalf("a push carried a preparation attempt: %+v", got)
	}
	if got.OperationID == "" || got.ExecutorIdentity == "" || got.Ref == "" ||
		got.OldCommit == "" || got.NewCommit == "" {
		t.Fatalf("a push lost its own subject: %+v", got)
	}
}

// TestAnUnknownOperationIsNotReadAsAPush is the direction the operation resolution takes now that the
// vocabulary has more than one member: an operation this build does not define is refused rather than
// defaulted to one of the defined ones.
//
// It lives next to the push binding because `push` is the value that used to be the example of an
// unknown operation, and it must not become the example of a default.
func TestAnUnknownOperationIsNotReadAsAPush(t *testing.T) {
	_, private := testKeys(t)
	for _, operation := range []string{"", "PUSH", "Push", "push ", " force", "delete"} {
		binding := pushBinding()
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
