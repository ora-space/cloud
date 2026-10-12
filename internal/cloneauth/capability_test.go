package cloneauth

import (
	"errors"
	"testing"
)

// TestCapabilityVocabularyIsClosedToWhatTheDecisionFixes pins the vocabulary to the credentials
// decision's D2, which states V1 in as many words: capabilities is `["clone"]` and it does not imply
// fetch, pull or push.
//
// The test exists because widening this list is a decision rather than an edit. If someone adds a
// value, this fails and points at the sentence they have to change first, instead of letting a new
// capability appear in signed authorizations because a constant looked incomplete.
func TestCapabilityVocabularyIsClosedToWhatTheDecisionFixes(t *testing.T) {
	if len(capabilities) != 1 || capabilities[0] != CapabilityClone {
		t.Fatalf("capability vocabulary = %v, want exactly [%q]", capabilities, CapabilityClone)
	}
	for _, outside := range []Capability{"read", "write", "push", "fetch", "pull", "admin", ""} {
		if KnownCapability(outside) {
			t.Fatalf("%q is not in the V1 vocabulary but KnownCapability accepted it", outside)
		}
	}
	if !KnownCapability(CapabilityClone) {
		t.Fatalf("KnownCapability(%q) = false, want true", CapabilityClone)
	}
}

// TestCanonicalCapabilitiesRefusesEverySetItCannotHaveIssued covers the inputs a caller can reach
// CanonicalCapabilities with. Each rejection is a direction the issuer could otherwise encode into a
// signed payload: an empty set would read as "no restriction", an unknown value as a capability the
// executor might act on, and a repeat as a set whose size is not its content.
func TestCanonicalCapabilitiesRefusesEverySetItCannotHaveIssued(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want bool
	}{
		{"the one V1 capability", []string{"clone"}, true},
		{"nothing at all", nil, false},
		{"an empty set", []string{}, false},
		{"a capability outside V1", []string{"push"}, false},
		{"a capability V1 does not imply", []string{"clone", "fetch"}, false},
		{"the same capability twice", []string{"clone", "clone"}, false},
		{"a differently cased spelling", []string{"Clone"}, false},
		{"an empty string member", []string{""}, false},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			got, err := CanonicalCapabilities(test.in)
			if test.want {
				if err != nil {
					t.Fatalf("CanonicalCapabilities(%v) error = %v, want success", test.in, err)
				}
				if len(got) != 1 || got[0] != "clone" {
					t.Fatalf("CanonicalCapabilities(%v) = %v, want [clone]", test.in, got)
				}
				return
			}
			if !errors.Is(err, ErrIncomplete) {
				t.Fatalf("CanonicalCapabilities(%v) error = %v, want ErrIncomplete", test.in, err)
			}
		})
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
		{"another capability is asked for", []string{"clone"}, "push", false},
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

// There is deliberately no test here proving that `Issue` reorders a capability set before signing:
// the V1 vocabulary has one member, so there is no order to change and such a test would assert
// something it cannot demonstrate. The canonicalization in `Issue` is what the second member will
// need; until then only the rejection cases above are observable, and they are covered directly.
