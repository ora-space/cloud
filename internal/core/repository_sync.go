package core

// Repository sync (cloud/project, Binding and snapshot sync). Syncs are created by the foundation
// with the Project, by a member, by a candidate credential and after a publish; the worker runs
// them through the Repo Executor, and settling one is the only way the Binding's current snapshot
// moves. Settling a sync and creating its successor happen in one transaction, so the Binding's
// latest sync is always either unfinished or a terminal state that will not be replayed (D9).

// Sync error codes Cloud records. Codes reported by the executor are kept when they are on this
// list; anything else becomes sync_failed, so no unvetted text reaches the public contract.
var syncErrorCodes = map[string]bool{
	"repository_requires_credential": true, "credential_rejected": true, "upstream_branch_missing": true,
	"repository_too_large": true, "timeout": true, "lease_lost": true, "executor_unreachable": true,
	"executor_busy": true, "git_server_rejected": true, "git_server_unavailable": true, "verification_mismatch": true,
	"credential_unavailable": true, "credential_not_in_store": true, "repository_credential_frozen": true,
	"repository_credential_invalid": true, "repository_credential_scope_mismatch": true, "repository_credential_unavailable": true,
	"redemption_failed": true, "egress_denied": true, "fetch_failed": true, "repository_url_invalid": true,
	"authorization_invalid": true, "authorization_expired": true, "authorization_mismatch": true,
}

// SyncErrorCode maps an executor-reported failure to the code Cloud records.
func SyncErrorCode(code string) string {
	if code == "too_large" {
		return "repository_too_large"
	}
	if syncErrorCodes[code] {
		return code
	}
	return "sync_failed"
}

// repositoryPublic serves the Project's repository routes that only touch PostgreSQL: the
// Binding view, manual and candidate-credential syncs, the Operation list and the publish
// approval actions. Publishing itself needs a ref query first and is served by CreatePublish.
func repositoryPublic(t *transaction, r *PublicRequest, uid string) (Object, int) {
	p, _ := projectInSpace(t, r.TenantID, uid, r.ProjectID)
	if r.Method == "GET" {
		switch {
		case r.RepositoryOperationID != "":
			return repositoryOperationView(projectRepositoryOperation(t, p, r.RepositoryOperationID)), 200
		case hasPathSuffix(r.Path, "/repository/operations"):
			return pageRepositoryOperations(t, p, r), 200
		default:
			return repositoryView(t, p), 200
		}
	}
	switch {
	case hasPathSuffix(r.Path, "/repository/syncs"):
		return triggerManualSync(t, p, uid)
	case hasPathSuffix(r.Path, "/repository/credential"):
		return submitCandidateCredential(t, r, p, uid), 202
	case r.RepositoryOperationID != "":
		return publishAction(t, r, p, uid), 200
	}
	reject(404, "not_found")
	return nil, 0
}

func hasPathSuffix(path, suffix string) bool {
	return len(path) >= len(suffix) && path[len(path)-len(suffix):] == suffix
}

// repositoryView is the "code source" document: the Binding, its credential's safe metadata, the
// current snapshot and the latest sync (sync ADR D8).
func repositoryView(t *transaction, p Object) Object {
	b := bindingOf(t, p.S("id"))
	out := Object{
		"binding":  Object{"id": b["id"], "projectId": b["projectId"], "repositoryUrl": b["repositoryUrl"], "branch": b["branch"], "credentialRefId": b["credentialRefId"], "resyncRequested": b["resyncRequested"], "version": b["version"]},
		"snapshot": nil, "latestSync": nil, "credential": nil,
	}
	if s := t.one("SELECT s.*,o.result->'forced' AS forced FROM repository_snapshots s JOIN repository_operations o ON o.id=s.sync_operation_id WHERE s.binding_id=$1", b.S("id")); s != nil {
		out["snapshot"] = Object{"commitOid": s["commitOid"], "snapshotRef": s["snapshotRef"], "syncOperationId": s["syncOperationId"], "recordedAt": s["recordedAt"], "forced": s.B("forced")}
	}
	if o := t.one("SELECT * FROM repository_operations WHERE binding_id=$1 AND type='repository_sync' ORDER BY created_at DESC, id DESC LIMIT 1", b.S("id")); o != nil {
		out["latestSync"] = repositoryOperationView(o)
	}
	if b.S("credentialRefId") != "" {
		if c := t.one(gitCredentialSelect+" WHERE c.id=$1", b.S("credentialRefId")); c != nil {
			out["credential"] = gitCredentialView(c)
		}
	}
	return out
}

func projectRepositoryOperation(t *transaction, p Object, id string) Object {
	require(validID(id), 404, "not_found")
	o := t.one("SELECT * FROM repository_operations WHERE id=$1 AND project_id=$2", id, p.S("id"))
	require(o != nil, 404, "not_found")
	return o
}

// pageRepositoryOperations lists the Project's syncs and publishes newest first.
func pageRepositoryOperations(t *transaction, p Object, r *PublicRequest) Object {
	q := "SELECT * FROM repository_operations o WHERE o.project_id=$1"
	args := []any{p.S("id")}
	if r.After != "" {
		require(validID(r.After), 400, "invalid_cursor")
		args = append(args, r.After)
		q += " AND (o.created_at,o.id) < ((SELECT a.created_at FROM repository_operations a WHERE a.id=$2::uuid),$2::uuid)"
	}
	out := window(t, q+" ORDER BY o.created_at DESC, o.id DESC", args, r)
	items := []Object{}
	for _, o := range out["items"].([]Object) {
		items = append(items, repositoryOperationView(o))
	}
	out["items"] = items
	return out
}

// triggerManualSync starts a sync a member asked for. An unfinished sync answers the request:
// two triggers never race into two Operations (sync ADR D2).
func triggerManualSync(t *transaction, p Object, uid string) (Object, int) {
	require(p.S("lifecycle") == "active", 409, "resource_unavailable")
	b := bindingOf(t, p.S("id"))
	if o := unfinishedSync(t, b.S("id")); o != nil {
		return Object{"resource": repositoryOperationView(o)}, 200
	}
	fields := Object{"requested_by": uid}
	if c := b.S("credentialRefId"); c != "" {
		code := credentialProblem(t, b.S("tenantId"), c, b.S("repositoryUrl"), "")
		require(code == "", 409, code)
		fields["credential_ref_id"], fields["credential_version"] = c, t.one("SELECT store_version FROM credential_refs WHERE id=$1", c).N("storeVersion")
	}
	return Object{"resource": repositoryOperationView(insertSync(t, b, "manual", fields))}, 202
}

// submitCandidateCredential proposes another credential for the Binding. It is used for a sync
// first and adopted only if that sync succeeds while the Binding is unchanged (sync ADR D5).
func submitCandidateCredential(t *transaction, r *PublicRequest, p Object, uid string) Object {
	require(p.S("lifecycle") == "active", 409, "resource_unavailable")
	b := bindingOf(t, p.S("id"))
	version(b, r.Body.N("version"))
	c := loadGitCredential(t, r.TenantID, r.Body.S("credentialRefId"))
	requireCredentialManager(t, c, uid)
	require(unfinishedSync(t, b.S("id")) == nil, 409, "sync_in_progress")
	code := credentialProblem(t, b.S("tenantId"), c.S("id"), b.S("repositoryUrl"), "")
	require(code == "", 409, code)
	o := insertSync(t, b, "credential_candidate", Object{"requested_by": uid, "credential_ref_id": c.S("id"), "credential_version": c.N("storeVersion"), "binding_version": b.N("version")})
	return Object{"resource": repositoryOperationView(o)}
}

// settleSync records a sync's terminal state and everything that must happen in the same
// transaction: the current snapshot on success, the read capability it proved, a candidate
// credential's adoption, a token the external side rejected, and at most one successor.
func settleSync(t *transaction, o Object, state, code string, result Object) {
	finishRepositoryOperation(t, o.S("id"), state, code, result)
	b := t.one("SELECT * FROM repository_bindings WHERE id=$1", o.S("bindingId"))
	if state == "succeeded" {
		commit := result.S("commitOid")
		t.exec(`INSERT INTO repository_snapshots(binding_id,tenant_id,project_id,commit_oid,snapshot_ref,sync_operation_id) VALUES($1,$2,$3,$4,$5,$6)
		 ON CONFLICT(binding_id) DO UPDATE SET commit_oid=EXCLUDED.commit_oid,snapshot_ref=EXCLUDED.snapshot_ref,sync_operation_id=EXCLUDED.sync_operation_id,recorded_at=clock_timestamp()`,
			b.S("id"), b.S("tenantId"), b.S("projectId"), commit, GitSnapshotRef(b.S("id"), commit), o.S("id"))
		if c := o.S("credentialRefId"); c != "" {
			recordCapability(t, c, o.N("credentialVersion"), CapabilityRead, o.S("id"))
			if o.S("trigger") == "credential_candidate" && b.N("version") == o.N("bindingVersion") {
				t.exec("UPDATE repository_bindings SET credential_ref_id=$2,version=version+1 WHERE id=$1", b.S("id"), c)
			}
		}
	}
	if code == "credential_rejected" && o.S("credentialRefId") != "" {
		markCredentialInvalid(t, o.S("credentialRefId"), o.N("credentialVersion"))
	}
	createSyncSuccessor(t, o, state, code, b)
}

// createSyncSuccessor creates at most one sync after o ends: an automatic replay when o was lost
// to its worker or executor (at most three in a row) or superseded (once), otherwise the resync a
// publish asked for. A replay absorbs the resync request, since it fetches after the publish too.
// A system-triggered sync that cannot be created only records why; the settling transaction never
// rolls back for it (contract D5 item 3).
func createSyncSuccessor(t *transaction, o Object, state, code string, b Object) {
	p := t.one("SELECT lifecycle,deleted_at FROM projects WHERE id=$1", b.S("projectId"))
	live := p != nil && p["deletedAt"] == nil && (p.S("lifecycle") == "active" || p.S("lifecycle") == "provisioning")
	replay := state == "failed" && (code == "lease_lost" || code == "executor_unreachable") && o.N("replayCount") < maxAutomaticReplays ||
		state == "superseded" && o.N("replayCount") == 0
	resync := b.B("resyncRequested")
	if resync {
		t.exec("UPDATE repository_bindings SET resync_requested=false WHERE id=$1", b.S("id"))
	}
	if !live || !replay && (!resync || p.S("lifecycle") != "active") {
		if replay || resync {
			t.exec("UPDATE repository_operations SET result=result||'{\"successorSkipped\":\"project_not_active\"}'::jsonb WHERE id=$1", o.S("id"))
		}
		return
	}
	fields := Object{"source_operation_id": o.S("id")}
	trigger := "after_publish"
	if replay {
		trigger = o.S("trigger")
		fields["replay_count"] = o.N("replayCount") + 1
		fields["requested_by"] = o["requestedBy"]
		if o.S("trigger") == "credential_candidate" {
			fields["credential_ref_id"], fields["credential_version"], fields["binding_version"] = o["credentialRefId"], o["credentialVersion"], o["bindingVersion"]
		}
	}
	if c := b.S("credentialRefId"); c != "" && fields["credential_ref_id"] == nil {
		if problem := credentialProblem(t, b.S("tenantId"), c, b.S("repositoryUrl"), ""); problem != "" {
			t.exec("UPDATE repository_operations SET result=result||jsonb_build_object('successorSkipped',$2::text) WHERE id=$1", o.S("id"), problem)
			return
		}
		fields["credential_ref_id"], fields["credential_version"] = c, t.one("SELECT store_version FROM credential_refs WHERE id=$1", c).N("storeVersion")
	}
	insertSync(t, b, trigger, fields)
}

// requestSyncAfterPublish makes the Binding's current snapshot catch up with a publish that
// changed its branch (publish ADR D6): reuse a sync that has not fetched yet, ask a running one
// for a successor, or create one.
func requestSyncAfterPublish(t *transaction, publish Object) {
	b := t.one("SELECT * FROM repository_bindings WHERE id=$1", publish.S("bindingId"))
	if b == nil || b.S("branch") != publish.S("targetBranch") {
		return
	}
	if o := unfinishedSync(t, b.S("id")); o != nil {
		if o.S("state") == "running" {
			t.exec("UPDATE repository_bindings SET resync_requested=true WHERE id=$1", b.S("id"))
		}
		return
	}
	p := t.one("SELECT lifecycle FROM projects WHERE id=$1 AND deleted_at IS NULL", b.S("projectId"))
	if p == nil || p.S("lifecycle") != "active" {
		return
	}
	fields := Object{"source_operation_id": publish.S("id")}
	if c := b.S("credentialRefId"); c != "" {
		if problem := credentialProblem(t, b.S("tenantId"), c, b.S("repositoryUrl"), ""); problem != "" {
			t.exec("UPDATE repository_operations SET result=result||jsonb_build_object('resyncSkipped',$2::text) WHERE id=$1", publish.S("id"), problem)
			return
		}
		fields["credential_ref_id"], fields["credential_version"] = c, t.one("SELECT store_version FROM credential_refs WHERE id=$1", c).N("storeVersion")
	}
	insertSync(t, b, "after_publish", fields)
}
