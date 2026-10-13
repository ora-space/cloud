// Package mirrorpublish writes and reconciles the internal Git Server's mirrors.
//
// The store in internal/mirror answers whether a mirror may be read. It deliberately does not answer
// what should exist: that question belongs to Cloud, because only Cloud's rows know which project,
// which upstream repository and which commit a Workspace is entitled to. This package is the writer
// that asks those rows, materializes exactly the mirrors they name, and withdraws the ones they stop
// naming.
//
// The authorization is structural rather than a convention. [Reconciler.Reconcile] is the only entry
// point, it takes Cloud's answer as its first act, and nothing in this package can name a key that
// answer does not contain: a process that can write the mirror volume still cannot publish a mirror
// Cloud has not bound to a tenant, a repository and a clone execution. That is D2's "the writer is C"
// made checkable — it is not a permission the filesystem grants.
//
// What this package does not do is decide that a clone may happen. Cloud writes a Workspace's clone
// input only after the mirror is readable (D7), and that write belongs to the control plane, not
// here.
package mirrorpublish

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/wanglongan587/cloud/internal/mirror"
)

// Reference is one mirror Cloud's authoritative rows require, together with the facts that make it
// fetchable.
type Reference struct {
	// Key is the immutable (project, upstream repository, commit) identity D6 keys mirrors by.
	Key mirror.Key
	// TenantID is the tenant Cloud's rows bind this mirror to. It is carried for logging and for the
	// per-tenant quota D6 reserves; it is not part of the key, because two tenants that name the same
	// upstream repository and commit are entitled to byte-identical content.
	TenantID string
	// Upstream is the branch Cloud recorded as where this commit came from. It is what makes the
	// fetch deterministic: the writer fetches that one branch and refuses to publish unless its tip
	// is exactly Key.Commit, so a branch that moved after Cloud pinned the commit produces no mirror
	// rather than a mirror of the wrong commit.
	Upstream string
}

// Project is one live Project and the upstream repository its rows currently name.
type Project struct {
	ID string
	// TenantID is the owning tenant.
	TenantID string
	// Repository is the canonical upstream identity, or empty when the stored URL is not one the
	// canonicalizer accepts. An empty value is why a mismatch must be proven rather than assumed: it
	// says "this project cannot be compared", not "this project points somewhere else".
	Repository string
}

// Binding is Cloud's whole answer for one reconciliation: which Projects exist and what they point
// at, and which mirror keys its rows reference right now.
type Binding struct {
	// Projects holds every live Project. It is the complete set, not only the ones with a pinned
	// Workspace, because a Project that changed its upstream must withdraw the mirrors of the
	// upstream it left even when no Workspace pins one any more.
	Projects []Project
	// Live holds the referenced keys: the ones a read could be authorized for right now, and the ones
	// a clone that has not settled is about to consume. Both are derived from the same two facts the
	// consumers derive them from — the Workspace's pinned baseline for the first, the preparation's
	// published commit for the second — so this list is the keys Cloud's rows name, not a second
	// opinion about them.
	Live []Reference
}

// references reports whether Cloud's answer contains this exact key.
func (b Binding) references(key mirror.Key) bool {
	for i := range b.Live {
		if b.Live[i].Key == key {
			return true
		}
	}
	return false
}

// upstream returns the live Project with this identifier.
func (b Binding) upstream(projectID string) (Project, bool) {
	for i := range b.Projects {
		if b.Projects[i].ID == projectID {
			return b.Projects[i], true
		}
	}
	return Project{}, false
}

// keys is the live set in the form internal/mirror's sweep takes it.
func (b Binding) keys() []mirror.Key {
	keys := make([]mirror.Key, 0, len(b.Live))
	for i := range b.Live {
		keys = append(keys, b.Live[i].Key)
	}
	return keys
}

// Binder answers which mirrors Cloud currently requires. It is the consuming boundary's own
// interface so a deployment can be tested without PostgreSQL, and so the writer cannot acquire any
// other way of learning what to publish.
type Binder interface {
	Binding(ctx context.Context) (Binding, error)
}

// Fetcher materializes the content of one referenced commit into a staging directory the caller
// owns and has emptied.
//
// It reports the ref and commit it actually materialized, rather than the ones it was asked for:
// the store verifies that answer against the key, so a fetcher that returns the request back
// without checking would publish a mirror whose content is not the commit it claims.
//
// The reference is a pointer for the same reason the clone executor's job is: it is what the
// authorization is compared against, and the copy the fetch reads is the one the pass bound, so a
// caller that kept the value cannot edit what is being fetched.
type Fetcher interface {
	Fetch(ctx context.Context, reference *Reference, destination string) (mirror.PublishVerification, error)
}

// Report is what one reconciliation did. It exists so an operator reads a reclaim instead of
// assuming one, and so a failure names the key it belongs to rather than only a count.
type Report struct {
	// Ready lists keys Cloud references and that are readable once this pass finished, whether they
	// were published now or were already there. It is the answer to "may Cloud write the clone
	// input?", and it is derived from the store rather than from what this pass attempted.
	Ready []mirror.Key
	// Published lists the keys this pass materialized.
	Published []mirror.Key
	// Failed lists keys this pass could not materialize; their records are in the failed state,
	// which is a state a reader refuses rather than one they serve.
	Failed []mirror.Key
	// Revoked lists keys withdrawn because the Project they belong to now names a different upstream
	// repository.
	Revoked []mirror.Key
	// Skipped lists referenced keys a previous withdrawal still covers. A revoked key is durable, so
	// a pass that cannot publish one says so instead of retrying it forever in silence.
	Skipped []mirror.Key
	// Sweep is what the store reclaimed: unreferenced mirrors past their grace period, abandoned
	// staging directories and published trees no record names.
	Sweep mirror.SweepReport
}

// Reconciler is the mirror writer. Every field is a deployment-fixed dependency: no caller passes a
// key, a limit or an upstream, so no request can widen what is published.
type Reconciler struct {
	// Mirrors is the store whose volume this writer owns.
	Mirrors *mirror.Store
	// Bindings answers what Cloud requires. It is the authorization this writer runs under.
	Bindings Binder
	// Fetcher materializes content. It is the only component here that reaches the network.
	Fetcher Fetcher
	// Clock stamps the sweep. Nil means time.Now.
	Clock func() time.Time
	// Logger records what happened. Nil discards.
	Logger *slog.Logger
}

// ErrNotAuthorized reports a key Cloud's rows do not reference. Publishing it would put content in
// the store that no tenant, repository and clone execution bind, which is exactly what D2 forbids.
var ErrNotAuthorized = errors.New("mirror is not referenced by Cloud")

// Reconcile performs one pass: it withdraws what Cloud stopped naming, publishes what Cloud requires,
// and then sweeps the store against Cloud's live set.
//
// The order matters and is not an optimization. Withdrawals come first so a Project that changed its
// upstream stops being served before anything new is published for it; the sweep comes last and is
// given Cloud's answer rather than the store's own records, because a store that treats its records
// as evidence of being wanted would let a mirror keep itself alive (D6).
func (r *Reconciler) Reconcile(ctx context.Context) (Report, error) {
	var report Report
	if r.Mirrors == nil || r.Bindings == nil || r.Fetcher == nil {
		return report, errors.New("the reconciler needs a mirror store, a binder and a fetcher")
	}
	binding, err := r.Bindings.Binding(ctx)
	if err != nil {
		// Nothing is published and nothing is withdrawn on a question this pass could not answer: an
		// unavailable Cloud must not read as "nothing is referenced", which would withdraw the store.
		return report, fmt.Errorf("read Cloud's mirror references: %w", err)
	}
	records, err := r.Mirrors.Records()
	if err != nil {
		return report, fmt.Errorf("read the stored mirrors: %w", err)
	}
	revoked, err := r.withdrawAll(ctx, binding, records)
	if err != nil {
		return report, err
	}
	report.Revoked = revoked
	for i := range binding.Live {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		key, outcome, err := r.materialize(ctx, binding, &binding.Live[i])
		if err != nil {
			// One key's failure is not the pass's failure: the remaining keys are independent, and a
			// single unreachable upstream must not stop every other tenant's mirror from being
			// published. The failure is counted and named, and the next pass retries it.
			report.Failed = append(report.Failed, key)
			r.log().WarnContext(ctx, "mirror could not be published", "mirror", key.String(), "error", err.Error())
			continue
		}
		switch outcome {
		case outcomePublished:
			report.Published = append(report.Published, key)
		case outcomeReused:
		case outcomeSkipped:
			report.Skipped = append(report.Skipped, key)
			continue
		}
		report.Ready = append(report.Ready, key)
	}
	sweep, err := r.Mirrors.Sweep(ctx, binding.keys(), r.now())
	if err != nil {
		return report, fmt.Errorf("sweep the mirror store: %w", err)
	}
	report.Sweep = sweep
	return report, nil
}

// Maintain reconciles the store against Cloud's live set without publishing anything: it withdraws
// the mirrors of Projects that moved to another upstream and sweeps what nothing references any more.
//
// It is a separate entry point from Reconcile rather than a mode of it because the step it omits is
// one this deployment cannot take, not one it chose not to. Publishing a mirror in this channel means
// fetching a private upstream as the credential-executing identity C, and C does that by redeeming a
// single-use authorization Cloud signs for one Operation — bound to that Operation's tenant, Project,
// Workspace, credential version, ref and runtime generation (see `mirror_preparations`). A
// reconciliation pass is not an Operation: it has no tenant to bind, no credential version to pin and
// no generation to fence a late result against, so there is no authorization such a fetch could be
// made under. The alternative — fetching with the store's own access and no authorization at all —
// would be an unauthorized path into the mirror volume, and for a private upstream it would fail and
// leave a live key's record in the state readers refuse. Publication therefore stays with the
// per-Operation worker, and this pass keeps the store honest about what is still wanted.
//
// The withdrawal and the sweep are the same code Reconcile runs; what differs is only that nothing
// here writes content.
func (r *Reconciler) Maintain(ctx context.Context) (Report, error) {
	var report Report
	if r.Mirrors == nil || r.Bindings == nil {
		return report, errors.New("the maintainer needs a mirror store and a binder")
	}
	binding, err := r.Bindings.Binding(ctx)
	if err != nil {
		// As in Reconcile: a question that could not be answered must not read as "nothing is
		// referenced", which would start every mirror's grace period and eventually remove the store.
		return report, fmt.Errorf("read Cloud's mirror references: %w", err)
	}
	records, err := r.Mirrors.Records()
	if err != nil {
		return report, fmt.Errorf("read the stored mirrors: %w", err)
	}
	revoked, err := r.withdrawAll(ctx, binding, records)
	if err != nil {
		return report, err
	}
	report.Revoked = revoked
	for i := range binding.Live {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		// Ready means "Cloud references this key and the store can serve it", which is the same answer
		// Reconcile's Ready gives and the answer to "may a clone input name this mirror?". A referenced
		// key the store does not have is reported by its absence, and publishing it is the worker's
		// job, not this pass's.
		if record, found, err := r.Mirrors.Lookup(binding.Live[i].Key); err != nil {
			return report, fmt.Errorf("read the record for %s: %w", binding.Live[i].Key.String(), err)
		} else if found && record.Readable() {
			report.Ready = append(report.Ready, binding.Live[i].Key)
		}
	}
	sweep, err := r.Mirrors.Sweep(ctx, binding.keys(), r.now())
	if err != nil {
		return report, fmt.Errorf("sweep the mirror store: %w", err)
	}
	report.Sweep = sweep
	return report, nil
}

// withdrawAll applies withdraw to every stored record, stopping at the first error.
func (r *Reconciler) withdrawAll(ctx context.Context, binding Binding, records []mirror.Record) ([]mirror.Key, error) {
	var revoked []mirror.Key
	for i := range records {
		if err := ctx.Err(); err != nil {
			return revoked, err
		}
		keys, err := r.withdraw(binding, &records[i])
		if err != nil {
			return revoked, err
		}
		revoked = append(revoked, keys...)
	}
	return revoked, nil
}

// withdraw applies D6's first revocation trigger to one stored record: the Project it belongs to now
// names a different upstream repository, so the content is for a repository that project no longer
// clones from and must never be served again — not even to a Workspace that still pins its commit.
//
// A key whose Project still exists and still names the same repository is left alone. That covers
// both the referenced case and the "nothing references it any more" case, and the second is the
// sweep's job: it is a reclaim after a grace period, not a withdrawal. A withdrawal is durable, so
// it may only be applied to a mismatch that was actually observed.
func (r *Reconciler) withdraw(binding Binding, record *mirror.Record) ([]mirror.Key, error) {
	if binding.references(record.Key) {
		return nil, nil
	}
	project, live := binding.upstream(record.Key.ProjectID)
	if !live || project.Repository == "" || project.Repository == record.Key.Repository {
		return nil, nil
	}
	if record.State == mirror.StateRevoked || record.State == mirror.StateDestroyed {
		// Already withdrawn or already reclaimed. Revoking a destroyed record would write a record
		// over content that is gone and make the sweep's accounting lie.
		return nil, nil
	}
	if _, err := r.Mirrors.Revoke(record.Key, mirror.ReasonWithdrawn); err != nil {
		return nil, fmt.Errorf("withdraw the mirror for %s: %w", record.Key.String(), err)
	}
	r.log().Info("mirror withdrawn: the project changed its upstream",
		"mirror", record.Key.String(), "project", project.ID)
	return []mirror.Key{record.Key}, nil
}

// outcome is what materialize did with one referenced key.
type outcome int

const (
	outcomePublished outcome = iota
	outcomeReused
	outcomeSkipped
)

// materialize makes one referenced key readable.
//
// The authorization check comes first and is not redundant with Reconcile's loop: it is what makes
// the assertion "no mirror is published that Cloud does not reference" hold for every path into this
// function, including a later one that does not loop over the binding.
func (r *Reconciler) materialize(ctx context.Context, binding Binding, reference *Reference) (mirror.Key, outcome, error) {
	key := reference.Key
	if !binding.references(key) {
		return key, outcomeSkipped, fmt.Errorf("%w: %s", ErrNotAuthorized, key.String())
	}
	record, found, err := r.Mirrors.Lookup(key)
	if err != nil {
		return key, outcomeSkipped, err
	}
	if found && record.Readable() {
		// The content of a key is immutable and the same key is what a second Workspace of the same
		// Project resolves to, so an existing readable mirror is reused rather than materialized
		// again (D6).
		return key, outcomeReused, nil
	}
	prep, err := r.Mirrors.Begin(ctx, key)
	if err != nil {
		if errors.Is(err, mirror.ErrRevoked) {
			// A withdrawal is durable by design: an ordinary pass may not undo one. Reporting it as a
			// skip keeps the pass from failing while making the condition visible.
			return key, outcomeSkipped, err
		}
		return key, outcomeSkipped, err
	}
	defer func() { _ = prep.Close() }()
	verification, err := r.Fetcher.Fetch(ctx, reference, prep.Dir())
	if err != nil {
		return key, outcomeSkipped, r.fail(prep, "the fetch failed", err)
	}
	if verification.Commit != key.Commit {
		return key, outcomeSkipped, r.fail(prep, "the fetch materialized another commit",
			fmt.Errorf("fetched %s but the key pins %s", verification.Commit, key.Commit))
	}
	if _, err := prep.Publish(ctx, verification); err != nil {
		// Publish has already recorded the failure it found (a size or volume limit is terminal for
		// this attempt and the staged content is discarded), so this only reports it.
		return key, outcomeSkipped, err
	}
	r.log().Info("mirror published", "mirror", key.String(), "tenant", reference.TenantID, "ref", verification.Ref)
	return key, outcomePublished, nil
}

// fail abandons a preparation with the writer's own reason code and returns the cause.
func (r *Reconciler) fail(prep *mirror.Preparation, message string, cause error) error {
	if _, err := prep.Fail(mirror.ReasonWriterFailed); err != nil {
		return fmt.Errorf("%s (%v) and the preparation could not be failed: %w", message, cause, err)
	}
	return fmt.Errorf("%s: %w", message, cause)
}

func (r *Reconciler) now() time.Time {
	if r.Clock == nil {
		return time.Now()
	}
	return r.Clock()
}

func (r *Reconciler) log() *slog.Logger {
	if r.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return r.Logger
}
