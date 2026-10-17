package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/wanglongan587/cloud/internal/core"
)

// publishRefs stands in for the ref query only; executor/Git boundary tests use real repositories.
type publishRefs struct{}

func (publishRefs) ResolveGitRef(context.Context, string, string, string) (string, bool, error) {
	return strings.Repeat("a", 40), true, nil
}

func (publishRefs) ListGitRefs(context.Context, string, string, string) ([]core.GitRefCommit, error) {
	return nil, nil
}

func (f *fixture) publishBody(credential string) core.Object {
	return core.Object{"sourceRef": core.GitDeliveryRefPrefix + uuid.NewString(), "sourceCommit": strings.Repeat("a", 40), "targetBranch": "main", "expectedOldOid": strings.Repeat("b", 40), "credentialRefId": credential}
}

func (f *fixture) publishCredential(owner, kind string) string {
	f.t.Helper()
	id := f.storedGitCredential(owner, kind, "https://example.invalid/repo")
	must(f.t, firstErr(f.store.Pool.Exec(`UPDATE credential_refs SET capabilities='["publish"]' WHERE id=$1`, id)))
	f.store.GitRefs = publishRefs{}
	return id
}

func (f *fixture) startPublish(pid, id string) core.RepositoryTask {
	f.t.Helper()
	tasks, err := f.store.LeaseRepositoryOperations(context.Background(), "test", 10, time.Minute)
	must(f.t, err)
	for _, task := range tasks {
		if task.OperationID != id {
			continue
		}
		start, err := f.store.StartRepositoryOperation(context.Background(), id, task.LeaseGeneration, "", time.Minute, time.Minute)
		must(f.t, err)
		if start.Refused != "" {
			f.t.Fatalf("publish refused: %s", start.Refused)
		}
		return task
	}
	f.t.Fatalf("publish %s in project %s was not leased", id, pid)
	return core.RepositoryTask{}
}

func TestRepositoryPublishPersonalApprovalMatrixAndIdempotency(t *testing.T) {
	f := setup(t)
	pid := f.create("publish-approval").O("resource").S("id")
	f.drain()
	bob, bobID := f.addUser(t, "bob-publisher", "Bob")
	credential := f.publishCredential(bobID, "personal")
	path := f.path("/projects/" + pid + "/repository/publishes")
	body := f.publishBody(credential)
	op := f.gitCall("POST", path, body, "same-publish", 201).O("resource")
	if op.S("state") != "awaiting_approval" {
		t.Fatalf("administrator used personal credential: %v", op)
	}
	same := f.gitCall("POST", path, body, "same-publish", 201).O("resource")
	if same.S("id") != op.S("id") {
		t.Fatal("idempotency did not reuse publish")
	}
	body["targetBranch"] = "other"
	f.gitCall("POST", path, body, "same-publish", 409)
	action := f.path("/projects/" + pid + "/repository/operations/" + op.S("id") + "/approve")
	f.gitCall("POST", action, nil, "", 403)
	f.user = bob
	approved := f.gitCall("POST", action, nil, "", 200).O("resource")
	if approved.S("state") != "pending" || approved.S("approvedBy") != bobID {
		t.Fatalf("owner approval = %v", approved)
	}
	// A personal owner may initiate with their own credential despite being a normal member.
	own := f.gitCall("POST", path, f.publishBody(credential), "", 201).O("resource")
	if own.S("state") != "pending" {
		t.Fatalf("owner did not self-approve: %v", own)
	}
	_, otherID := f.addUser(t, "other-publisher", "Other")
	must(t, firstErr(f.store.Pool.Exec("UPDATE credential_refs SET owner_user_id=$2 WHERE id=$1", credential, otherID)))
	f.gitCall("POST", path, f.publishBody(credential), "", 403)
	// An unverified credential can never create a publish.
	unverified := f.storedGitCredential(bobID, "personal", "https://example.invalid/repo")
	f.gitCall("POST", path, f.publishBody(unverified), "", 409)
}

func TestRepositoryPublishLeaseMutualExclusionAndRecovery(t *testing.T) {
	f := setup(t)
	pid := f.create("publish-recovery").O("resource").S("id")
	f.drain()
	credential := f.publishCredential(f.uid, "team")
	path := f.path("/projects/" + pid + "/repository/publishes")
	first := f.gitCall("POST", path, f.publishBody(credential), "", 201).O("resource")
	second := f.gitCall("POST", path, f.publishBody(credential), "", 201).O("resource")
	task := f.startPublish(pid, first.S("id"))
	if f.scalar("SELECT count(*) FROM repository_operations WHERE id=$1 AND state='pending'", second.S("id")) != 1 {
		t.Fatal("same-branch publish escaped exclusion")
	}
	must(t, firstErr(f.store.Pool.Exec("UPDATE repository_operations SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1", task.OperationID)))
	must(t, f.store.SweepRepositoryLeasesOnce(context.Background()))
	if state, _ := f.repositoryOperationState(task.OperationID); state != "uncertain" {
		t.Fatalf("lost publish = %s", state)
	}
	if _, err := f.store.AuthorizeGitOperation(context.Background(), task.OperationID, f.tid, pid); err == nil {
		t.Fatal("uncertain publish retained Git access")
	}
	// uncertain no longer owns the branch slot; another explicit publish can start.
	f.startPublish(pid, second.S("id"))
	checks, err := f.store.BeginRepositoryChecks(context.Background(), 1)
	must(t, err)
	if len(checks) != 0 {
		t.Fatal("checked before task deadline")
	}
	must(t, firstErr(f.store.Pool.Exec("UPDATE repository_operations SET next_check_at=clock_timestamp()-interval '1 second' WHERE id=$1", task.OperationID)))
	checks, err = f.store.BeginRepositoryChecks(context.Background(), 1)
	must(t, err)
	if len(checks) != 1 {
		t.Fatalf("checks = %v", checks)
	}
	must(t, f.store.SettleRepositoryCheck(context.Background(), task.OperationID, checks[0].Attempt, strings.Repeat("a", 40), true, ""))
	if state, _ := f.repositoryOperationState(task.OperationID); state != "succeeded" {
		t.Fatalf("verified publish = %s", state)
	}
	if f.scalar("SELECT count(*) FROM repository_operations WHERE id=$1 AND state='leased'", f.initialSync(pid)) != 1 {
		t.Fatal("publish did not reuse not-yet-running sync")
	}
}

func (f *fixture) initialSync(pid string) string {
	f.t.Helper()
	var id string
	must(f.t, f.store.Pool.QueryRow("SELECT id FROM repository_operations WHERE project_id=$1 AND type='repository_sync' ORDER BY created_at LIMIT 1", pid).Scan(&id))
	return id
}

func TestRepositoryPublishSuccessSchedulesExactlyOneSyncAfterRunningSync(t *testing.T) {
	f := setup(t)
	pid, st := f.leasedSync()
	f.drain()
	_, err := f.store.StartRepositoryOperation(context.Background(), st.OperationID, st.LeaseGeneration, strings.Repeat("0", 40), time.Minute, time.Minute)
	must(t, err)
	credential := f.publishCredential(f.uid, "team")
	path := f.path("/projects/" + pid + "/repository/publishes")
	for range 2 {
		op := f.gitCall("POST", path, f.publishBody(credential), "", 201).O("resource")
		task := f.startPublish(pid, op.S("id"))
		must(t, f.store.SettleRepositoryPublish(context.Background(), task.OperationID, task.LeaseGeneration, core.PublishResult{Classification: "applied"}))
	}
	if !f.bindingOf(pid).B("resyncRequested") {
		t.Fatal("running sync did not request a successor")
	}
	must(t, f.store.SettleRepositorySync(context.Background(), st.OperationID, st.LeaseGeneration, core.SyncResult{State: "succeeded", CommitOID: strings.Repeat("c", 40)}))
	if f.scalar("SELECT count(*) FROM repository_operations WHERE project_id=$1 AND type='repository_sync' AND state='pending' AND trigger='after_publish'", pid) != 1 || f.bindingOf(pid).B("resyncRequested") {
		t.Fatal("multiple publishes did not coalesce into one successor")
	}
}

func TestRepositoryPublishApprovalExpiresAndRotationCancelsOldVersion(t *testing.T) {
	f := setup(t)
	pid := f.create("publish-expire").O("resource").S("id")
	f.drain()
	c := f.confirmCredential(f.gitCredential())
	f.store.GitRefs = publishRefs{}
	must(t, firstErr(f.store.Pool.Exec(`UPDATE credential_refs SET capabilities='["publish"]' WHERE id=$1`, c.S("id"))))
	path := f.path("/projects/" + pid + "/repository/publishes")
	expired := f.gitCall("POST", path, f.publishBody(c.S("id")), "", 201).O("resource")
	must(t, firstErr(f.store.Pool.Exec("UPDATE repository_operations SET approval_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1", expired.S("id"))))
	must(t, f.store.ExpirePublishApprovalsOnce(context.Background()))
	if state, code := f.repositoryOperationState(expired.S("id")); state != "cancelled" || code != "approval_expired" {
		t.Fatalf("expiry = %s/%s", state, code)
	}
	pending := f.gitCall("POST", path, f.publishBody(c.S("id")), "", 201).O("resource")
	c = f.gitCall("GET", f.path("/git-credentials/"+c.S("id")), nil, "", 200)
	rotation := f.gitCall("POST", f.path("/git-credentials/"+c.S("id")+"/rotate"), core.Object{"version": c.N("version")}, "", 200)
	f.confirmCredential(rotation)
	if state, code := f.repositoryOperationState(pending.S("id")); state != "cancelled" || code != "credential_changed" {
		t.Fatalf("rotation = %s/%s", state, code)
	}
}
