package cloneauth

import (
	"bytes"
	"encoding/base64"
	"errors"
	"reflect"
	"testing"
)

// TestValidateRef proves the mirror purpose's ref is checkable before it can be signed, so a
// preparation can never name something other than a branch, or something a Git command would read as
// an option rather than as a name.
func TestValidateRef(t *testing.T) {
	cases := []struct {
		name string
		ref  string
		want error
	}{
		{"a branch", "refs/heads/main", nil},
		{"a nested branch", "refs/heads/feature/nested/name", nil},
		{"a branch with a dot", "refs/heads/release-1.2", nil},
		{"empty", "", ErrIncomplete},
		{"a short branch name", "main", ErrIncomplete},
		{"a tag", "refs/tags/v1.0.0", ErrIncomplete},
		{"a remote-tracking ref", "refs/remotes/origin/main", ErrIncomplete},
		{"the namespace alone", "refs/heads/", ErrIncomplete},
		{"an empty component", "refs/heads//main", ErrIncomplete},
		{"a leading dash", "refs/heads/-main", ErrIncomplete},
		{"a trailing slash", "refs/heads/main/", ErrIncomplete},
		{"a dot-dot", "refs/heads/../main", ErrIncomplete},
		{"an at-brace", "refs/heads/main@{1}", ErrIncomplete},
		{"a space", "refs/heads/ma in", ErrIncomplete},
		{"a tilde", "refs/heads/ma~in", ErrIncomplete},
		{"a caret", "refs/heads/ma^in", ErrIncomplete},
		{"a colon", "refs/heads/ma:in", ErrIncomplete},
		{"a question mark", "refs/heads/ma?in", ErrIncomplete},
		{"a star", "refs/heads/ma*in", ErrIncomplete},
		{"a backslash", `refs/heads/ma\in`, ErrIncomplete},
		{"a lock suffix", "refs/heads/main.lock", ErrIncomplete},
		{"a dot suffix", "refs/heads/main.", ErrIncomplete},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if err := ValidateRef(test.ref); !errors.Is(err, test.want) {
				t.Fatalf("ValidateRef(%q) = %v, want %v", test.ref, err, test.want)
			}
		})
	}
}

// mirrorBinding returns the one valid binding for the mirror purpose, so each mirror test states only
// what it changes about the purpose rather than restating the whole scope.
//
// The identity fields swap outright rather than being added to: a mirror preparation is a step of a
// Project Operation executed by a named service identity, so it carries no execution and no Node
// runtime, and the clone fields are cleared to say so.
func mirrorBinding() Binding {
	binding := testBinding()
	binding.Operation = OperationMirrorPrepare
	binding.Ref = "refs/heads/main"
	binding.OperationID = "operation-1"
	binding.PreparationGeneration = 1
	binding.ExecutorIdentity = "spiffe://ora.test/executor/mirror"
	binding.ExecutionID = ""
	binding.NodeID = ""
	binding.NodeInstanceID = ""
	binding.NodeIncarnationID = ""
	return binding
}

// TestMirrorPreparationRoundTrip proves the mirror purpose survives the wire with its ref intact and
// stays distinguishable from a clone, which is what lets a consumer refuse a token minted for the
// other purpose rather than having to infer which one it received.
func TestMirrorPreparationRoundTrip(t *testing.T) {
	public, private := testKeys(t)
	want := mirrorBinding()
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
	if got.Operation == OperationClone {
		t.Fatal("a verified mirror preparation is indistinguishable from a clone")
	}
}

// TestTheRefIsInsideTheSignedPayload proves the pinned ref is signed rather than merely transported.
// The edit below changes only the ref's bytes and keeps the issuer's signature, which is exactly what
// an attacker holding a token for one branch would attempt in order to retarget it at another.
func TestTheRefIsInsideTheSignedPayload(t *testing.T) {
	public, private := testKeys(t)
	token, err := Issue(private, mirrorBinding())
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	payload, signature, err := split(token)
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	edited := bytes.Replace(payload, []byte("refs/heads/main"), []byte("refs/heads/attacker"), 1)
	if bytes.Equal(edited, payload) {
		t.Fatal("the signed payload does not contain the ref, so this test would prove nothing")
	}
	forged := base64.RawURLEncoding.EncodeToString(edited) + "." + base64.RawURLEncoding.EncodeToString(signature)
	if _, err := verifyOne(public, forged, testIssued()); !errors.Is(err, ErrSignature) {
		t.Fatalf("a payload whose ref was repointed verified: %v", err)
	}
}
