package repoexec

import (
	"context"
	"strings"
	"time"
)

// sourceLocalRef holds the publish source fetched from the Git Server.
const sourceLocalRef = "refs/ora-source/head"

// verifyReserve is how much of the task budget is kept for the post-push ls-remote, so a push that
// runs out of time still leaves room to learn whether it took effect.
func verifyReserve(budget time.Duration) time.Duration {
	reserve := budget / 4
	return min(max(reserve, 500*time.Millisecond), 30*time.Second)
}

// publish fast-forwards the external target ref to a commit taken from the Git Server (ADR D6),
// comparing and swapping against expectedOldOid, and classifies by the push report and a post-push
// ls-remote (ADR D8). It never retries the push.
func (t *taskRun) publish(ctx context.Context) Result {
	task := t.task
	server := t.e.gitServer.repository(task.TenantID, task.ProjectID)

	// Step 2: the source, only the Operation's recorded ref and commit. The executor has no write
	// access to the Git Server.
	if o := t.run(ctx, "fetch", "--quiet", "--no-tags", "--no-write-fetch-head", "--no-recurse-submodules", "--", server, "+"+task.SourceRef+":"+sourceLocalRef); !o.ok() {
		if strings.Contains(o.stderr, "couldn't find remote ref") {
			return Result{Classification: SourceUnavailable, ErrorCode: CodeSourceUnavailable, Detail: "the source ref does not exist on the Git Server"}
		}
		return t.fail(ctx, gitServerFailure(o.stderr), &o)
	}
	if o := t.run(ctx, "cat-file", "-e", task.SourceCommit+"^{commit}"); !o.ok() {
		if ctx.Err() != nil {
			return t.fail(ctx, CodeInternal, &o)
		}
		return Result{Classification: SourceUnavailable, ErrorCode: CodeSourceUnavailable, Detail: "the source commit is not reachable from the source ref"}
	}
	if o := t.run(ctx, "merge-base", "--is-ancestor", task.SourceCommit, sourceLocalRef); !o.ok() {
		if o.exit == 1 {
			return Result{Classification: SourceUnavailable, ErrorCode: CodeSourceUnavailable, Detail: "the source commit is not reachable from the source ref"}
		}
		return t.fail(ctx, CodeInternal, &o)
	}

	// Step 3: fast-forward only, decided from the Git Server's objects alone, before the external
	// repository is contacted at all.
	if !zeroOID(task.ExpectedOldOID) {
		known := t.run(ctx, "cat-file", "-e", task.ExpectedOldOID+"^{commit}")
		if ctx.Err() != nil {
			return t.fail(ctx, CodeInternal, &known)
		}
		ancestor := known
		if known.ok() {
			ancestor = t.run(ctx, "merge-base", "--is-ancestor", task.ExpectedOldOID, task.SourceCommit)
		}
		if !ancestor.ok() {
			if ancestor.exit != 1 && known.ok() || ctx.Err() != nil {
				return t.fail(ctx, CodeInternal, &ancestor)
			}
			return Result{Classification: NotFastForward, ErrorCode: CodeNotFastForward, Detail: "expectedOldOid is not an ancestor of the source commit"}
		}
	}

	// Step 4: pre-check without opening receive-pack.
	pre, failure := t.lsRemoteTarget(ctx, ctx)
	if failure != nil {
		return *failure
	}
	if !sameOID(pre, task.ExpectedOldOID) {
		observed := orZero(pre, task.ExpectedOldOID)
		return Result{Classification: Conflict, ErrorCode: "", ObservedOID: observed, PreObservedOID: observed, Detail: "the external ref no longer holds expectedOldOid"}
	}

	// Step 5: compare-and-swap push. The push gets the budget minus a reserve for step 6.
	deadline, _ := ctx.Deadline()
	reserve := verifyReserve(t.task.Timeout())
	pushCtx, cancelPush := context.WithDeadline(ctx, deadline.Add(-reserve))
	external, refusal := t.externalRepository(pushCtx)
	if refusal != nil {
		cancelPush()
		return *refusal
	}
	push := t.git.run(pushCtx, "push", "--porcelain", "--no-verify",
		"--force-with-lease="+task.TargetRef+":"+task.ExpectedOldOID,
		"--", external, task.SourceCommit+":"+task.TargetRef)
	pushEnded := pushCtx.Err() != nil
	cancelPush()
	outcome := outcomeUnknown
	if !push.stdoutCut {
		if line, ok := parsePorcelain(push.stdout)[task.TargetRef]; ok {
			outcome = line.outcome()
		}
	}
	if outcome == outcomeUnknown && !pushEnded && pushNeverSent(push.stderr) {
		// Ref discovery failed (authentication or authorization): no update command was sent.
		return Result{Classification: Failed, ErrorCode: CodePushDenied, PreObservedOID: orZero(pre, task.ExpectedOldOID), Detail: redact(push.stderr, t.secrets)}
	}

	// Step 6: post-check, on its own bounded window that the task's end does not cut short, so even a
	// push killed by the deadline or by shutdown is verified once.
	verifyCtx, cancelVerify := context.WithTimeout(context.WithoutCancel(ctx), reserve)
	defer cancelVerify()
	post, postFailure := t.lsRemoteTarget(verifyCtx, ctx)
	classification, confirmed := classifyPublish(outcome, post, postFailure == nil, task.ExpectedOldOID, task.SourceCommit)
	result := Result{Classification: classification, ConfirmedByVerification: confirmed, PreObservedOID: orZero(pre, task.ExpectedOldOID)}
	if postFailure == nil {
		result.PostObservedOID = orZero(post, task.ExpectedOldOID)
		result.ObservedOID = result.PostObservedOID
	} else {
		result.ObservedOID = result.PreObservedOID
	}
	switch classification {
	case Applied:
		if postFailure == nil && post != task.SourceCommit {
			result.Detail = "the external ref changed again after the publish"
		}
		if confirmed {
			result.Detail = strings.TrimSpace("the result was confirmed by verification " + result.Detail)
		}
	case Rejected:
		result.ErrorCode = CodeExternalRejected
		result.Detail = redact(push.stdout+"\n"+push.stderr, t.secrets)
	default:
		result.Detail = redact(push.stderr, t.secrets)
	}
	return result
}

// lsRemoteTarget reads the target ref of the external repository; "" means absent. outer is the
// task context, used only to attribute a failure to the task's own end.
func (t *taskRun) lsRemoteTarget(ctx, outer context.Context) (string, *Result) {
	external, refusal := t.externalRepository(ctx)
	if refusal != nil {
		return "", refusal
	}
	o := t.git.run(ctx, "ls-remote", "--refs", "--", external, t.task.TargetRef)
	if !o.ok() || o.stdoutCut {
		r := t.fail(outer, externalFailure(o.stderr, t.anonymous), &o)
		if ctx.Err() != nil && outer.Err() == nil {
			r = failed(CodeTimeout, "verification exceeded its window")
		}
		return "", &r
	}
	refs, ok := parseLsRemote(o.stdout)
	if !ok || len(refs) > 1 {
		r := failed(CodeFetchFailed, "ls-remote answer is unreadable")
		return "", &r
	}
	return refs[t.task.TargetRef], nil
}

// orZero spells an absent ref as the zero OID of the expected value's length.
func orZero(observed, like string) string {
	if observed != "" {
		return observed
	}
	return strings.Repeat("0", max(len(like), 40))
}
