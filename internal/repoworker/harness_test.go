package repoworker

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/internal/core"
	"github.com/wanglongan587/cloud/internal/repoexec"
)

// testCommit is a syntactically valid SHA-1 object id, distinct from the zero OID a first sync uses.
const testCommit = "1111111111111111111111111111111111111111"

// The fixtures use canonical UUIDs because the authorization the worker signs is validated against
// the same rules the executor applies, so a placeholder would settle as authorization_invalid.
const (
	syncOpID        = "11111111-1111-4111-8111-111111111111"
	publishOpID     = "22222222-2222-4222-8222-222222222222"
	tenantID        = "33333333-3333-4333-8333-333333333333"
	projectID       = "44444444-4444-4444-8444-444444444444"
	bindingID       = "55555555-5555-4555-8555-555555555555"
	otherBindingID  = "55555555-5555-4555-8555-555555555556"
	credentialID    = "66666666-6666-4666-8666-666666666666"
	authorizationID = "77777777-7777-4777-8777-777777777777"
	verificationID  = "88888888-8888-4888-8888-888888888888"
)

// await is the only waiting primitive these tests use: every synchronization point is an event the
// code under test produces, never a fixed pause.
func await(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// fakeRefs serves the Git Server ref query from a function and records every ref asked for.
type fakeRefs struct {
	mu      sync.Mutex
	resolve func(ref string) (string, bool, error)
	queried []string
}

func (f *fakeRefs) ResolveGitRef(_ context.Context, _, _, ref string) (string, bool, error) {
	f.mu.Lock()
	f.queried = append(f.queried, ref)
	f.mu.Unlock()
	if f.resolve == nil {
		return "", false, nil
	}
	return f.resolve(ref)
}

func (f *fakeRefs) ListGitRefs(context.Context, string, string, string) ([]core.GitRefCommit, error) {
	return nil, errors.New("ListGitRefs is not part of the worker's contract")
}

func (f *fakeRefs) refs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.queried...)
}

// fakeExecutor records the tasks it is handed and how many ran at once.
type fakeExecutor struct {
	mu    sync.Mutex
	tasks []repoexec.Task
	live  int
	peak  int
	run   func(ctx context.Context, task *repoexec.Task) (repoexec.Result, error)
}

func (e *fakeExecutor) Run(ctx context.Context, task *repoexec.Task) (repoexec.Result, error) {
	e.mu.Lock()
	e.tasks = append(e.tasks, *task)
	e.live++
	e.peak = max(e.peak, e.live)
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		e.live--
		e.mu.Unlock()
	}()
	if e.run == nil {
		return repoexec.Result{Classification: repoexec.Applied}, nil
	}
	return e.run(ctx, task)
}

func (e *fakeExecutor) calls() []repoexec.Task {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]repoexec.Task(nil), e.tasks...)
}

func (e *fakeExecutor) peakLive() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.peak
}

// startCall is one StartRepositoryOperation request, recorded so tests can assert what the worker
// asked core for rather than only what it did afterwards.
type startCall struct {
	id, expectedTracking string
	gen                  int64
	lease, timeout       time.Duration
}

type syncSettle struct {
	id  string
	gen int64
	r   core.SyncResult
}

type publishSettle struct {
	id  string
	gen int64
	r   core.PublishResult
}

type checkSettle struct {
	id       string
	attempt  int64
	observed string
	found    bool
	failure  string
}

type verificationSettle struct {
	id     string
	gen    int64
	result string
}

// fakeStore is the worker's whole persistence contract. Every method records its call and answers
// from the fields the test set, so each branch of the worker is reachable without PostgreSQL.
type fakeStore struct {
	mu    sync.Mutex
	calls []string

	leased     []core.RepositoryTask
	leaseErr   error
	leaseOwner string
	leaseLimit int

	startFn   func(startCall) (core.RepositoryStart, error)
	releaseFn func(id string, gen int64) error
	renewFn   func(id string, gen int64) error
	sweepErrs map[string]error

	checks []core.RepositoryCheck
	probes []core.CredentialCheck

	starts       []startCall
	releases     []string
	renewals     int
	renewTTL     time.Duration
	syncs        []syncSettle
	publishes    []publishSettle
	checkSettles []checkSettle
	probeSettles []verificationSettle
}

func (s *fakeStore) record(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, name)
}

// ran counts how many times a store method was entered.
func (s *fakeStore) ran(name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, c := range s.calls {
		if c == name {
			n++
		}
	}
	return n
}

// firstN mirrors the store's LIMIT clause: a claim never returns more rows than the caller asked
// for, which is what lets the worker start a pass without overrunning its slots.
func firstN[T any](all []T, limit int) []T {
	if limit < len(all) {
		all = all[:limit]
	}
	return append([]T(nil), all...)
}

func (s *fakeStore) LeaseRepositoryOperations(_ context.Context, owner string, limit int, _ time.Duration) ([]core.RepositoryTask, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, "lease")
	s.leaseOwner, s.leaseLimit = owner, limit
	return firstN(s.leased, limit), s.leaseErr
}

func (s *fakeStore) StartRepositoryOperation(_ context.Context, id string, gen int64, expectedTracking string, lease, timeout time.Duration) (core.RepositoryStart, error) {
	s.mu.Lock()
	call := startCall{id: id, gen: gen, expectedTracking: expectedTracking, lease: lease, timeout: timeout}
	s.starts = append(s.starts, call)
	fn := s.startFn
	s.mu.Unlock()
	if fn == nil {
		return startFor(s.taskFor(id), expectedTracking), nil
	}
	return fn(call)
}

// taskFor finds the leased task a start call refers to, so the default answer mirrors what core
// returns for a row it accepted.
func (s *fakeStore) taskFor(id string) core.RepositoryTask {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, task := range s.leased {
		if task.OperationID == id {
			return task
		}
	}
	return core.RepositoryTask{}
}

func (s *fakeStore) ReleaseRepositoryLease(_ context.Context, id string, gen int64) error {
	s.mu.Lock()
	s.calls = append(s.calls, "release")
	s.releases = append(s.releases, id)
	fn := s.releaseFn
	s.mu.Unlock()
	if fn == nil {
		return nil
	}
	return fn(id, gen)
}

func (s *fakeStore) RenewRepositoryLease(_ context.Context, id string, gen int64, lease time.Duration) error {
	s.mu.Lock()
	s.calls = append(s.calls, "renew")
	s.renewals++
	s.renewTTL = lease
	fn := s.renewFn
	s.mu.Unlock()
	if fn == nil {
		return nil
	}
	return fn(id, gen)
}

// renewedWith is the lease the last renewal asked core to write.
func (s *fakeStore) renewedWith() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.renewTTL
}

func (s *fakeStore) SettleRepositorySync(_ context.Context, id string, gen int64, r *core.SyncResult) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, "settle_sync")
	s.syncs = append(s.syncs, syncSettle{id: id, gen: gen, r: *r})
	return nil
}

func (s *fakeStore) SettleRepositoryPublish(_ context.Context, id string, gen int64, r *core.PublishResult) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, "settle_publish")
	s.publishes = append(s.publishes, publishSettle{id: id, gen: gen, r: *r})
	return nil
}

func (s *fakeStore) sweep(name string) error {
	s.record(name)
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sweepErrs[name]
}

func (s *fakeStore) SweepRepositoryLeasesOnce(context.Context) error { return s.sweep("lease sweep") }

func (s *fakeStore) ExpirePublishApprovalsOnce(context.Context) error {
	return s.sweep("approval expiry")
}

func (s *fakeStore) SettleAbandonedGitSubmissionsOnce(context.Context) error {
	return s.sweep("abandoned submissions")
}

func (s *fakeStore) RevokeGitCredentialsInStoreOnce(context.Context) error {
	return s.sweep("store revocations")
}

func (s *fakeStore) BeginRepositoryChecks(_ context.Context, limit int) ([]core.RepositoryCheck, error) {
	s.record("checks")
	s.mu.Lock()
	defer s.mu.Unlock()
	return firstN(s.checks, limit), nil
}

func (s *fakeStore) SettleRepositoryCheck(_ context.Context, id string, attempt int64, observed string, found bool, failure string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, "settle_check")
	s.checkSettles = append(s.checkSettles, checkSettle{id: id, attempt: attempt, observed: observed, found: found, failure: failure})
	return nil
}

func (s *fakeStore) StartCredentialVerifications(_ context.Context, limit int, _ time.Duration) ([]core.CredentialCheck, error) {
	s.record("verifications")
	s.mu.Lock()
	defer s.mu.Unlock()
	return firstN(s.probes, limit), nil
}

func (s *fakeStore) SettleCredentialVerification(_ context.Context, id string, gen int64, result string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, "settle_verification")
	s.probeSettles = append(s.probeSettles, verificationSettle{id: id, gen: gen, result: result})
	return nil
}

func (s *fakeStore) settledSync() (syncSettle, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.syncs) == 0 {
		return syncSettle{}, false
	}
	return s.syncs[len(s.syncs)-1], true
}

func (s *fakeStore) settledPublish() (publishSettle, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.publishes) == 0 {
		return publishSettle{}, false
	}
	return s.publishes[len(s.publishes)-1], true
}

func (s *fakeStore) settledCheck() (checkSettle, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.checkSettles) == 0 {
		return checkSettle{}, false
	}
	return s.checkSettles[len(s.checkSettles)-1], true
}

func (s *fakeStore) settledProbe() (verificationSettle, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.probeSettles) == 0 {
		return verificationSettle{}, false
	}
	return s.probeSettles[len(s.probeSettles)-1], true
}

func (s *fakeStore) lastStart() (startCall, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.starts) == 0 {
		return startCall{}, false
	}
	return s.starts[len(s.starts)-1], true
}

// newWorker builds a worker over the fakes. Slots default to one so a test drives exactly the task
// it handed over; tweak adjusts the budgets a test cares about.
func newWorker(t *testing.T, store Store, exec Executor, refs core.GitRefReader, tweak func(*Config)) *Worker {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generating the signing key: %v", err)
	}
	cfg := &Config{
		Store: store, Executor: exec, Refs: refs, Key: key, Owner: "worker-1", Slots: 1,
		Lease: time.Minute, SyncTimeout: 3 * time.Minute, PublishTimeout: 2 * time.Minute, ProbeTimeout: time.Minute, Interval: time.Millisecond,
	}
	if tweak != nil {
		tweak(cfg)
	}
	w, err := New(cfg)
	if err != nil {
		t.Fatalf("building the worker: %v", err)
	}
	return w
}

func syncTask() core.RepositoryTask {
	return core.RepositoryTask{
		OperationID: syncOpID, Type: core.RepositorySync, TenantID: tenantID, ProjectID: projectID, BindingID: bindingID,
		RepositoryURL: repositoryURL, Branch: "main", LeaseGeneration: 7,
	}
}

func publishTask() core.RepositoryTask {
	return core.RepositoryTask{
		OperationID: publishOpID, Type: core.RepositoryPublish, TenantID: tenantID, ProjectID: projectID, BindingID: bindingID,
		RepositoryURL: repositoryURL, Branch: "main", LeaseGeneration: 9,
		SourceRef: core.GitSnapshotRef(bindingID, testCommit), SourceCommit: testCommit, TargetBranch: "release", ExpectedOldOID: testCommit,
	}
}

const repositoryURL = "https://git.example.com/team/repo.git"

// startFor is the RepositoryStart core returns once an Operation moved to running.
func startFor(task core.RepositoryTask, expectedTracking string) core.RepositoryStart {
	return core.RepositoryStart{
		Task: task, AuthorizationID: authorizationID, IssuedAt: time.Now(), ExpiresAt: time.Now().Add(time.Minute),
		Deadline: time.Now().Add(time.Minute), CredentialID: credentialID, CredentialVersion: 3, ExpectedTrackingOID: expectedTracking,
	}
}
