package contract

import (
	"strings"

	"github.com/wanglongan587/cloud/internal/api/router"
)

// The external Git line's public surface: Git credential references, a Project's code source
// (Binding, current snapshot, sync and publish Operations) and the Workspace branches a publish
// may use as its source. Kept apart from openapi.go so that file stays one concern per section.

func oid() obj { return obj{"type": "string", "pattern": "^([0-9a-f]{40}|[0-9a-f]{64})$"} }

func repositorySchemas(s obj) {
	capability := enumeration("read", "publish")
	s["GitCredentialSubmission"] = object(obj{
		"submissionId": uuid(), "grant": obj{"type": "string", "description": "One-time submission grant, presented to the Credential Store as `Authorization: Ora-Submission <grant>`. It authorizes writing one version, expires within five minutes and is not a credential."},
		"ingressUrl":   obj{"type": "string", "description": "The Credential Store browser entry the token is PUT to, as `{\"token\": \"...\"}`. The token never goes to Cloud."},
		"storeVersion": number(), "expiresAt": timestamp(),
	}, "submissionId", "grant", "ingressUrl", "storeVersion", "expiresAt")
	s["GitCredentialVerification"] = object(obj{"id": uuid(), "storeVersion": number(), "state": enumeration("pending", "leased", "running", "succeeded", "failed"), "result": optional(enumeration("verified", "push_denied", "verification_failed")), "createdAt": timestamp(), "finishedAt": optional(timestamp())}, "id", "storeVersion", "state", "createdAt")
	s["GitCredential"] = object(obj{
		"id": uuid(), "tenantId": uuid(), "name": str(), "attribution": enumeration("personal", "team", "unknown"), "ownerUserId": uuid(),
		"repositoryScope": str(), "secretState": enumeration("awaiting_secret", "stored", "revoked"), "availability": enumeration("available", "frozen", "invalid"),
		"unavailableReason": optional(str()), "storeVersion": number(), "capabilities": array(capability), "requestedCapabilities": array(capability),
		"capabilityEvidence": obj{"type": "object", "additionalProperties": true, "description": "Per capability: when it was proven, for which storeVersion, and by which sync Operation or verification."},
		"pendingSubmission":  optional(object(obj{"submissionId": uuid(), "storeVersion": number(), "expiresAt": timestamp()}, "submissionId", "storeVersion", "expiresAt")),
		"lastVerification":   nullableRepositorySchema(s, "GitCredentialVerification"),
		"version":            number(), "createdAt": timestamp(), "revokedAt": optional(timestamp()),
	}, "id", "tenantId", "name", "attribution", "ownerUserId", "repositoryScope", "secretState", "availability", "storeVersion", "capabilities", "requestedCapabilities", "capabilityEvidence", "version", "createdAt")
	nullable := func(s obj) obj { return optional(s) }
	s["RepositoryOperation"] = object(obj{
		"id": uuid(), "type": enumeration("repository_sync", "repository_publish"), "tenantId": uuid(), "projectId": uuid(), "bindingId": uuid(),
		"state":   enumeration("awaiting_approval", "pending", "leased", "running", "uncertain", "succeeded", "failed", "superseded", "cancelled", "conflict", "rejected"),
		"trigger": str(), "errorCode": nullable(str()), "requestedBy": nullable(uuid()), "sourceOperationId": nullable(uuid()), "replayCount": number(),
		"credentialRefId": nullable(uuid()), "credentialVersion": nullable(number()), "createdAt": timestamp(), "startedAt": nullable(timestamp()), "finishedAt": nullable(timestamp()),
		"result":    obj{"type": "object", "additionalProperties": true, "description": "Settled facts: commitOid and forced for a sync, observed external values for a publish, a redacted detail. Never a credential or raw platform output."},
		"sourceRef": nullable(str()), "sourceCommit": nullable(oid()), "targetBranch": nullable(str()), "expectedOldOid": nullable(oid()), "revisionId": nullable(uuid()),
		"approverKind": nullable(enumeration("credential_owner", "publisher")), "approvedBy": nullable(uuid()), "approvedAt": nullable(timestamp()), "approvalExpiresAt": nullable(timestamp()),
		"checkAttempts": number(), "nextCheckAt": nullable(timestamp()),
	}, "id", "type", "tenantId", "projectId", "bindingId", "state", "trigger", "replayCount", "createdAt", "result", "checkAttempts")
	s["ProjectRepository"] = object(obj{
		"binding":    object(obj{"id": uuid(), "projectId": uuid(), "repositoryUrl": str(), "branch": str(), "credentialRefId": optional(uuid()), "resyncRequested": boolean(), "version": number()}, "id", "projectId", "repositoryUrl", "branch", "resyncRequested", "version"),
		"snapshot":   optional(object(obj{"commitOid": oid(), "snapshotRef": str(), "syncOperationId": uuid(), "recordedAt": timestamp(), "forced": boolean()}, "commitOid", "snapshotRef", "syncOperationId", "recordedAt", "forced")),
		"latestSync": nullableRepositorySchema(s, "RepositoryOperation"),
		"credential": nullableRepositorySchema(s, "GitCredential"),
	}, "binding")
	s["WorkspaceBranch"] = object(obj{"ref": str(), "branch": str(), "commit": oid()}, "ref", "branch", "commit")
}

func isRepositoryRoute(r router.Route) bool {
	return strings.Contains(r.Path, "/git-credentials") || strings.Contains(r.Path, "/projects/:pid/repository")
}

func repositoryList(r router.Route) bool {
	return r.Method == "GET" && (strings.HasSuffix(r.Path, "/git-credentials") || strings.HasSuffix(r.Path, "/repository/operations"))
}

func resourceOf(name string) obj { return object(obj{"resource": ref(name)}, "resource") }

func repositoryResponse(r router.Route) (obj, string) {
	switch {
	case r.Path == router.WorkspaceBranchesPath:
		return object(obj{"items": array(ref("WorkspaceBranch"))}, "items"), "200"
	case r.Path == router.PublishPath:
		return resourceOf("RepositoryOperation"), "201"
	case strings.Contains(r.Path, "/git-credentials"):
		switch {
		case repositoryList(r):
			return object(obj{"items": array(ref("GitCredential")), "nextCursor": str()}, "items", "nextCursor"), "200"
		case r.Method == "GET":
			return ref("GitCredential"), "200"
		case strings.HasSuffix(r.Path, "/git-credentials"):
			return object(obj{"resource": ref("GitCredential"), "submission": ref("GitCredentialSubmission")}, "resource", "submission"), "201"
		case strings.HasSuffix(r.Path, "/rotate"):
			return object(obj{"resource": ref("GitCredential"), "submission": ref("GitCredentialSubmission")}, "resource", "submission"), "200"
		case strings.HasSuffix(r.Path, "/verifications"):
			return resourceOf("GitCredentialVerification"), "202"
		}
		return resourceOf("GitCredential"), "200"
	case repositoryList(r):
		return object(obj{"items": array(ref("RepositoryOperation")), "nextCursor": str()}, "items", "nextCursor"), "200"
	case r.Method == "GET" && strings.HasSuffix(r.Path, "/repository"):
		return ref("ProjectRepository"), "200"
	case r.Method == "GET":
		return ref("RepositoryOperation"), "200"
	case strings.HasSuffix(r.Path, "/syncs") || strings.HasSuffix(r.Path, "/repository/credential"):
		return resourceOf("RepositoryOperation"), "202"
	}
	return resourceOf("RepositoryOperation"), "200"
}

func repositoryInput(name string) (obj, bool) {
	switch name {
	case "attribution":
		return enumeration("personal", "team"), true
	case "repositoryScope":
		return obj{"type": "string", "maxLength": 2048, "description": "The one https repository the credential works on; normalized (lowercase host, no :443, no trailing / or .git)."}, true
	case "requestedCapabilities":
		return array(enumeration("read", "publish")), true
	case "submissionId", "revisionId":
		return uuid(), true
	case "sourceRef":
		return obj{"type": "string", "description": "refs/heads/workspaces/<workspaceId>/<branch> or refs/ora/deliveries/<deliveryWorkId> of this Project."}, true
	case "sourceCommit", "expectedOldOid":
		return oid(), true
	case "targetBranch":
		return obj{"type": "string", "minLength": 1, "maxLength": 200}, true
	}
	return nil, false
}

func repositoryOptional(name string, r router.Route) bool {
	if r.Path == router.PublishPath {
		return name == "sourceRef" || name == "sourceCommit" || name == "revisionId" || name == "credentialRefId"
	}
	return name == "requestedCapabilities"
}

func repositoryDescription(r router.Route) string {
	switch {
	case r.Path == router.CredentialConfirmPath:
		return "Confirms that the browser submitted the pending version to the Credential Store. Cloud does not trust the report: it asks the Store which version and submission it holds and only an exact match makes the reference stored. A new version has no capability until proven; publishes approved for the previous version are cancelled (credential_changed). 409 submission_not_pending / submission_not_stored; 503 credential_store_unavailable."
	case strings.HasSuffix(r.Path, "/git-credentials") && r.Method == "POST":
		return "Creates a Git credential reference and its one-time submission grant. The request never carries a token (a token field is rejected as unknown_field); the browser PUTs the token to submission.ingressUrl. Team credentials are created by tenant admins only; a personal credential belongs to its creator. 503 credential_store_unconfigured."
	case strings.HasSuffix(r.Path, "/rotate"):
		return "Issues a submission grant for the reference's next version. Owner (personal) or admin (team) only. A still-valid pending submission is returned again rather than replaced."
	case strings.HasSuffix(r.Path, "/verifications"):
		return "Requests a push-capability verification of the current version: the Repo Executor reads the repository's receive-pack ref advertisement without writing. verified adds publish to capabilities; push_denied keeps it out."
	case strings.Contains(r.Path, "/git-credentials") && r.Method == "DELETE":
		return "Revokes the reference: Cloud stops issuing authorizations at once, then revokes in the Credential Store, retried until it confirms. An authorization issued before may still be redeemed within its lifetime (at most two minutes). Owner or tenant admin."
	case strings.Contains(r.Path, "/git-credentials"):
		return "Git credential reference metadata, visible to tenant members. Never contains the token."
	case r.Path == router.PublishPath:
		return "Creates a PublishOperation: fast-forward targetBranch of the Binding's repository from expectedOldOid (40/64 zeros creates the branch) to sourceCommit, taken from sourceRef (any Workspace branch or delivery ref of this Project, confirmed through the Git Server ref query) or from revisionId. Fields are immutable. A personal credential is approved by its owner only, a team credential by a tenant admin; when the requester is the approver it starts pending. The credential must have proven publish (409 credential_publish_unverified). 400 nothing_to_publish when expectedOldOid equals sourceCommit; 409 source_ref_not_found."
	case r.Path == router.WorkspaceBranchesPath:
		return "Lists one Workspace's branches (refs/heads/workspaces/<workspaceId>/*) of the Project repository through the Git Server ref query, for choosing a publish source. The answer is the query's moment only."
	case strings.HasSuffix(r.Path, "/repository/syncs"):
		return "Starts a manual sync of the Binding's branch. An unfinished sync is returned instead (200), so concurrent triggers share one Operation. The Project must be active and its credential usable (409 repository_credential_unavailable, repository_credential_frozen, repository_credential_invalid, repository_credential_scope_mismatch, credential_not_in_store)."
	case strings.HasSuffix(r.Path, "/repository/credential"):
		return "Proposes another credential for the Binding. A credential_candidate sync proves it can read; on success the Binding switches to it atomically if its version is unchanged. 409 sync_in_progress while a sync is unfinished."
	case strings.HasSuffix(r.Path, "/approve") || strings.HasSuffix(r.Path, "/reject"):
		return "Approves or rejects an awaiting publish. Only the approver the credential determines may act: the owner of a personal credential (never an administrator on their behalf) or a tenant admin for a team credential."
	case strings.HasSuffix(r.Path, "/cancel"):
		return "Cancels the caller's own publish before it runs (awaiting_approval or pending)."
	case strings.HasSuffix(r.Path, "/check"):
		return "Schedules the next read-only check of an uncertain publish now (not before its task deadline). Checks never push again."
	case strings.HasSuffix(r.Path, "/repository"):
		return "The Project's code source: its Binding, the credential's metadata, the current snapshot (the latest sync Cloud verified on the Git Server) and the latest sync."
	}
	return "Syncs and publishes of the Project repository, newest first."
}

// OpenAPI 3.0 ignores siblings of $ref, including nullable. Copy the schema at
// nullable association sites so validators and generated clients agree on null.
func nullableRepositorySchema(s obj, name string) obj {
	source := s[name].(obj)
	copy := obj{}
	for k, v := range source {
		copy[k] = v
	}
	return optional(copy)
}
