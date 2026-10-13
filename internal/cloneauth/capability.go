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
// The vocabulary is fixed by decision rather than by this package, and since 2026-10-13 it has exactly
// two members. The credentials decision's D2 wrote V1 as "`capabilities` 第一版只有 `["clone"]`；它不
// 蕴含 fetch／pull／push", and the push decision
// (`20261013-push-capability-vocabulary-extension`, `approved` 2026-10-13) added the second: `push`,
// with `clone` still first. Widening this list, or changing its order, is therefore a decision to be
// taken rather than a constant to be appended here — `TestCapabilityVocabularyIsClosedToWhatTheDecisionFixes`
// fails until the decision exists.
type Capability string

// CapabilityClone permits reading one repository into one checkout.
//
// The spelling matches `credential_refs.capabilities` and the published contract's enumeration, so
// the value a credential declares, the value the authorization binds and the value the executor
// checks are one string rather than three that have to be kept in step.
const CapabilityClone Capability = "clone"

// CapabilityPush permits one remote write: updating one fully qualified ref of one upstream
// repository from one commit to another.
//
// It is a separate member rather than a write flag on CapabilityClone because a capability is a
// property of the reference and is the surface every later operation is authorized against. Folding
// read and write into one member would give every reference that needs only a clone a write grant as
// well, and a revocation could not say which half it withdrew (push decision D1).
//
// Its existence changes nothing about any reference. A row that says `["clone"]` still says
// `["clone"]`: no default, migration or deployment-managed reference gains this member, and every
// enforcement point — admission (`internal/core`, which admits only work whose required capability
// the reference permits), signing (`permittedCapabilities`, which permits `push` to `OperationPush`
// alone), execution (`internal/cloneexec`, whose push job re-compares the set before it touches the
// network) and redemption (`internal/credentialstore`, which serves the write purpose only to an
// authorization minted for it) — still refuses what it does not serve.
//
// Nor does it enable anything in a deployment by itself. The dispatcher and the executor's push
// route are installed from configuration, and where they are absent the work is refused rather than
// falling back to something else.
const CapabilityPush Capability = "push"

// capabilities is the complete vocabulary in its canonical order.
//
// The order is part of the signed payload's spelling rather than a presentation choice:
// `canonicalCapabilities` refuses a set that is not in this order, so "the capabilities the issuer
// signed" and "the capabilities the executor compares" are one value rather than two that a
// normalization step could disagree about.
var capabilities = []Capability{CapabilityClone, CapabilityPush}

// KnownCapability reports whether a value names a capability this vocabulary defines.
//
// It answers membership in the vocabulary, not whether a particular deployment can perform the work
// the capability describes: every member is performed by the executor today, but whether a deployment
// installed that component is a deployment fact, and a component that does not serve a capability
// refuses it on its own terms rather than by this function answering false.
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
