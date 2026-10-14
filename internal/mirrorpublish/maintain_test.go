package mirrorpublish

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/internal/mirror"
)

// Maintain is the reclaim loop `cmd/git-server` runs. It exists because the process that owns the
// mirror volume and the process that can read Cloud's rows are different ones, so the only loop that
// can reconcile a stored mirror against Cloud's answer is the one that has both. These tests pin the
// two halves of what it may do — withdraw and sweep — and the one thing it may not do, which is
// publish.

// TestMaintainNeverPublishesTheKeysCloudReferences is the boundary between the reclaim loop and the
// per-Operation worker. A referenced key the store does not have is not this pass's work: publishing
// it would mean fetching a private upstream outside the authorization Cloud signs for one Operation,
// and the fetcher below is a working one, so a pass that called it would publish and be caught.
func TestMaintainNeverPublishesTheKeysCloudReferences(t *testing.T) {
	h := newHarness(t, time.Hour)
	reference := referenceFor(testProject, testRepoA, testCommitA)
	h.binder.binding = bindingFor(testProject, testRepoA, reference)

	report, err := h.subject.Maintain(context.Background())
	if err != nil {
		t.Fatalf("Maintain: %v", err)
	}
	if len(report.Published) != 0 {
		t.Fatalf("the reclaim pass published %v", report.Published)
	}
	if len(h.fetcher.calls) != 0 {
		t.Fatalf("the reclaim pass fetched %d times; a reconciliation is not an Operation and has no authorization to fetch under", len(h.fetcher.calls))
	}
	// Ready is answered from the store rather than from what the pass attempted, so a referenced key
	// with no mirror behind it is absent rather than reported as ready.
	if len(report.Ready) != 0 {
		t.Fatalf("a key with no mirror was reported ready: %v", report.Ready)
	}
	if _, err := h.store.Open(reference.Key); !errors.Is(err, mirror.ErrNotReady) {
		t.Fatalf("Open on a key the reclaim pass must not have published: %v", err)
	}
}

// TestMaintainReportsWhatTheStoreCanAlreadyServe is the other half of Ready: a key Cloud references
// whose content is there, published by the worker in an earlier pass, is what the reclaim loop answers
// "may a clone input name this mirror?" from.
func TestMaintainReportsWhatTheStoreCanAlreadyServe(t *testing.T) {
	h := newHarness(t, time.Hour)
	published := referenceFor(testProject, testRepoA, testCommitA)
	missing := referenceFor(testProject, testRepoB, testCommitB)
	h.binder.binding = bindingFor(testProject, testRepoA, published)
	if _, err := h.subject.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	// The project is rebound to the second repository, so both keys are Cloud's: one the store holds
	// and one it does not. Rebounding also makes the first key a withdrawal candidate, which is why
	// the second reference is added before the pass that asserts Ready.
	h.binder.binding = Binding{
		Projects: []Project{{ID: testProject, TenantID: testTenant, Repository: testRepoB}},
		Live:     []Reference{published, missing},
	}
	h.subject.Fetcher = nil

	report, err := h.subject.Maintain(context.Background())
	if err != nil {
		t.Fatalf("Maintain: %v", err)
	}
	if len(report.Ready) != 1 || report.Ready[0] != published.Key {
		t.Fatalf("Ready = %v, want exactly the key the store can serve", report.Ready)
	}
	if len(report.Revoked) != 0 {
		t.Fatalf("a referenced key was withdrawn: %v", report.Revoked)
	}
}

// TestMaintainWithdrawsASupersededMirrorAndSweepsWhatNothingWants walks one pass over a store holding
// two mirrors: one whose Project moved to another upstream, which is a withdrawal, and one nothing
// references at all, whose Project still names its repository, which is a reclaim question rather than
// a security one. A pass must do the first outright and only start the clock on the second.
func TestMaintainWithdrawsASupersededMirrorAndSweepsWhatNothingWants(t *testing.T) {
	h := newHarness(t, time.Hour)
	superseded := referenceFor(testProject, testRepoA, testCommitA)
	abandoned := referenceFor(testOtherRepo, testRepoA, testCommitB)
	h.binder.binding = Binding{
		Projects: []Project{
			{ID: testProject, TenantID: testTenant, Repository: testRepoA},
			{ID: testOtherRepo, TenantID: testTenant, Repository: testRepoA},
		},
		Live: []Reference{superseded, abandoned},
	}
	if _, err := h.subject.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	// The first Project now names another repository; the second Project is gone from Cloud's answer
	// entirely, which is a Project that cannot be compared and therefore must not withdraw anything.
	h.binder.binding = Binding{
		Projects: []Project{{ID: testProject, TenantID: testTenant, Repository: testRepoB}},
	}
	report, err := h.subject.Maintain(context.Background())
	if err != nil {
		t.Fatalf("Maintain: %v", err)
	}
	if len(report.Revoked) != 1 || report.Revoked[0] != superseded.Key {
		t.Fatalf("Revoked = %v, want exactly the superseded key", report.Revoked)
	}
	// Both mirrors are unreferenced now, so both start a grace period: the withdrawn one because a
	// withdrawal makes content unreadable rather than deleting it, and the abandoned one because
	// nothing names it. The bytes go when that period passes, which is the next test's subject.
	if !containsKey(report.Sweep.ObservedUnreferenced, superseded.Key) {
		t.Fatalf("the withdrawn mirror did not start a grace period: %v", report.Sweep.ObservedUnreferenced)
	}
	if !containsKey(report.Sweep.ObservedUnreferenced, abandoned.Key) {
		t.Fatalf("the abandoned mirror did not start a grace period: %v", report.Sweep.ObservedUnreferenced)
	}
	if len(report.Sweep.Destroyed) != 0 {
		t.Fatalf("the pass destroyed %v inside the grace period", report.Sweep.Destroyed)
	}
	// Two different outcomes, and the difference is the point. The withdrawn mirror is unreadable from
	// this pass on, while the merely abandoned one is untouched: a grace period is a delay, not a
	// refusal, and a Workspace being torn down must not take another Workspace's mirror with it.
	if _, err := h.store.Open(superseded.Key); !errors.Is(err, mirror.ErrNotReady) {
		t.Fatalf("the superseded mirror is still readable: %v", err)
	}
	if _, err := h.store.Open(abandoned.Key); err != nil {
		t.Fatalf("the abandoned mirror was made unreadable before its grace period passed: %v", err)
	}
	if record, found, err := h.store.Lookup(superseded.Key); err != nil || !found || record.State != mirror.StateRevoked {
		t.Fatalf("the superseded mirror's record = %+v found=%v err=%v, want a revoked record", record, found, err)
	}
}

// TestMaintainReclaimsOnceTheGracePeriodHasPassed is why the loop exists at all: with publication left
// to the per-Operation worker, an abandoned mirror is only ever removed by a pass that keeps asking.
// Two passes with the clock moved between them are the whole lifecycle — the first starts the clock,
// the second destroys — and the volume is what the assertion is about, not the report.
func TestMaintainReclaimsOnceTheGracePeriodHasPassed(t *testing.T) {
	h := newHarness(t, time.Hour)
	reference := referenceFor(testProject, testRepoA, testCommitA)
	h.binder.binding = bindingFor(testProject, testRepoA, reference)
	if _, err := h.subject.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if _, err := h.store.Open(reference.Key); err != nil {
		t.Fatalf("the mirror is not readable after publication: %v", err)
	}

	// Cloud stops referencing it; the Project still names the same repository, so this is a reclaim.
	h.binder.binding = bindingFor(testProject, testRepoA)
	first, err := h.subject.Maintain(context.Background())
	if err != nil {
		t.Fatalf("first Maintain: %v", err)
	}
	if len(first.Sweep.ObservedUnreferenced) != 1 || len(first.Sweep.Destroyed) != 0 {
		t.Fatalf("first pass = %+v, want the grace period to start and nothing destroyed", first.Sweep)
	}

	h.now = h.now.Add(time.Hour)
	second, err := h.subject.Maintain(context.Background())
	if err != nil {
		t.Fatalf("second Maintain: %v", err)
	}
	if len(second.Sweep.Destroyed) != 1 || second.Sweep.Destroyed[0] != reference.Key {
		t.Fatalf("second pass destroyed %v, want exactly the abandoned key", second.Sweep.Destroyed)
	}
	if _, err := h.store.Open(reference.Key); !errors.Is(err, mirror.ErrNotReady) {
		t.Fatalf("the reclaimed mirror is still readable: %v", err)
	}
}

// TestMaintainRemovesNothingWhenCloudCannotAnswer is the fail-closed case, and it is the reason the
// reference read comes first. An unreachable Cloud must not read as "nothing is referenced": a pass
// that answered that way would start every mirror's grace period at once, and the deployment would
// destroy its whole store during one database outage.
func TestMaintainRemovesNothingWhenCloudCannotAnswer(t *testing.T) {
	h := newHarness(t, time.Hour)
	reference := referenceFor(testProject, testRepoA, testCommitA)
	h.binder.binding = bindingFor(testProject, testRepoA, reference)
	if _, err := h.subject.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	h.binder.err = errors.New("the database is unavailable")

	report, err := h.subject.Maintain(context.Background())
	if err == nil {
		t.Fatal("Maintain answered a question it could not ask")
	}
	if len(report.Revoked) != 0 || len(report.Sweep.Destroyed) != 0 || len(report.Sweep.ObservedUnreferenced) != 0 {
		t.Fatalf("a pass with no answer removed something: %+v", report)
	}
	if _, err := h.store.Open(reference.Key); err != nil {
		t.Fatalf("the mirror did not survive an unanswered pass: %v", err)
	}
}

// TestMaintainRequiresItsDependencies keeps a half-wired deployment from reading as a healthy one: a
// maintainer with no database answer would otherwise report "nothing referenced, nothing done" while
// looking exactly like a maintainer that had checked.
func TestMaintainRequiresItsDependencies(t *testing.T) {
	h := newHarness(t, time.Hour)
	for name, subject := range map[string]Reconciler{
		"no mirror store": {Bindings: h.binder},
		"no binder":       {Mirrors: h.store},
		"neither":         {},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := subject.Maintain(context.Background()); err == nil {
				t.Fatal("Maintain ran without its dependencies")
			}
		})
	}
}

// TestMaintainDoesNotNeedAFetcher keeps the entry points honestly separate: Reconcile cannot run
// without a fetcher because publishing is what it does, and Maintain must run without one because
// publishing is what it must never do.
func TestMaintainDoesNotNeedAFetcher(t *testing.T) {
	h := newHarness(t, time.Hour)
	h.subject.Fetcher = nil
	if _, err := h.subject.Maintain(context.Background()); err != nil {
		t.Fatalf("Maintain without a fetcher: %v", err)
	}
	if _, err := h.subject.Reconcile(context.Background()); err == nil {
		t.Fatal("Reconcile ran without a fetcher")
	}
}

// containsKey reports whether a report names this key. The sweep lists keys in the store's own record
// order, which is the digest order rather than the order a test published them in.
func containsKey(keys []mirror.Key, want mirror.Key) bool {
	for i := range keys {
		if keys[i] == want {
			return true
		}
	}
	return false
}
