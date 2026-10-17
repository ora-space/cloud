package repoexec

import (
	"context"
	"strings"
)

// Local refs of the task repository during a sync.
const (
	syncRef = "refs/heads/ora-sync"
	baseRef = "refs/ora-base/tracking"
)

func snapshotRef(binding, commit string) string {
	return "refs/ora/upstreams/" + binding + "/snapshots/" + commit
}

func trackingRef(binding, branch string) string {
	return "refs/ora/upstreams/" + binding + "/heads/" + branch
}

// sync fetches the external branch and writes the snapshot and tracking refs to the Git Server in one
// atomic push (ADR D5). It reports facts only; Cloud confirms them with a ref query.
func (t *taskRun) sync(ctx context.Context) Result {
	task := t.task
	server := t.e.gitServer.repository(task.TenantID, task.ProjectID)
	tracking := trackingRef(task.BindingID, task.Branch)
	expected := task.ExpectedTrackingOID

	// Step 2 (optimization): start from the current tracking ref so step 3 downloads only the delta.
	// Skipped on the first sync, when the Project repository may not even exist; any failure only
	// gives up the optimization.
	if !zeroOID(expected) {
		_ = t.run(ctx, "fetch", "--quiet", "--no-tags", "--no-write-fetch-head", "--no-recurse-submodules", "--", server, "+"+tracking+":"+baseRef)
		if ctx.Err() != nil {
			return t.fail(ctx, CodeInternal, &output{})
		}
	}

	// Step 3: the hardened external fetch.
	external, refusal := t.externalRepository(ctx)
	if refusal != nil {
		return *refusal
	}
	if o := t.run(ctx, "fetch", "--quiet", "--no-tags", "--no-write-fetch-head", "--no-recurse-submodules", "--", external, "+refs/heads/"+task.Branch+":"+syncRef); !o.ok() {
		return t.fail(ctx, externalFailure(o.stderr, t.anonymous), &o)
	}
	refs := t.run(ctx, "for-each-ref", "--format=%(refname)")
	if !refs.ok() || refs.stdoutCut {
		return t.fail(ctx, CodeInternal, &refs)
	}
	for _, ref := range strings.Fields(refs.stdout) {
		if ref != syncRef && ref != baseRef {
			return failed(CodeFetchFailed, "the fetch wrote an unexpected ref")
		}
	}

	// Step 4: the commit and whether the external branch was rewritten.
	resolved := t.run(ctx, "rev-parse", "--verify", "--quiet", syncRef+"^{commit}")
	commit := strings.TrimSpace(resolved.stdout)
	if !resolved.ok() || !validOID(commit) {
		return t.fail(ctx, CodeFetchFailed, &resolved)
	}
	forced, failure := t.forced(ctx, expected, commit)
	if failure != nil {
		return *failure
	}

	// Step 5: one atomic push. The refspecs carry no '+': with it Git treats the ref as forced and the
	// lease no longer applies, not even "must not exist". Without it the lease rejects a stale
	// expected value, and a matching one still admits a non-fast-forward (the service listener turns
	// receive.denyNonFastForwards off) so the tracking ref follows an external force-push.
	snapshot := snapshotRef(task.BindingID, commit)
	push := t.run(ctx, "push", "--atomic", "--porcelain", "--no-verify",
		"--force-with-lease="+tracking+":"+expected,
		"--", server, commit+":"+snapshot, commit+":"+tracking)
	result := Result{CommitOID: commit, SnapshotRef: snapshot, TrackingRef: tracking, Forced: forced}
	if ctx.Err() != nil || push.stdoutCut {
		r := t.fail(ctx, CodeGitServerUnavailable, &push)
		return withSync(&r, &result)
	}
	report := parsePorcelain(push.stdout)
	snapLine, snapOK := report[snapshot]
	trackLine, trackOK := report[tracking]
	if push.ok() && snapOK && trackOK && snapLine.outcome() == outcomeOK && trackLine.outcome() == outcomeOK {
		// Includes the replay: both refs already hold the commit and Git sends nothing.
		result.Classification = Applied
		return result
	}
	switch {
	case trackOK && trackLine.outcome() == outcomeStale:
		return t.stale(ctx, server, &result)
	case snapOK && snapLine.outcome() == outcomeRejected, trackOK && trackLine.outcome() == outcomeRejected:
		// pre-receive (or the Git Server's own policy) declined: the Operation ended, its lease
		// expired, or a ref was outside its grant. Never read as superseded.
		r := failed(CodeGitServerRejected, redact(push.stderr+"\n"+snapLine.summary+"\n"+trackLine.summary, t.secrets))
		return withSync(&r, &result)
	case !snapOK && !trackOK:
		r := failed(gitServerFailure(push.stderr), redact(push.stderr, t.secrets))
		return withSync(&r, &result)
	}
	r := failed(CodeGitServerRejected, redact(push.stderr, t.secrets))
	return withSync(&r, &result)
}

// forced reports whether the external branch no longer contains the expected tracking commit. If the
// expected commit is not in the task repository at all, it cannot be an ancestor of the fetched
// commit: fetching a commit brings all of its ancestors.
func (t *taskRun) forced(ctx context.Context, expected, commit string) (bool, *Result) {
	if zeroOID(expected) {
		return false, nil
	}
	if o := t.run(ctx, "cat-file", "-e", expected+"^{commit}"); !o.ok() {
		if ctx.Err() != nil {
			r := t.fail(ctx, CodeInternal, &o)
			return false, &r
		}
		return true, nil
	}
	o := t.run(ctx, "merge-base", "--is-ancestor", expected, commit)
	switch {
	case o.ok():
		return false, nil
	case o.exit == 1:
		return true, nil
	}
	r := t.fail(ctx, CodeInternal, &o)
	return false, &r
}

// stale handles a lease that did not hold: the tracking ref is not expectedTrackingOid. If it already
// holds this commit and its snapshot exists, the same result was written before (a replay racing an
// earlier attempt) and the sync is applied; otherwise someone else advanced it and the sync is
// superseded, with the value seen.
func (t *taskRun) stale(ctx context.Context, server string, result *Result) Result {
	result.Classification = Superseded
	o := t.run(ctx, "ls-remote", "--refs", "--", server, result.TrackingRef, result.SnapshotRef)
	if !o.ok() || o.stdoutCut {
		result.Detail = "the tracking ref could not be read again"
		return *result
	}
	refs, ok := parseLsRemote(o.stdout)
	if !ok {
		result.Detail = "the tracking ref could not be read again"
		return *result
	}
	observed := refs[result.TrackingRef]
	if observed == result.CommitOID && refs[result.SnapshotRef] == result.CommitOID {
		result.Classification = Applied
		return *result
	}
	if observed == "" {
		observed = strings.Repeat("0", len(result.CommitOID))
	}
	result.ObservedOID = observed
	return *result
}

// withSync keeps what the sync already established (commit, refs) on a failure result.
func withSync(r, facts *Result) Result {
	r.CommitOID, r.SnapshotRef, r.TrackingRef, r.Forced = facts.CommitOID, facts.SnapshotRef, facts.TrackingRef, facts.Forced
	return *r
}
