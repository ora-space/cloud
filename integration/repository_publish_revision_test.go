package integration

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/wanglongan587/cloud/internal/cloneauth"
	"github.com/wanglongan587/cloud/internal/core"
)

// Publishing a run's Revision publishes what Cloud verified when it registered the Revision: its
// delivery ref on the Git Server and its final commit (publish ADR, revisionId; revision ADR D4).
// The Revision's revisionRef is only the Node-local refs/ora/revisions/<run> and never exists on
// the Git Server, so it must never become a publish source.
func TestPublishingARevisionUsesItsVerifiedDeliveryRef(t *testing.T) {
	f := setup(t)
	f.useObjectStore()
	scene := deliveringScene(t, f)
	execution := f.deliverRevision(t, scene)
	in := f.deliveryInputOf(execution)
	f.push(in, revisionFinalCommit)
	if _, e := f.deliveryTerminal(scene, execution, "publish-revision-saved", 1, deliveredResult(scene, in, revisionFinalCommit), "revision_delivered"); e != nil {
		t.Fatalf("a verified delivery must be taken over: %v", e)
	}
	row := f.revisionRow(scene.runID)
	if row == nil || row.DeliveryRef == "" || row.DeliveryRef == row.RevisionRef {
		t.Fatalf("the run must have registered a Git Revision with its own delivery ref: %+v", row)
	}

	// The scene lives in its own tenant, where the fixture's user is an administrator.
	f.tid = scene.tenantID
	projectID, repositoryURL := f.runPlacement(scene.runID)
	scope, err := cloneauth.NormalizeRepository(repositoryURL)
	must(t, err)
	credential := f.storedGitCredential(f.uid, "team", scope)
	must(t, firstErr(f.store.Pool.Exec(`UPDATE credential_refs SET capabilities='["publish"]' WHERE id=$1`, credential)))
	f.store.GitRefs = publishRefs{}

	path := f.path("/projects/" + projectID + "/repository/publishes")
	body := core.Object{"revisionId": row.ID, "targetBranch": "main", "expectedOldOid": revisionBaseCommitOf(t, f, row.ID), "credentialRefId": credential}
	op := f.gitCall("POST", path, body, "", 201).O("resource")
	if op.S("sourceRef") != row.DeliveryRef || op.S("sourceCommit") != revisionFinalCommit || op.S("revisionId") != row.ID {
		t.Fatalf("a Revision publish must use the verified delivery ref %s and final commit %s, got %v", row.DeliveryRef, revisionFinalCommit, op)
	}
}

// A Revision registered before the Git delivery switch keeps the bundle shape: no delivery ref, a
// base commit that differs from its final commit, and the bundle objects the old registration
// stored (0054). The Git Server never received its revision_ref, so it has no publishable source.
// Publishing one by revisionId alone must be refused as revision_not_publishable, and the refusal
// must be the whole effect: no operation, nothing queued for the executor, and not even the ref
// query — the shape is decided inside the creating transaction, before any Git Server call.
func TestPublishingABundleShapedRevisionIsRefused(t *testing.T) {
	f := setup(t)
	runID, workspaceID, nodeID := seedStartingAgentRun(t, f.store)
	var tenantID string
	must(t, f.store.Pool.QueryRow(`SELECT tenant_id FROM issue_runs WHERE id=$1`, runID).Scan(&tenantID))
	must(t, firstErr(f.store.Pool.Exec(`INSERT INTO tenant_memberships(tenant_id,user_id,role,status) VALUES($1,$2,'admin','active')`, tenantID, f.uid)))
	f.tid = tenantID
	projectID, repositoryURL := f.runPlacement(runID)
	scope, err := cloneauth.NormalizeRepository(repositoryURL)
	must(t, err)
	credential := f.storedGitCredential(f.uid, "team", scope)
	must(t, firstErr(f.store.Pool.Exec(`UPDATE credential_refs SET capabilities='["publish"]' WHERE id=$1`, credential)))
	revisionID := seedBundleRevision(t, f, runID, workspaceID, nodeID)

	f.store.GitRefs = unreachedPublishRefs{t: t}
	refused := f.gitCall("POST", f.path("/projects/"+projectID+"/repository/publishes"), core.Object{"revisionId": revisionID}, "", 409)
	if refused.S("code") != "revision_not_publishable" {
		t.Fatalf("bundle Revision publish code = %q; want revision_not_publishable", refused.S("code"))
	}
	if n := f.scalar(`SELECT count(*) FROM repository_operations WHERE project_id=$1`, projectID); n != 0 {
		t.Fatalf("a refused bundle Revision publish created %d repository operations; want none", n)
	}
}

// unreachedPublishRefs fails the case if a publish expected to be refused reaches the Git Server:
// every call below would mean the Revision was treated as a source after all.
type unreachedPublishRefs struct{ t *testing.T }

func (r unreachedPublishRefs) ResolveGitRef(context.Context, string, string, string) (string, bool, error) {
	r.t.Error("the Git Server ref query ran for a Revision with no delivery ref")
	return "", false, nil
}

func (r unreachedPublishRefs) ListGitRefs(context.Context, string, string, string) ([]core.GitRefCommit, error) {
	r.t.Error("the Git Server ref list ran for a Revision with no delivery ref")
	return nil, nil
}

// seedBundleRevision stages a Revision in the pre-Git bundle shape through the same schema the old
// registration wrote. The Revision is chained to Cloud's delivery verdict, which is chained to the
// Node execution that produced it, so a believable row is three rows. No work item is needed: only
// the Revision the earlier delivery left behind is read by the publish path.
func seedBundleRevision(t *testing.T, f *fixture, runID, workspaceID, nodeID string) string {
	t.Helper()
	projectID, repositoryURL := f.runPlacement(runID)
	var tenantID string
	must(t, f.store.Pool.QueryRow(`SELECT tenant_id FROM issue_runs WHERE id=$1`, runID).Scan(&tenantID))
	execution, id := "exec-bundle-"+uuid.NewString(), uuid.NewString()
	must(t, firstErr(f.store.Pool.Exec(`
		INSERT INTO node_executions(execution_id, kind, operation_id, workspace_id, node_id,
		                            node_operation_id, input, result, dispatched_epoch)
		VALUES ($1,'deliver_revision',$2,$3,$4,$1,'{}','{"outcome":"revision_delivered"}',1)`,
		execution, runID, workspaceID, nodeID)))
	must(t, firstErr(f.store.Pool.Exec(`
		INSERT INTO revision_verifications(execution_id, outcome) VALUES ($1,'delivered')`, execution)))
	must(t, firstErr(f.store.Pool.Exec(`
		INSERT INTO revisions(id, execution_id, tenant_id, run_id, workspace_id, project_id, repository_url,
		                      base_commit, final_commit, revision_ref,
		                      bundle_key, bundle_size, bundle_sha256,
		                      history_key, history_size, history_sha256)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,1024,$12,$13,512,$14)`,
		id, execution, tenantID, runID, workspaceID, projectID, repositoryURL,
		strings.Repeat("2", 40), strings.Repeat("3", 40), "refs/ora/revisions/"+runID,
		"revisions/"+tenantID+"/"+runID+"/"+execution+"/bundle.bundle", strings.Repeat("d", 64),
		"revisions/"+tenantID+"/"+runID+"/"+execution+"/history.jsonl", strings.Repeat("c", 64))))
	return id
}

// revisionBaseCommitOf is the commit the Revision started from, a target branch value that differs
// from its final commit so the publish has something to fast-forward.
func revisionBaseCommitOf(t *testing.T, f *fixture, revisionID string) string {
	t.Helper()
	var base string
	must(t, f.store.Pool.QueryRow(`SELECT base_commit FROM revisions WHERE id=$1`, revisionID).Scan(&base))
	return base
}
