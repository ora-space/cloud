package repoworker

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/internal/cloneauth"
	"github.com/wanglongan587/cloud/internal/core"
	"github.com/wanglongan587/cloud/internal/repoexec"
)

func TestSyncReadsTheTrackingRefBeforeItStartsTheOperation(t *testing.T) {
	var mu sync.Mutex
	var order []string
	mark := func(what string) {
		mu.Lock()
		defer mu.Unlock()
		order = append(order, what)
	}
	store := &fakeStore{leased: []core.RepositoryTask{syncTask()}}
	store.startFn = func(call startCall) (core.RepositoryStart, error) {
		mark("start")
		return startFor(store.taskFor(call.id), call.expectedTracking), nil
	}
	refs := &fakeRefs{resolve: func(string) (string, bool, error) {
		mark("ref")
		return testCommit, true, nil
	}}
	exec := &fakeExecutor{run: func(context.Context, *repoexec.Task) (repoexec.Result, error) {
		return repoexec.Result{Classification: repoexec.Failed, ErrorCode: "fetch_failed"}, nil
	}}
	w := newWorker(t, store, exec, refs, nil)

	task := syncTask()
	w.runOperation(context.Background(), &task)

	// The expected old value is read as late as possible, so core must be handed it before the
	// Operation moves to running (sync ADR D2).
	if want := []string{"ref", "start"}; !reflect.DeepEqual(order, want) {
		t.Fatalf("call order = %v, want %v", order, want)
	}
	if got, want := refs.refs(), []string{core.GitUpstreamHeadPrefix(bindingID) + "main"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("refs queried = %v, want %v", got, want)
	}
	start, ok := store.lastStart()
	if !ok {
		t.Fatal("the Operation never started")
	}
	if start.expectedTracking != testCommit {
		t.Fatalf("expected tracking = %q, want %q", start.expectedTracking, testCommit)
	}
	if start.timeout != 3*time.Minute {
		t.Fatalf("start timeout = %v, want the sync budget", start.timeout)
	}
	calls := exec.calls()
	if len(calls) != 1 {
		t.Fatalf("executor calls = %d, want 1", len(calls))
	}
	got := calls[0]
	if got.Purpose != cloneauth.PurposeSync || got.ExpectedTrackingOID != testCommit || got.Branch != "main" ||
		got.Repository != repositoryURL || got.OperationID != syncOpID ||
		got.CredentialID != credentialID || got.CredentialVersion != 3 || got.TimeoutSeconds != 180 {
		t.Fatalf("executor task = %+v, want the sync's fields and the sync budget", got)
	}
	if got.Authorization == "" {
		t.Fatal("the executor task carries no authorization")
	}
}

func TestSyncReturnsTheLeaseWhenTheTrackingRefIsUnreadable(t *testing.T) {
	store := &fakeStore{leased: []core.RepositoryTask{syncTask()}}
	refs := &fakeRefs{resolve: func(string) (string, bool, error) {
		return "", false, errors.New("git server unavailable")
	}}
	exec := &fakeExecutor{}
	w := newWorker(t, store, exec, refs, nil)

	task := syncTask()
	w.runOperation(context.Background(), &task)

	if _, ok := store.lastStart(); ok {
		t.Fatal("the Operation started without an expected tracking value")
	}
	if got := store.ran("release"); got != 1 {
		t.Fatalf("release calls = %d, want 1", got)
	}
	if calls := exec.calls(); len(calls) != 0 {
		t.Fatalf("executor calls = %d, want none", len(calls))
	}
	if _, ok := store.settledSync(); ok {
		t.Fatal("a sync that never ran must not be settled")
	}
}

func TestRefusedStartNeverReachesTheExecutor(t *testing.T) {
	store := &fakeStore{leased: []core.RepositoryTask{publishTask()}}
	store.startFn = func(startCall) (core.RepositoryStart, error) {
		return core.RepositoryStart{Refused: "credential_unavailable"}, nil
	}
	exec := &fakeExecutor{}
	w := newWorker(t, store, exec, &fakeRefs{}, nil)

	task := publishTask()
	w.runOperation(context.Background(), &task)

	if calls := exec.calls(); len(calls) != 0 {
		t.Fatalf("executor calls = %d, want none", len(calls))
	}
	if _, ok := store.settledPublish(); ok {
		t.Fatal("core already ended the Operation; the worker must not settle it again")
	}
}

func TestStartErrorsLeaveTheOperationToCore(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"lease lost", core.ErrLeaseLost},
		{"unexpected failure", errors.New("connection reset")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeStore{leased: []core.RepositoryTask{publishTask()}}
			store.startFn = func(startCall) (core.RepositoryStart, error) {
				return core.RepositoryStart{}, tc.err
			}
			exec := &fakeExecutor{}
			w := newWorker(t, store, exec, &fakeRefs{}, nil)

			task := publishTask()
			w.runOperation(context.Background(), &task)

			if calls := exec.calls(); len(calls) != 0 {
				t.Fatalf("executor calls = %d, want none", len(calls))
			}
			if _, ok := store.settledPublish(); ok {
				t.Fatal("an Operation that never started must not be settled by the worker")
			}
		})
	}
}

func TestSyncSucceedsOnlyAfterBothRefsNameTheReportedCommit(t *testing.T) {
	store := &fakeStore{leased: []core.RepositoryTask{syncTask()}}
	refs := &fakeRefs{resolve: func(string) (string, bool, error) { return testCommit, true, nil }}
	snapshot := core.GitSnapshotRef(bindingID, testCommit)
	tracking := core.GitUpstreamHeadPrefix(bindingID) + "main"
	exec := &fakeExecutor{run: func(context.Context, *repoexec.Task) (repoexec.Result, error) {
		return repoexec.Result{Classification: repoexec.Applied, CommitOID: testCommit, SnapshotRef: snapshot, TrackingRef: tracking, Forced: true}, nil
	}}
	w := newWorker(t, store, exec, refs, nil)

	task := syncTask()
	w.runOperation(context.Background(), &task)

	settle, ok := store.settledSync()
	if !ok {
		t.Fatal("the sync was never settled")
	}
	want := core.SyncResult{State: "succeeded", CommitOID: testCommit, Forced: true}
	if settle.r != want {
		t.Fatalf("settled %+v, want %+v", settle.r, want)
	}
	if settle.id != syncOpID || settle.gen != 7 {
		t.Fatalf("settled (%q, %d), want (the sync operation, 7)", settle.id, settle.gen)
	}
	// The pre-start tracking read, then the snapshot and tracking verification.
	if got, want := refs.refs(), []string{tracking, snapshot, tracking}; !reflect.DeepEqual(got, want) {
		t.Fatalf("refs queried = %v, want %v", got, want)
	}
}

func TestSyncVerificationMismatches(t *testing.T) {
	tracking := core.GitUpstreamHeadPrefix(bindingID) + "main"
	for _, tc := range []struct {
		name    string
		result  repoexec.Result
		resolve func(ref string) (string, bool, error)
		queried int
	}{
		{
			name:    "the tracking ref names another commit",
			result:  repoexec.Result{Classification: repoexec.Applied, CommitOID: testCommit},
			resolve: func(ref string) (string, bool, error) { return "2222222222222222222222222222222222222222", true, nil },
			queried: 2,
		},
		{
			name:    "the tracking ref is gone",
			result:  repoexec.Result{Classification: repoexec.Applied, CommitOID: testCommit},
			resolve: func(ref string) (string, bool, error) { return "", false, nil },
			queried: 2,
		},
		{
			name: "the executor names an unrelated snapshot ref",
			result: repoexec.Result{
				Classification: repoexec.Applied, CommitOID: testCommit,
				SnapshotRef: core.GitSnapshotRef(otherBindingID, testCommit),
			},
			resolve: func(string) (string, bool, error) { return testCommit, true, nil },
			queried: 1,
		},
		{
			name:    "the executor reports no object id",
			result:  repoexec.Result{Classification: repoexec.Applied, SnapshotRef: core.GitSnapshotRef(bindingID, testCommit)},
			resolve: func(string) (string, bool, error) { return testCommit, true, nil },
			queried: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeStore{leased: []core.RepositoryTask{syncTask()}}
			refs := &fakeRefs{resolve: tc.resolve}
			exec := &fakeExecutor{run: func(context.Context, *repoexec.Task) (repoexec.Result, error) {
				return tc.result, nil
			}}
			w := newWorker(t, store, exec, refs, nil)

			task := syncTask()
			w.runOperation(context.Background(), &task)

			settle, ok := store.settledSync()
			if !ok {
				t.Fatal("the sync was never settled")
			}
			if settle.r.State != "failed" || settle.r.ErrorCode != "verification_mismatch" {
				t.Fatalf("settled %+v, want failed/verification_mismatch", settle.r)
			}
			if got := len(refs.refs()); got != tc.queried {
				t.Fatalf("refs queried %d times, want %d (tracking: %s)", got, tc.queried, tracking)
			}
		})
	}
}

func TestSyncVerificationIsRetriedWhileTheLeaseHolds(t *testing.T) {
	store := &fakeStore{leased: []core.RepositoryTask{syncTask()}}
	var mu sync.Mutex
	attempts := 0
	refs := &fakeRefs{resolve: func(string) (string, bool, error) {
		mu.Lock()
		defer mu.Unlock()
		attempts++
		if attempts == 2 {
			return "", false, errors.New("git server unavailable")
		}
		return testCommit, true, nil
	}}
	exec := &fakeExecutor{run: func(context.Context, *repoexec.Task) (repoexec.Result, error) {
		return repoexec.Result{Classification: repoexec.Applied, CommitOID: testCommit}, nil
	}}
	w := newWorker(t, store, exec, refs, nil)

	task := syncTask()
	w.runOperation(context.Background(), &task)

	settle, ok := store.settledSync()
	if !ok {
		t.Fatal("the sync was never settled")
	}
	if settle.r.State != "succeeded" {
		t.Fatalf("settled %+v, want a succeeded sync once the query recovered", settle.r)
	}
}

func TestSyncVerificationStopsWithoutSettlingWhenTheLeaseEnds(t *testing.T) {
	store := &fakeStore{leased: []core.RepositoryTask{syncTask()}}
	refs := &fakeRefs{resolve: func(string) (string, bool, error) {
		return "", false, errors.New("git server unavailable")
	}}
	exec := &fakeExecutor{run: func(context.Context, *repoexec.Task) (repoexec.Result, error) {
		return repoexec.Result{Classification: repoexec.Applied, CommitOID: testCommit}, nil
	}}
	w := newWorker(t, store, exec, refs, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	task := syncTask()
	w.runOperation(ctx, &task)

	// An unverified sync must never be recorded as anything: the lease sweep replays it.
	if settle, ok := store.settledSync(); ok {
		t.Fatalf("an unverified sync was settled as %+v", settle.r)
	}
}

func TestSyncVerdicts(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   repoexec.Result
		want core.SyncResult
	}{
		{
			name: "superseded keeps the observed value",
			in:   repoexec.Result{Classification: repoexec.Superseded, ObservedOID: "abc", Detail: "stale"},
			want: core.SyncResult{State: "superseded", Observed: "abc", Detail: "stale"},
		},
		{
			name: "failed keeps the executor's code",
			in:   repoexec.Result{Classification: repoexec.Failed, ErrorCode: "fetch_failed", Detail: "d"},
			want: core.SyncResult{State: "failed", ErrorCode: "fetch_failed", Detail: "d"},
		},
		{
			name: "a class that is not a sync outcome becomes sync_failed",
			in:   repoexec.Result{Classification: repoexec.Conflict, ErrorCode: "conflict"},
			want: core.SyncResult{State: "failed", ErrorCode: "sync_failed"},
		},
		{
			name: "an applied class that reached the verdict is not a success",
			in:   repoexec.Result{Classification: repoexec.Applied, CommitOID: testCommit},
			want: core.SyncResult{State: "failed", ErrorCode: "sync_failed"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := syncVerdict(&tc.in); *got != tc.want {
				t.Fatalf("syncVerdict(%+v) = %+v, want %+v", tc.in, *got, tc.want)
			}
		})
	}
}

func TestPublishVerdictCarriesTheWholeClassification(t *testing.T) {
	in := repoexec.Result{
		Classification: repoexec.Conflict, ErrorCode: "external_rejected", Detail: "d",
		PreObservedOID: "aaa", PostObservedOID: "bbb", ConfirmedByVerification: true,
	}
	want := core.PublishResult{
		Classification: "conflict", ErrorCode: "external_rejected", Detail: "d",
		PreObserved: "aaa", PostObserved: "bbb", Confirmed: true,
	}
	if got := publishVerdict(&in); *got != want {
		t.Fatalf("publishVerdict(%+v) = %+v, want %+v", in, *got, want)
	}
}

func TestExecutorCallFailuresSettleByWhetherTheTaskCouldHaveRun(t *testing.T) {
	for _, tc := range []struct {
		name    string
		err     error
		wantSy  core.SyncResult
		wantPub core.PublishResult
	}{
		{
			name:    "the executor refused because every slot is taken",
			err:     &repoexec.RefusedError{Status: 429, Code: repoexec.CodeExecutorBusy},
			wantSy:  core.SyncResult{State: "failed", ErrorCode: "executor_busy"},
			wantPub: core.PublishResult{Classification: "failed", ErrorCode: "executor_busy"},
		},
		{
			name:    "the executor refused for another reason",
			err:     &repoexec.RefusedError{Status: 403, Code: "identity"},
			wantSy:  core.SyncResult{State: "failed", ErrorCode: "executor_unreachable"},
			wantPub: core.PublishResult{Classification: "failed", ErrorCode: "executor_unreachable"},
		},
		{
			name:    "the task never reached the executor",
			err:     &repoexec.NotDeliveredError{Err: errors.New("dial tcp: connection refused")},
			wantSy:  core.SyncResult{State: "failed", ErrorCode: "executor_unreachable"},
			wantPub: core.PublishResult{Classification: "failed", ErrorCode: "executor_unreachable"},
		},
		{
			// A lost response may have let the push through, so a publish must stay reconcilable.
			name:    "the response was lost",
			err:     &repoexec.LostError{Err: errors.New("unexpected EOF")},
			wantSy:  core.SyncResult{State: "failed", ErrorCode: "executor_unreachable"},
			wantPub: core.PublishResult{Classification: "uncertain", ErrorCode: "executor_unreachable"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exec := &fakeExecutor{run: func(context.Context, *repoexec.Task) (repoexec.Result, error) {
				return repoexec.Result{}, tc.err
			}}

			store := &fakeStore{leased: []core.RepositoryTask{syncTask()}}
			w := newWorker(t, store, exec, &fakeRefs{}, nil)
			task := syncTask()
			w.runOperation(context.Background(), &task)
			settle, ok := store.settledSync()
			if !ok {
				t.Fatal("the sync was never settled")
			}
			if settle.r != tc.wantSy {
				t.Fatalf("sync settled %+v, want %+v", settle.r, tc.wantSy)
			}

			store = &fakeStore{leased: []core.RepositoryTask{publishTask()}}
			w = newWorker(t, store, exec, &fakeRefs{}, nil)
			task = publishTask()
			w.runOperation(context.Background(), &task)
			pub, ok := store.settledPublish()
			if !ok {
				t.Fatal("the publish was never settled")
			}
			if pub.r != tc.wantPub {
				t.Fatalf("publish settled %+v, want %+v", pub.r, tc.wantPub)
			}
		})
	}
}

func TestLosingTheLeaseCancelsTheRunningTask(t *testing.T) {
	store := &fakeStore{leased: []core.RepositoryTask{publishTask()}}
	store.renewFn = func(string, int64) error { return core.ErrLeaseLost }
	cancelled := make(chan struct{})
	exec := &fakeExecutor{run: func(ctx context.Context, _ *repoexec.Task) (repoexec.Result, error) {
		<-ctx.Done()
		close(cancelled)
		return repoexec.Result{}, ctx.Err()
	}}
	w := newWorker(t, store, exec, &fakeRefs{}, func(c *Config) { c.Lease = 30 * time.Millisecond })

	task := publishTask()
	w.runOperation(context.Background(), &task)
	await(t, cancelled, "the executor call to be cancelled once the lease was lost")

	if store.ran("renew") == 0 {
		t.Fatal("the lease was never renewed")
	}
	if got := store.renewedWith(); got != 30*time.Millisecond {
		t.Fatalf("renewed with %v, want the configured lease", got)
	}
	settle, ok := store.settledPublish()
	if !ok {
		t.Fatal("the publish was never settled")
	}
	// The push may have been sent before the lease was lost, so the outcome must stay reconcilable.
	want := core.PublishResult{Classification: "uncertain", ErrorCode: "executor_unreachable"}
	if settle.r != want {
		t.Fatalf("settled %+v, want %+v", settle.r, want)
	}
}
