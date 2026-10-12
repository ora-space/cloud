// Package mirror is the internal mirror store the read-only Git Server serves from.
//
// A mirror is one immutable copy of one commit of one upstream repository, taken by the credentialed
// executor C and published here. Nothing in this package contacts a network, holds a credential or
// runs Git: it owns the *lifecycle* — what exists, what is readable, when it stops being readable,
// and when it is destroyed — because that lifecycle is what a read authorization is decided against.
// Producing the content and verifying it with `git` stays with the writer, which is the only party
// that ever handled the upstream credential.
//
// The layout is three directories under one root, and the split is the visibility rule:
//
//	root/staging/<digest>.<nonce>/   an in-progress preparation; never readable
//	root/published/<digest>/gen-<n>/ one immutable published tree; readable only while the record says ready
//	root/records/<digest>.json       the mirror's state; written atomically, and the only thing a reader consults
//
// A reader follows the record first and the content second, in that order, so a mirror becomes
// readable only after its content is already in place, and stops being readable the moment the
// record says so — which is what makes revocation immediate while destruction can wait for a grace
// period (decisions/cloud/git-server/20261012-workspace-content-transport-over-the-internal-git-server
// D6, invariants 6, 8, 13).
package mirror

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/wanglongan587/cloud/internal/cloneauth"
)

// State is a mirror's lifecycle position. Only StateReady is readable.
type State string

const (
	// StatePreparing means content is being materialized. It is never readable, and a preparation
	// that never finishes leaves no record at all — only a staging directory a sweep reclaims.
	StatePreparing State = "preparing"
	// StateReady means the content was verified by the writer and published atomically.
	StateReady State = "ready"
	// StateFailed is terminal for this publication attempt: the content never became readable and is
	// gone. A later attempt for the same key starts again from StatePreparing.
	StateFailed State = "failed"
	// StateRevoked means the mirror was readable and deliberately stopped being so — the Workspace
	// it served is gone, the Project changed its upstream, or an operator withdrew it. Reads are
	// refused from the moment the record is written; the bytes follow after the grace period.
	StateRevoked State = "revoked"
	// StateDestroyed is terminal: the published content is gone from disk.
	StateDestroyed State = "destroyed"
)

// Key identifies one mirror. It is the unit a read authorization binds and the unit an orphan sweep
// reconciles, so its three fields are exactly the three facts that decide whether reading a mirror
// is legitimate: whose clone it serves, which upstream repository it came from, and which commit it
// pins.
type Key struct {
	// ProjectID is Cloud's project identifier. Two tenants may name the same upstream repository,
	// and two projects may point at the same commit; the project is what separates them.
	ProjectID string
	// Repository is the canonical upstream identity computed by cloneauth.CanonicalRepository, so the
	// mirror binds the same repository spelling the credential authorization binds. A mirror keyed by
	// a differently spelled URL would be a mirror nobody's authorization covers.
	Repository string
	// Commit is the exact commit the mirror pins.
	Commit string
}

// commitPattern accepts the object names Git writes today and keeps room for the wider ones.
var commitPattern = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)

// ErrKey reports a key that cannot name a mirror.
var ErrKey = errors.New("mirror key is not usable")

// Validate reports whether the key names a mirror at all.
func (k Key) Validate() error {
	if strings.TrimSpace(k.ProjectID) == "" {
		return fmt.Errorf("%w: it names no project", ErrKey)
	}
	canonical, err := cloneauth.CanonicalRepository(k.Repository)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrKey, err)
	}
	if canonical != k.Repository {
		// The key must already carry the canonical spelling: recomputing it here would let a caller
		// store two records for one repository, one per spelling, and only one of them would ever be
		// found by the reader that asks with the canonical form.
		return fmt.Errorf("%w: repository %q is not in canonical form (%q)", ErrKey, k.Repository, canonical)
	}
	if !commitPattern.MatchString(k.Commit) {
		return fmt.Errorf("%w: commit %q is not a full object name", ErrKey, k.Commit)
	}
	return nil
}

// Digest is the key's stable on-disk name. Fields are length-prefixed so no two distinct keys can
// hash the same bytes by moving a separator inside a field.
func (k Key) Digest() string {
	h := sha256.New()
	for _, field := range []string{k.ProjectID, k.Repository, k.Commit} {
		fmt.Fprintf(h, "%d:", len(field))
		h.Write([]byte(field))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// String renders the key for logs. It is not reversible and is not used as an identifier.
func (k Key) String() string {
	short := k.Commit
	if len(short) > 12 {
		short = short[:12]
	}
	return k.ProjectID + "/" + k.Repository + "@" + short
}

// Record is a mirror's persisted state. It is the only thing a reader consults to decide whether a
// mirror is readable, which is why revocation and destruction are record writes rather than file
// deletions.
type Record struct {
	Key Key `json:"key"`
	// State is the lifecycle position; see the State constants.
	State State `json:"state"`
	// Generation counts successful publications of this key. It starts at 1 and only ever grows, so an
	// authorization that binds a generation can tell "the mirror I approved" from "a later republication
	// under the same key".
	Generation int64 `json:"generation"`
	// Ref is the single ref the published tree exposes, and it points at the key's commit. A mirror with
	// a second ref would make the clone result depend on which ref the client asked for.
	Ref string `json:"ref"`
	// Bytes is the size of the published tree, measured at publication.
	Bytes int64 `json:"bytes"`
	// CreatedAt is when the preparation for this state began; ChangedAt is the last state transition.
	// Grace periods are measured from the latter, never from wall-clock guesses.
	CreatedAt time.Time `json:"createdAt"`
	ChangedAt time.Time `json:"changedAt"`
	// PublishedAt is set on the first successful publication of this generation and is zero otherwise.
	PublishedAt *time.Time `json:"publishedAt,omitempty"`
	// Reason is the stable code for why a failed or revoked mirror is not readable.
	Reason string `json:"reason,omitempty"`
	// UnreferencedSince is when a sweep first observed this key absent from Cloud's authoritative
	// references. It is what makes the grace period mean "unreferenced for this long" rather than
	// "old", and it is cleared as soon as the key is referenced again.
	UnreferencedSince *time.Time `json:"unreferencedSince,omitempty"`
}

// Readable reports whether the record permits serving content.
func (r *Record) Readable() bool { return r.State == StateReady }

// PublishVerification is the writer's proof that a staged mirror is what it claims to be.
//
// It exists so this package never runs Git. Integrity — every object present, the commit actually
// reachable, exactly one ref and that ref pointing at the commit — is a Git question, and the writer
// already has Git and the checkout it just produced. What this package enforces is that the proof
// matches the key: a writer cannot publish content for commit B under a key naming commit A.
type PublishVerification struct {
	// Commit must equal the key's commit.
	Commit string
	// Ref is the single ref the published tree exposes, in full (for example refs/ora/mirror).
	Ref string
}

// Validate reports whether the verification is complete enough to publish against key.
func (v PublishVerification) Validate(key Key) error {
	if v.Commit != key.Commit {
		return fmt.Errorf("verification commits %q but the key pins %q", v.Commit, key.Commit)
	}
	return validateRef(v.Ref)
}

// ErrRef reports a ref name that cannot be published.
var ErrRef = errors.New("mirror ref is not usable")

// validateRef accepts only a full, well-formed ref under refs/. A short name, a name with a Git
// metacharacter or a name that is not under refs/ would make the mirror's advertised ref depend on
// Git's own defaulting, which is exactly the ambiguity a pinned mirror exists to remove.
func validateRef(ref string) error {
	if !strings.HasPrefix(ref, "refs/") {
		return fmt.Errorf("%w: %q is not a full ref name", ErrRef, ref)
	}
	if strings.HasSuffix(ref, "/") || strings.Contains(ref, "//") || strings.HasSuffix(ref, ".") {
		return fmt.Errorf("%w: %q is malformed", ErrRef, ref)
	}
	if strings.ContainsAny(ref, " ~^:?*[\\") || strings.Contains(ref, "..") || strings.Contains(ref, "@{") {
		return fmt.Errorf("%w: %q contains a reserved sequence", ErrRef, ref)
	}
	return nil
}
