package integration

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/wanglongan587/cloud/internal/core"
)

// credentialMetadata is the Store management boundary; it deliberately has no token field.
type credentialMetadata struct {
	stored core.StoredCredential
	revoke func(string) error
}

func (m *credentialMetadata) CredentialVersion(context.Context, string) (core.StoredCredential, error) {
	return m.stored, nil
}

func (m *credentialMetadata) RevokeCredential(_ context.Context, id string) error {
	return m.revoke(id)
}

func (f *fixture) gitCredential() core.Object {
	f.t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	must(f.t, err)
	f.store.GitSubmissions = &core.GitCredentialSubmissions{Key: key, IngressURL: "https://credentials.example.invalid"}
	return f.gitCall("POST", f.path("/git-credentials"), core.Object{"name": "External", "attribution": "personal", "repositoryScope": "https://EXAMPLE.invalid:443/repo.git/", "requestedCapabilities": []any{"read"}}, "", 201)
}

func (f *fixture) confirmCredential(created core.Object) core.Object {
	f.t.Helper()
	c, sub := created.O("resource"), created.O("submission")
	f.store.GitCredentialStore = &credentialMetadata{stored: core.StoredCredential{CurrentVersion: sub.N("storeVersion"), SubmissionID: sub.S("submissionId")}}
	return f.gitCall("POST", f.path("/git-credentials/"+c.S("id")+"/confirm"), core.Object{"submissionId": sub.S("submissionId")}, "", 200).O("resource")
}

func TestGitCredentialSubmissionRotationConfirmationAndRevocation(t *testing.T) {
	f := setup(t)
	created := f.gitCredential()
	c, sub := created.O("resource"), created.O("submission")
	if c.S("secretState") != "awaiting_secret" || c.S("repositoryScope") != "https://example.invalid/repo" {
		t.Fatalf("credential = %v", c)
	}
	// Browser claims are insufficient: both the Store version and submission must match.
	for _, wrong := range []core.StoredCredential{{CurrentVersion: 2, SubmissionID: sub.S("submissionId")}, {CurrentVersion: 1, SubmissionID: uuid.NewString()}, {CurrentVersion: 1, SubmissionID: sub.S("submissionId"), Revoked: true}} {
		f.store.GitCredentialStore = &credentialMetadata{stored: wrong}
		got := f.gitCall("POST", f.path("/git-credentials/"+c.S("id")+"/confirm"), core.Object{"submissionId": sub.S("submissionId")}, "", 409)
		if got.S("code") != "submission_not_stored" {
			t.Fatalf("mismatch = %v", got)
		}
	}
	c = f.confirmCredential(created)
	rotation := f.gitCall("POST", f.path("/git-credentials/"+c.S("id")+"/rotate"), core.Object{"version": c.N("version")}, "", 200)
	if rotation.O("resource").S("secretState") != "stored" || rotation.O("resource").N("storeVersion") != 1 {
		t.Fatalf("rotation replaced stored version: %v", rotation)
	}
	repeated := f.gitCall("POST", f.path("/git-credentials/"+c.S("id")+"/rotate"), core.Object{"version": rotation.O("resource").N("version")}, "", 200)
	if repeated.O("submission").S("grant") != rotation.O("submission").S("grant") {
		t.Fatal("rotation issued two grants for one version")
	}
	c = f.confirmCredential(rotation)
	if c.N("storeVersion") != 2 || len(c["capabilities"].([]any)) != 0 {
		t.Fatalf("new version reused evidence: %v", c)
	}
	f.store.GitCredentialStore = &credentialMetadata{revoke: func(id string) error {
		var state string
		must(t, f.store.Pool.QueryRow("SELECT secret_state FROM credential_refs WHERE id=$1", id).Scan(&state))
		if state != "revoked" {
			t.Fatalf("Store called before Cloud revoked: %s", state)
		}
		return errors.New("temporary store failure")
	}}
	f.gitCall("DELETE", f.path("/git-credentials/"+c.S("id")), core.Object{"version": c.N("version")}, "", 200)
	if err := f.store.RevokeGitCredentialsInStoreOnce(context.Background()); err == nil {
		t.Fatal("revocation failure disappeared")
	}
	if f.scalar("SELECT count(*) FROM credential_refs WHERE id=$1 AND store_revocation_pending", c.S("id")) != 1 {
		t.Fatal("failed revocation was not retained")
	}
	f.store.GitCredentialStore = &credentialMetadata{revoke: func(string) error { return nil }}
	must(t, f.store.RevokeGitCredentialsInStoreOnce(context.Background()))
	if f.scalar("SELECT count(*) FROM credential_refs WHERE store_revocation_pending") != 0 {
		t.Fatal("successful revocation remains pending")
	}
	f.gitCall("POST", f.path("/git-credentials/"+c.S("id")+"/rotate"), core.Object{"version": c.N("version") + 1}, "", 409)
	// Cloud's strict decoder must never accept a raw token.
	f.gitCall("POST", f.path("/git-credentials"), core.Object{"token": "not-a-real-secret"}, "", 400)
}

func TestGitCredentialAbandonedRotationKeepsStoredVersion(t *testing.T) {
	f := setup(t)
	c := f.confirmCredential(f.gitCredential())
	rotation := f.gitCall("POST", f.path("/git-credentials/"+c.S("id")+"/rotate"), core.Object{"version": c.N("version")}, "", 200)
	must(t, firstErr(f.store.Pool.Exec("UPDATE credential_refs SET pending_submission_expires_at=clock_timestamp()-interval '3 minutes' WHERE id=$1", c.S("id"))))
	// Store still holds v1; clearing the abandoned v2 must not revoke v1.
	f.store.GitCredentialStore = &credentialMetadata{stored: core.StoredCredential{CurrentVersion: 1, SubmissionID: uuid.NewString()}}
	must(t, f.store.SettleAbandonedGitSubmissionsOnce(context.Background()))
	c = f.gitCall("GET", f.path("/git-credentials/"+c.S("id")), nil, "", 200)
	if c.S("secretState") != "stored" || c.N("storeVersion") != 1 || c["pendingSubmission"] != nil {
		t.Fatalf("abandoned rotation = %v (submission %s)", c, rotation.O("submission").S("submissionId"))
	}
}

func (f *fixture) leasedSync() (string, core.RepositoryTask) {
	f.t.Helper()
	pid := f.create(uuid.NewString()).O("resource").S("id")
	tasks, err := f.store.LeaseRepositoryOperations(context.Background(), "test", 1, time.Minute)
	must(f.t, err)
	if len(tasks) != 1 {
		f.t.Fatalf("leased = %v", tasks)
	}
	return pid, tasks[0]
}

func TestRepositorySyncLeaseFencingReplayAndResyncAreAtomic(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	pid, task := f.leasedSync()
	// Pending/leased can be reclaimed, but stale generations must never start.
	must(t, f.store.ReleaseRepositoryLease(ctx, task.OperationID, task.LeaseGeneration))
	tasks, err := f.store.LeaseRepositoryOperations(ctx, "second", 1, time.Minute)
	must(t, err)
	_, err = f.store.StartRepositoryOperation(ctx, task.OperationID, task.LeaseGeneration, strings.Repeat("0", 40), time.Minute, time.Minute)
	if !core.IsLeaseLost(err) {
		t.Fatalf("stale start = %v", err)
	}
	task = tasks[0]
	start, err := f.store.StartRepositoryOperation(ctx, task.OperationID, task.LeaseGeneration, strings.Repeat("0", 40), time.Minute, time.Minute)
	must(t, err)
	if start.AuthorizationID == "" {
		t.Fatal("running has no authorization")
	}
	_, err = f.store.StartRepositoryOperation(ctx, task.OperationID, task.LeaseGeneration, strings.Repeat("0", 40), time.Minute, time.Minute)
	if !core.IsLeaseLost(err) {
		t.Fatalf("second start = %v", err)
	}
	must(t, firstErr(f.store.Pool.Exec("UPDATE repository_bindings SET resync_requested=true WHERE project_id=$1", pid)))
	must(t, firstErr(f.store.Pool.Exec("UPDATE repository_operations SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1", task.OperationID)))
	must(t, f.store.SweepRepositoryLeasesOnce(ctx))
	if state, code := f.repositoryOperationState(task.OperationID); state != "failed" || code != "lease_lost" {
		t.Fatalf("lost lease = %s/%s", state, code)
	}
	if f.scalar("SELECT count(*) FROM repository_operations WHERE binding_id=$1 AND state='pending' AND source_operation_id=$2 AND replay_count=1", task.BindingID, task.OperationID) != 1 || f.bindingOf(pid).B("resyncRequested") {
		t.Fatal("replay did not absorb resync in settling transaction")
	}
	err = f.store.SettleRepositorySync(ctx, task.OperationID, task.LeaseGeneration, core.SyncResult{State: "succeeded", CommitOID: strings.Repeat("a", 40)})
	if !core.IsLeaseLost(err) {
		t.Fatalf("late result = %v", err)
	}
	// Exhaust the chain: no original ID or authorization can be reused.
	for replay := 1; replay <= 3; replay++ {
		tasks, err = f.store.LeaseRepositoryOperations(ctx, "test", 1, time.Minute)
		must(t, err)
		if len(tasks) != 1 {
			t.Fatalf("replay %d = %v", replay, tasks)
		}
		task = tasks[0]
		_, err = f.store.StartRepositoryOperation(ctx, task.OperationID, task.LeaseGeneration, strings.Repeat("0", 40), time.Minute, time.Minute)
		must(t, err)
		must(t, f.store.SettleRepositorySync(ctx, task.OperationID, task.LeaseGeneration, core.SyncResult{State: "failed", ErrorCode: "executor_unreachable"}))
	}
	if f.scalar("SELECT count(*) FROM repository_operations WHERE state IN ('pending','leased','running')") != 0 {
		t.Fatal("replay limit did not end chain")
	}
	if f.scalar("SELECT count(DISTINCT authorization_id) FROM repository_operations") != 4 {
		t.Fatal("replays reused authorization")
	}
}

func TestRepositorySyncConcurrentLeasesAndIdempotentTrigger(t *testing.T) {
	f := setup(t)
	pid := f.create("concurrent-sync").O("resource").S("id")
	f.drain()
	var wg sync.WaitGroup
	results := make(chan []core.RepositoryTask, 2)
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tasks, err := f.store.LeaseRepositoryOperations(context.Background(), uuid.NewString(), 1, time.Minute)
			results <- tasks
			errs <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		must(t, err)
	}
	count := 0
	for tasks := range results {
		count += len(tasks)
	}
	if count != 1 {
		t.Fatalf("concurrent workers leased %d times", count)
	}
	op := f.gitCall("POST", f.path("/projects/"+pid+"/repository/syncs"), nil, "", 200).O("resource")
	if op.S("state") != "leased" || f.scalar("SELECT count(*) FROM repository_operations") != 1 {
		t.Fatalf("manual trigger duplicated sync: %v", op)
	}
}

func TestRepositoryPublishUncertainFinalClaimCrashStillTerminates(t *testing.T) {
	f := setup(t)
	pid := f.create("uncertain-final").O("resource").S("id")
	bid := f.bindingOf(pid).S("id")
	id, err := f.repositoryOperation(pid, bid, core.RepositoryPublish, "uncertain")
	must(t, err)
	must(t, firstErr(f.store.Pool.Exec("UPDATE repository_operations SET check_attempts=20,next_check_at=clock_timestamp()-interval '1 second' WHERE id=$1", id)))
	checks, err := f.store.BeginRepositoryChecks(context.Background(), 1)
	must(t, err)
	if len(checks) != 0 {
		t.Fatalf("exhausted publish dispatched again: %v", checks)
	}
	if state, code := f.repositoryOperationState(id); state != "failed" || code != "outcome_unknown" {
		t.Fatalf("final claim crash = %s/%s", state, code)
	}
}

func (f *fixture) gitCall(method, path string, body core.Object, key string, want int) core.Object {
	f.t.Helper()
	if method != "GET" {
		if key == "" {
			key = uuid.NewString()
		}
		if body == nil {
			body = core.Object{}
		}
	}
	return f.call(method, path, body, key, want)
}
