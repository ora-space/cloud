package cloneauth

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// TestCapabilityVocabularyIsClosedToWhatTheDecisionFixes pins the vocabulary to the two decisions
// that fix it: the credentials decision's D2 wrote V1 as `["clone"]` and stated it implies neither
// fetch, pull nor push, and the push decision
// (`20261013-push-capability-vocabulary-extension`) added `push` as the second member with `clone`
// still first, leaving every judgment rule unchanged.
//
// The test exists because widening this list, or reordering it, is a decision rather than an edit. If
// someone adds a value, this fails and points at the sentences they have to change first, instead of
// letting a new capability appear in signed authorizations because a constant looked incomplete.
func TestCapabilityVocabularyIsClosedToWhatTheDecisionFixes(t *testing.T) {
	want := []Capability{CapabilityClone, CapabilityPush}
	if !reflect.DeepEqual(capabilities, want) {
		t.Fatalf("capability vocabulary = %v, want exactly %v in that order", capabilities, want)
	}
	// Every value the vocabulary is not, including the ones the credentials decision named as
	// deliberately absent and the spellings that differ from a member only by case.
	for _, outside := range []Capability{"read", "write", "fetch", "pull", "admin", "Clone", "PUSH", "push ", ""} {
		if KnownCapability(outside) {
			t.Fatalf("%q is not in the vocabulary but KnownCapability accepted it", outside)
		}
	}
	for _, member := range want {
		if !KnownCapability(member) {
			t.Fatalf("KnownCapability(%q) = false, want true", member)
		}
	}
}

// TestCanonicalCapabilitiesRefusesEverySetItCannotHaveIssued covers the inputs a caller can reach
// CanonicalCapabilities with. Each rejection is a direction the issuer could otherwise encode into a
// signed payload: an empty set would read as "no restriction", an unknown value as a capability the
// executor might act on, a repeat as a set whose size is not its content, and a non-canonical order
// as a second spelling of a set the executor compares byte for byte.
func TestCanonicalCapabilitiesRefusesEverySetItCannotHaveIssued(t *testing.T) {
	cases := []struct {
		name      string
		in        []string
		canonical []string
	}{
		{name: "the read capability alone", in: []string{"clone"}, canonical: []string{"clone"}},
		{name: "the write capability alone", in: []string{"push"}, canonical: []string{"push"}},
		{name: "both, in the vocabulary's order", in: []string{"clone", "push"}, canonical: []string{"clone", "push"}},
		{name: "both, in the other order", in: []string{"push", "clone"}},
		{name: "nothing at all", in: nil},
		{name: "an empty set", in: []string{}},
		{name: "a capability outside the vocabulary", in: []string{"fetch"}},
		{name: "a capability the vocabulary rejected before it had two members", in: []string{"clone", "fetch"}},
		{name: "the same capability twice", in: []string{"clone", "clone"}},
		{name: "the write capability twice", in: []string{"push", "push"}},
		{name: "a differently cased spelling", in: []string{"Clone"}},
		{name: "an empty string member", in: []string{""}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			got, err := CanonicalCapabilities(test.in)
			if test.canonical == nil {
				if !errors.Is(err, ErrIncomplete) {
					t.Fatalf("CanonicalCapabilities(%v) error = %v, want ErrIncomplete", test.in, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("CanonicalCapabilities(%v) error = %v, want success", test.in, err)
			}
			if !reflect.DeepEqual(got, test.canonical) {
				t.Fatalf("CanonicalCapabilities(%v) = %v, want %v", test.in, got, test.canonical)
			}
		})
	}
}

// TestCanonicalCapabilitiesRefusesASetThatIsNotAlreadyCanonical pins the difference between
// canonicalizing and repairing, which is only observable now that the vocabulary has two members.
//
// A set that is a valid set but not the canonical spelling of it must be refused rather than
// reordered, because this function is also the comparison a consumer runs over a stored value: if it
// returned a reordered answer, then "the capabilities the issuer signed" and "the capabilities the
// executor compares" would be two values a normalization step could disagree about.
func TestCanonicalCapabilitiesRefusesASetThatIsNotAlreadyCanonical(t *testing.T) {
	// The set is a real set and the returned spelling is the same set, but the answer says it was not
	// already canonical. A caller that read only the returned value would repair it silently; the
	// second return is what lets every caller refuse instead.
	if got, ok := canonicalCapabilities([]string{"push", "clone"}); ok || !reflect.DeepEqual(got, []string{"clone", "push"}) {
		t.Fatalf("canonicalCapabilities accepted an out-of-order set: %v, %v", got, ok)
	}
	got, ok := canonicalCapabilities([]string{"clone", "push"})
	if !ok || !reflect.DeepEqual(got, []string{"clone", "push"}) {
		t.Fatalf("canonicalCapabilities([clone push]) = %v, %v, want the same set and true", got, ok)
	}
}

// TestIssueCanonicalizesBeforeItJudges is the observable consequence of the second vocabulary member.
//
// `Issue` brings a caller's set to its canonical spelling and only then judges it against the
// operation, so the order a caller passes never becomes part of what the signature covers. The two
// refusals below are what makes that visible: a set that is out of order fails on the set's own rule,
// while a set that is canonical but not the one the operation permits fails on the operation's. If
// the issuer judged the caller's spelling instead, the second case would be reported as the first —
// and the second case is exactly what the vocabulary extension has to answer rather than treat as a
// malformed member.
//
// `Verify` deliberately does not canonicalize: a signed payload that is not already canonical could
// not have been produced by `Issue`, so verification refuses it rather than repairing it — see
// TestAForbiddenFieldIsRefusedEvenWhenTheSignatureIsValid.
func TestIssueCanonicalizesBeforeItJudges(t *testing.T) {
	_, private := testKeys(t)

	outOfOrder := testBinding()
	outOfOrder.Capabilities = []string{"push", "clone"}
	if _, err := Issue(private, outOfOrder); !errors.Is(err, ErrIncomplete) ||
		!strings.Contains(err.Error(), "empty, unknown, repeated or out of order") {
		t.Fatalf("an out-of-order set was refused for the wrong reason: %v", err)
	}

	notPermitted := testBinding()
	notPermitted.Capabilities = []string{"clone", "push"}
	if _, err := Issue(private, notPermitted); !errors.Is(err, ErrIncomplete) ||
		!strings.Contains(err.Error(), "permits exactly") {
		t.Fatalf("a canonical set the operation does not permit was refused for the wrong reason: %v", err)
	}
}

// TestHasCapabilityComparesTheCanonicalSpelling proves the membership check cannot be answered by a
// string the package would never have issued. `HasCapability` is the one call site that turns a
// signed set into a decision, so a case-insensitive or partial match here would be the whole
// capability check failing open.
func TestHasCapabilityComparesTheCanonicalSpelling(t *testing.T) {
	cases := []struct {
		name string
		set  []string
		want Capability
		ok   bool
	}{
		{"the capability is present", []string{"clone"}, CapabilityClone, true},
		{"the second capability is present", []string{"clone", "push"}, CapabilityPush, true},
		{"a set that does not contain it", []string{"clone"}, CapabilityPush, false},
		{"the read capability is absent from a write set", []string{"push"}, CapabilityClone, false},
		{"the set is empty", []string{}, CapabilityClone, false},
		{"the set is nil", nil, CapabilityClone, false},
		{"a differently cased request", []string{"clone"}, "Clone", false},
		{"a request with surrounding space", []string{"clone"}, " clone", false},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if got := HasCapability(test.set, test.want); got != test.ok {
				t.Fatalf("HasCapability(%v, %q) = %v, want %v", test.set, test.want, got, test.ok)
			}
		})
	}
}
