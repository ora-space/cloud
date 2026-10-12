package cloneauth

import (
	"fmt"
	"slices"
)

// Capability is one repository operation an authorization can permit.
//
// Capabilities are the currency of the continuity decision's D4: an execution plan fixes "引用身份、
// 版本、仓库与所需能力", and the executor may only act inside that plan. Putting the set in the signed
// binding is what makes the plan and the execution the same fact — without it the authorization would
// name a credential and a repository and leave "what may be done with them" to whatever the executor
// happens to support.
//
// The vocabulary is deliberately tiny because it is fixed by decision rather than by this package.
// The credentials decision's D2 states V1 in as many words: "`capabilities` 第一版只有 `["clone"]`；
// 它不蕴含 fetch／pull／push." A push capability is therefore not merely unimplemented — it is
// outside the approved vocabulary, and adding it is a decision to be taken rather than a constant to
// be appended here. A reader who finds only one value in this list is looking at the decision, not at
// an unfinished enum.
type Capability string

// CapabilityClone permits reading one repository into one checkout. It is the only capability V1
// defines, and the only one the credentialed-clone path exercises.
//
// The spelling matches `credential_refs.capabilities` and the published contract's enumeration, so
// the value a credential declares, the value the authorization binds and the value the executor
// checks are one string rather than three that have to be kept in step.
const CapabilityClone Capability = "clone"

// capabilities is the complete vocabulary in its canonical order.
var capabilities = []Capability{CapabilityClone}

// KnownCapability reports whether a value names a capability this build implements.
func KnownCapability(value Capability) bool {
	return slices.Contains(capabilities, value)
}

// canonicalCapabilities returns the canonical spelling of one capability set and reports whether the
// input was already canonical.
//
// Canonical means every member is a known capability, no member repeats, and the members appear in
// the vocabulary's own order. The input is not repaired: a set that is a valid set but not the
// canonical spelling of it is refused, so "the capabilities the issuer signed" and "the capabilities
// the executor compares" are one value rather than two a normalization step could disagree about.
func canonicalCapabilities(in []string) ([]string, bool) {
	if len(in) == 0 {
		return nil, false
	}
	seen := make(map[Capability]bool, len(in))
	for _, raw := range in {
		capability := Capability(raw)
		if !KnownCapability(capability) || seen[capability] {
			return nil, false
		}
		seen[capability] = true
	}
	canonical := make([]string, 0, len(in))
	for _, capability := range capabilities {
		if seen[capability] {
			canonical = append(canonical, string(capability))
		}
	}
	return canonical, slices.Equal(canonical, in)
}

// CanonicalCapabilities returns the one spelling of a capability set for storage and comparison, and
// an error naming the rule the set breaks.
//
// Issuers call this so the bytes they sign are already canonical, and callers that only compare a
// stored value call it to reject anything this package could not have issued.
func CanonicalCapabilities(in []string) ([]string, error) {
	canonical, ok := canonicalCapabilities(in)
	if !ok {
		return nil, fmt.Errorf("%w: capabilities are empty, unknown, repeated or out of order", ErrIncomplete)
	}
	return canonical, nil
}

// HasCapability reports whether a canonical capability set contains one capability.
func HasCapability(set []string, want Capability) bool {
	return slices.Contains(set, string(want))
}
