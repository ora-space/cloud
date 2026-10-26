package core

import "time"

// Shared mechanics of repository Operations (contract D5 item 3, sync ADR D2, publish ADR D3).
// Both types live in one table so the Git Server resolves an Operation ID with one lookup; every
// state change goes through the helpers here so finishing, auditing and invalidation stay uniform.

// Timing of the authorization an Operation carries into the executor. The authorization only
// covers starting the task and redeeming the credential; the task itself is bounded by the lease,
// which the worker renews (executor ADR D2).
const (
	authorizationLifetime = 2 * time.Minute
	// maxCheckAttempts bounds the read-only checks of an uncertain publish (publish ADR D5).
	maxCheckAttempts = 20
	// maxAutomaticReplays bounds consecutive replays of a sync lost to its worker or executor.
	maxAutomaticReplays = 3
)

// seconds renders a duration for make_interval, so every lease and window is computed on the
// database clock rather than a process clock.
func seconds(d time.Duration) float64 { return d.Seconds() }

// repositoryOperationView is the public document of an Operation: an allowlist of what a
// Project member may read. Credential plaintext never exists here; the raw executor output never
// does either, only the safe, redacted detail the executor reports.
func repositoryOperationView(o Object) Object {
	out := Object{}
	for _, k := range []string{
		"id", "type", "tenantId", "projectId", "bindingId", "state", "trigger", "errorCode", "requestedBy", "sourceOperationId", "replayCount",
		"credentialRefId", "credentialVersion", "createdAt", "startedAt", "finishedAt", "result",
		"sourceRef", "sourceCommit", "targetBranch", "expectedOldOid", "revisionId", "approverKind", "approvedBy", "approvedAt", "approvalExpiresAt", "checkAttempts", "nextCheckAt",
	} {
		if v, ok := o[k]; ok {
			out[k] = v
		}
	}
	return out
}

// finishRepositoryOperation ends an unfinished Operation in a terminal state with a safe error
// code and merges result facts. The caller has already decided the transition is legal; the
// table's CHECK constraints keep state, terminal time and code consistent.
func finishRepositoryOperation(t *transaction, id, state, code string, result Object) {
	var errorCode any
	if code != "" {
		errorCode = code
	}
	t.exec(`UPDATE repository_operations SET state=$2,error_code=$3,finished_at=clock_timestamp(),result=result||$4::jsonb,next_check_at=NULL
	 WHERE id=$1 AND state IN ('awaiting_approval','pending','leased','running','uncertain')`, id, state, errorCode, jsonText(result))
	auditRepositoryOperation(t, id, state, "", Object{"errorCode": errorCode})
	invalidateRepository(t, id)
}

// auditRepositoryOperation appends one event. The audit is append-only (a trigger refuses updates
// and deletes) and carries identifiers, codes and observed object names only.
func auditRepositoryOperation(t *transaction, id, kind, actor string, detail Object) {
	var by any
	if actor != "" {
		by = actor
	}
	t.exec("INSERT INTO repository_operation_events(operation_id,tenant_id,kind,actor_user_id,detail) SELECT id,tenant_id,$2,$3,$4::jsonb FROM repository_operations WHERE id=$1", id, kind, by, jsonText(detail))
}

// invalidateRepository tells the Project's space that its repository state changed. The notice
// carries no data: clients re-read the REST resources (sync ADR D8).
func invalidateRepository(t *transaction, operationID string) {
	p := t.one("SELECT p.id,p.space_id FROM repository_operations o JOIN projects p ON p.id=o.project_id WHERE o.id=$1", operationID)
	if p != nil && p.S("spaceId") != "" {
		t.events = append(t.events, SpaceEvent{Type: "project.repository_updated", SpaceID: p.S("spaceId"), ProjectID: p.S("id")})
	}
}

// insertSync creates one sync Operation for the Binding. fields uses column names: requested_by,
// source_operation_id, replay_count, credential_ref_id, credential_version, binding_version. The
// partial unique index refuses a second unfinished sync, so callers look for one first.
func insertSync(t *transaction, b Object, trigger string, fields Object) Object {
	id := newID()
	t.exec(`INSERT INTO repository_operations(id,type,tenant_id,project_id,binding_id,state,trigger,requested_by,source_operation_id,replay_count,credential_ref_id,credential_version,binding_version)
	 SELECT $1,'repository_sync',$2,$3,$4,'pending',$5,x.requested_by,x.source_operation_id,COALESCE(x.replay_count,0),x.credential_ref_id,x.credential_version,x.binding_version
	 FROM jsonb_to_record($6::jsonb) AS x(requested_by uuid,source_operation_id uuid,replay_count int,credential_ref_id uuid,credential_version bigint,binding_version bigint)`,
		id, b.S("tenantId"), b.S("projectId"), b.S("id"), trigger, jsonText(fields))
	actor, _ := fields["requested_by"].(string)
	auditRepositoryOperation(t, id, "created", actor, Object{"trigger": trigger})
	invalidateRepository(t, id)
	return t.one("SELECT * FROM repository_operations WHERE id=$1", id)
}

// bindingOf loads the Project's single Binding (phase one).
func bindingOf(t *transaction, projectID string) Object {
	b := t.one("SELECT * FROM repository_bindings WHERE project_id=$1", projectID)
	require(b != nil, 404, "not_found")
	return b
}

// unfinishedSync returns the Binding's one unfinished sync, if any.
func unfinishedSync(t *transaction, bindingID string) Object {
	return t.one("SELECT * FROM repository_operations WHERE binding_id=$1 AND type='repository_sync' AND state IN ('pending','leased','running')", bindingID)
}
