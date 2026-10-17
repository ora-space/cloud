package repoworker

import (
	"context"
	"errors"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/wanglongan587/cloud/internal/cloneauth"
	"github.com/wanglongan587/cloud/internal/core"
	"github.com/wanglongan587/cloud/internal/repoexec"
)

// runOperation takes one leased Operation from leased through running to its settlement.
func (w *Worker) runOperation(ctx context.Context, task core.RepositoryTask) {
	log := w.cfg.Log.With(zap.String("operationId", task.OperationID), zap.String("type", task.Type))
	expected := cloneauth.ZeroOID40
	timeout := w.cfg.PublishTimeout
	if task.Type == core.RepositorySync {
		timeout = w.cfg.SyncTimeout
		// expected old is read as late as possible, right before the Operation runs (sync ADR D2):
		// never from the snapshot record, which may lag the tracking ref.
		commit, found, err := w.cfg.Refs.ResolveGitRef(ctx, task.TenantID, task.ProjectID, core.GitUpstreamHeadPrefix(task.BindingID)+task.Branch)
		if err != nil {
			log.Warn("tracking ref unreadable; returning the lease", zap.Error(err))
			w.logErr(ctx, "release", w.cfg.Store.ReleaseRepositoryLease(ctx, task.OperationID, task.LeaseGeneration))
			return
		}
		if found {
			expected = commit
		}
	}
	start, err := w.cfg.Store.StartRepositoryOperation(ctx, task.OperationID, task.LeaseGeneration, expected, w.cfg.Lease, timeout)
	if err != nil || start.Refused != "" {
		if err != nil && !core.IsLeaseLost(err) {
			log.Warn("starting the Operation failed", zap.Error(err))
		}
		return
	}
	request, err := w.authorize(&start, timeout)
	if err != nil {
		// Signing cannot fail for a row core accepted; settle it rather than leave it running.
		log.Error("signing the authorization failed", zap.Error(err))
		w.settleLost(task, start.Task.LeaseGeneration, "authorization_invalid", false)
		return
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	renewed := w.renew(runCtx, cancel, task.OperationID, task.LeaseGeneration)
	result, err := w.cfg.Executor.Run(runCtx, request)
	if task.Type == core.RepositorySync && err == nil && result.Classification == repoexec.Applied {
		// Keep renewing while the refs are verified: verification is part of running.
		w.settleSync(runCtx, &task, &result, log)
		cancel()
		<-renewed
		return
	}
	cancel()
	<-renewed
	if err != nil {
		w.settleCallFailure(task, err, log)
		return
	}
	settle, stop := settleContext()
	defer stop()
	if task.Type == core.RepositorySync {
		err = w.cfg.Store.SettleRepositorySync(settle, task.OperationID, task.LeaseGeneration, syncVerdict(&result))
	} else {
		err = w.cfg.Store.SettleRepositoryPublish(settle, task.OperationID, task.LeaseGeneration, publishVerdict(&result))
	}
	if err != nil && !core.IsLeaseLost(err) {
		log.Warn("settling the Operation failed", zap.Error(err))
	}
}

// authorize signs the one-time authorization and builds the executor task with the same fields.
func (w *Worker) authorize(s *core.RepositoryStart, timeout time.Duration) (*repoexec.Task, error) {
	a := cloneauth.Authorization{
		AuthorizationID: s.AuthorizationID, ExecutorIdentity: cloneauth.ExecutorIdentity, IssuedAt: s.IssuedAt, ExpiresAt: s.ExpiresAt,
		OperationID: s.Task.OperationID, TenantID: s.Task.TenantID, ProjectID: s.Task.ProjectID, BindingID: s.Task.BindingID,
		Repository: s.Task.RepositoryURL, CredentialID: s.CredentialID, CredentialVersion: s.CredentialVersion,
	}
	if s.Task.Type == core.RepositorySync {
		a.Purpose, a.Branch, a.ExpectedTrackingOID = cloneauth.PurposeSync, s.Task.Branch, s.ExpectedTrackingOID
	} else {
		a.Purpose, a.SourceRef, a.SourceCommit, a.TargetRef, a.ExpectedOldOID = cloneauth.PurposePublish, s.Task.SourceRef, s.Task.SourceCommit, "refs/heads/"+s.Task.TargetBranch, s.Task.ExpectedOldOID
	}
	token, err := cloneauth.Sign(w.cfg.Key, a)
	if err != nil {
		return nil, err
	}
	return taskFor(token, &a, timeout), nil
}

func taskFor(token string, a *cloneauth.Authorization, timeout time.Duration) *repoexec.Task {
	return &repoexec.Task{
		Authorization: token, Purpose: a.Purpose, OperationID: a.OperationID, VerificationID: a.VerificationID, TenantID: a.TenantID,
		ProjectID: a.ProjectID, BindingID: a.BindingID, Repository: a.Repository, Branch: a.Branch, ExpectedTrackingOID: a.ExpectedTrackingOID,
		SourceRef: a.SourceRef, SourceCommit: a.SourceCommit, TargetRef: a.TargetRef, ExpectedOldOID: a.ExpectedOldOID, Service: a.Service,
		CredentialID: a.CredentialID, CredentialVersion: a.CredentialVersion, TimeoutSeconds: int(timeout / time.Second),
	}
}

// renew extends the lease every third of its length until ctx ends. Losing the lease cancels the
// task: the sweep has already ended the Operation, and the Git Server refuses its ID anyway.
func (w *Worker) renew(ctx context.Context, cancel context.CancelFunc, id string, gen int64) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(w.cfg.Lease / 3)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := w.cfg.Store.RenewRepositoryLease(ctx, id, gen, w.cfg.Lease); err != nil {
					if core.IsLeaseLost(err) {
						cancel()
						return
					}
					w.logErr(ctx, "lease renewal", err)
				}
			}
		}
	}()
	return done
}

// settleSync verifies an applied sync through the ref query before Cloud may record it (contract
// D6): both the snapshot ref and the tracking ref must name the reported commit. Query failures
// are retried while the lease holds; when it lapses the sweep replays the sync.
func (w *Worker) settleSync(ctx context.Context, task *core.RepositoryTask, r *repoexec.Result, log *zap.Logger) {
	verdict := core.SyncResult{State: "succeeded", CommitOID: r.CommitOID, Forced: r.Forced, Detail: r.Detail}
	snapshot, tracking := core.GitSnapshotRef(task.BindingID, r.CommitOID), core.GitUpstreamHeadPrefix(task.BindingID)+task.Branch
	if !core.GitObjectID(r.CommitOID) || r.SnapshotRef != "" && r.SnapshotRef != snapshot || r.TrackingRef != "" && r.TrackingRef != tracking {
		verdict = core.SyncResult{State: "failed", ErrorCode: "verification_mismatch", Detail: r.Detail}
	} else {
		for delay := time.Second; ; delay = min(delay*2, 15*time.Second) {
			ok, err := w.verifyRefs(ctx, task, snapshot, tracking, r.CommitOID)
			if err == nil {
				if !ok {
					verdict = core.SyncResult{State: "failed", ErrorCode: "verification_mismatch", Detail: r.Detail}
				}
				break
			}
			log.Warn("verifying the sync refs failed; retrying within the lease", zap.Error(err))
			select {
			case <-ctx.Done():
				return
			case <-time.After(delay):
			}
		}
	}
	if err := w.cfg.Store.SettleRepositorySync(ctx, task.OperationID, task.LeaseGeneration, verdict); err != nil && !core.IsLeaseLost(err) {
		log.Warn("settling the sync failed", zap.Error(err))
	}
}

func (w *Worker) verifyRefs(ctx context.Context, task *core.RepositoryTask, snapshot, tracking, commit string) (bool, error) {
	for _, ref := range []string{snapshot, tracking} {
		got, found, err := w.cfg.Refs.ResolveGitRef(ctx, task.TenantID, task.ProjectID, ref)
		if err != nil {
			return false, err
		}
		if !found || got != commit {
			return false, nil
		}
	}
	return true, nil
}

// settleCallFailure records a task whose executor call itself failed. A task that never reached
// the executor, or that the executor refused, certainly did not run. A lost connection may have
// let it run: a sync is replayed (its writes are idempotent), a publish becomes uncertain.
func (w *Worker) settleCallFailure(task core.RepositoryTask, err error, log *zap.Logger) {
	var notDelivered *repoexec.NotDeliveredError
	var refused *repoexec.RefusedError
	switch {
	case errors.As(err, &refused):
		code := "executor_unreachable"
		if refused.Code == repoexec.CodeExecutorBusy {
			code = repoexec.CodeExecutorBusy
		}
		w.settleLost(task, task.LeaseGeneration, code, false)
	case errors.As(err, &notDelivered):
		w.settleLost(task, task.LeaseGeneration, "executor_unreachable", false)
	default:
		log.Warn("executor call lost", zap.Error(err))
		w.settleLost(task, task.LeaseGeneration, "executor_unreachable", true)
	}
}

// settleLost records an Operation whose task did not report. lost=true means the task may have
// acted.
func (w *Worker) settleLost(task core.RepositoryTask, gen int64, code string, lost bool) {
	ctx, cancel := settleContext()
	defer cancel()
	var err error
	if task.Type == core.RepositorySync {
		err = w.cfg.Store.SettleRepositorySync(ctx, task.OperationID, gen, core.SyncResult{State: "failed", ErrorCode: code})
	} else {
		r := core.PublishResult{Classification: string(repoexec.Failed), ErrorCode: code}
		if lost {
			r.Classification = string(repoexec.Uncertain)
		}
		err = w.cfg.Store.SettleRepositoryPublish(ctx, task.OperationID, gen, r)
	}
	if err != nil && !core.IsLeaseLost(err) {
		w.cfg.Log.Warn("settling an unreported Operation failed", zap.String("operationId", task.OperationID), zap.Error(err))
	}
}

func syncVerdict(r *repoexec.Result) core.SyncResult {
	switch r.Classification {
	case repoexec.Superseded:
		return core.SyncResult{State: "superseded", Observed: r.ObservedOID, Detail: r.Detail}
	case repoexec.Failed:
		return core.SyncResult{State: "failed", ErrorCode: r.ErrorCode, Detail: r.Detail}
	}
	// Any other class is not a sync outcome; recording it as a failure keeps the snapshot intact.
	return core.SyncResult{State: "failed", ErrorCode: "sync_failed", Detail: r.Detail}
}

func publishVerdict(r *repoexec.Result) core.PublishResult {
	return core.PublishResult{Classification: string(r.Classification), ErrorCode: r.ErrorCode, Detail: r.Detail, PreObserved: r.PreObservedOID, PostObserved: r.PostObservedOID, Confirmed: r.ConfirmedByVerification}
}

// runCheck reads an uncertain publish's target once with a probe authorization (publish ADR D5).
func (w *Worker) runCheck(ctx context.Context, c *core.RepositoryCheck) {
	settle := func(observed string, found bool, failure string) {
		sctx, cancel := settleContext()
		defer cancel()
		if err := w.cfg.Store.SettleRepositoryCheck(sctx, c.OperationID, c.Attempt, observed, found, failure); err != nil {
			w.cfg.Log.Warn("settling a publish check failed", zap.String("operationId", c.OperationID), zap.Error(err))
		}
	}
	if c.Problem != "" {
		settle("", false, c.Problem)
		return
	}
	a := cloneauth.Authorization{
		AuthorizationID: c.AuthorizationID, Purpose: cloneauth.PurposeProbe, ExecutorIdentity: cloneauth.ExecutorIdentity, IssuedAt: c.IssuedAt, ExpiresAt: c.ExpiresAt,
		OperationID: c.OperationID, TenantID: c.TenantID, ProjectID: c.ProjectID, BindingID: c.BindingID, Repository: c.RepositoryURL,
		TargetRef: c.TargetRef, Service: cloneauth.ServiceUploadPack, CredentialID: c.CredentialID, CredentialVersion: c.CredentialVersion,
	}
	token, err := cloneauth.Sign(w.cfg.Key, a)
	if err != nil {
		settle("", false, "authorization_invalid")
		return
	}
	result, err := w.cfg.Executor.Run(ctx, taskFor(token, &a, w.cfg.ProbeTimeout))
	switch {
	case err != nil:
		settle("", false, "executor_unreachable")
	case result.Classification != repoexec.Applied:
		settle("", false, failureCode(result.ErrorCode))
	default:
		observed, found := result.Refs[c.TargetRef]
		settle(observed, found, "")
	}
}

func failureCode(code string) string {
	if code == "" || strings.ContainsAny(code, " \n") {
		return "verification_failed"
	}
	return code
}

// runVerification proves (or disproves) a credential version's push capability by reading the
// receive-pack advertisement once (identity-access D4).
func (w *Worker) runVerification(ctx context.Context, c *core.CredentialCheck) {
	result := "verification_failed"
	a := cloneauth.Authorization{
		AuthorizationID: c.AuthorizationID, Purpose: cloneauth.PurposeProbe, ExecutorIdentity: cloneauth.ExecutorIdentity, IssuedAt: c.IssuedAt, ExpiresAt: c.ExpiresAt,
		VerificationID: c.VerificationID, TenantID: c.TenantID, Repository: c.Repository, Service: cloneauth.ServiceReceivePack,
		CredentialID: c.CredentialID, CredentialVersion: c.CredentialVersion,
	}
	if token, err := cloneauth.Sign(w.cfg.Key, a); err == nil {
		r, err := w.cfg.Executor.Run(ctx, taskFor(token, &a, w.cfg.ProbeTimeout))
		switch {
		case err != nil:
		case r.Classification == repoexec.Applied && r.Advertised:
			result = "verified"
		case r.ErrorCode == repoexec.CodePushDenied || r.ErrorCode == repoexec.CodeCredentialRejected:
			result = "push_denied"
		}
	}
	sctx, cancel := settleContext()
	defer cancel()
	if err := w.cfg.Store.SettleCredentialVerification(sctx, c.VerificationID, c.LeaseGeneration, result); err != nil {
		w.cfg.Log.Warn("settling a credential verification failed", zap.String("verificationId", c.VerificationID), zap.Error(err))
	}
}
