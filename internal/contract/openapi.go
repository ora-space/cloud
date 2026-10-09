// Package contract builds the explicit OpenAPI contract for the implemented route allowlist.
package contract

import (
	"strings"

	"github.com/wanglongan587/cloud/internal/api/router"
	"github.com/wanglongan587/cloud/internal/core"
)

type obj = map[string]any

func asObject(v any) obj {
	o, ok := v.(map[string]any)
	if !ok {
		panic("invalid static OpenAPI object")
	}
	return o
}
func properties(s obj, name string) obj { return asObject(asObject(s[name])["properties"]) }

func ref(name string) obj              { return obj{"$ref": "#/components/schemas/" + name} }
func str() obj                         { return obj{"type": "string"} }
func number() obj                      { return obj{"type": "integer", "format": "int64"} }
func boolean() obj                     { return obj{"type": "boolean"} }
func enumeration(values ...string) obj { return obj{"type": "string", "enum": values} }
func array(item obj) obj               { return obj{"type": "array", "items": item} }

// contextRefTypeEnum is the closed set of issue context-ref types, shared by the ContextRef resource
// and by the applied-suggestion refs a confirm/assist body may carry.
func contextRefTypeEnum() obj {
	return enumeration("parent_issue", "run", "timeline_message", "pull_request", "project", "workspace", "acceptance_criteria")
}
func optional(s obj) obj { s["nullable"] = true; return s }

// object omits "required" when empty: OpenAPI 3.0 demands at least one item when the key is
// present, and strict downstream generators (the frontend's orval) reject null or [] there.
func object(properties obj, required ...string) obj {
	o := obj{"type": "object", "properties": properties, "additionalProperties": false}
	if len(required) > 0 {
		o["required"] = required
	}
	return o
}
func uuid() obj      { return obj{"type": "string", "format": "uuid"} }
func timestamp() obj { return obj{"type": "string", "format": "date-time"} }
func fields(names string) obj {
	p := obj{}
	for _, name := range strings.Fields(names) {
		switch {
		case name == "id" || strings.HasSuffix(name, "Id"):
			p[name] = uuid()
		case strings.HasSuffix(name, "At"):
			p[name] = timestamp()
		case name == "version" || name == "runtimeGeneration" || name == "admissionEpoch" || name == "generation" || name == "controllerEpoch" || name == "protocolVersion" || name == "idleAdmissionEpoch" || name == "layoutVersion" || name == "reconciledEpoch":
			p[name] = number()
		case name == "admissionOpen" || name == "initialized":
			p[name] = boolean()
		default:
			p[name] = str()
		}
	}
	return p
}

func resource(names, nullableNames string) obj {
	p := fields(names)
	for _, n := range strings.Fields(nullableNames) {
		p[n] = optional(asObject(p[n]))
	}
	return object(p, strings.Fields(names)...)
}

// Document returns complete schemas and operations. cmd/openapi writes its reviewable JSON artifact.
func Document() map[string]any {
	s := obj{}
	s["Error"] = object(obj{"code": str(), "params": obj{"type": "object", "additionalProperties": true}, "requestId": uuid()}, "code", "params", "requestId")
	s["User"] = resource("id displayName status version createdAt deletedAt", "deletedAt")
	s["Tenant"] = resource("id name status role", "")
	s["TenantCreated"] = object(obj{"tenant": ref("Tenant"), "space": ref("Space")}, "tenant", "space")
	s["Member"] = resource("tenantId userId role status version createdAt", "")
	s["MemberListItem"] = resource("id tenantId userId role status version displayName", "")
	s["Space"] = resource("id tenantId name slug description createdBy version createdAt updatedAt archivedAt", "archivedAt")
	s["SpaceListItem"] = resource("id tenantId name slug description createdBy version createdAt updatedAt archivedAt role", "archivedAt")
	s["DirectoryPerson"] = resource("globalUserId name employeeNumber departmentName", "")
	properties(s, "DirectoryPerson")["globalUserId"] = str()
	s["HuaweiMember"] = resource("tenantId userId role status version displayName", "")
	s["JoinedMembership"] = resource("tenantId userId role status version name", "")
	s["Invitation"] = object(obj{"id": uuid(), "tenantId": uuid(), "createdBy": uuid(), "createdAt": timestamp(), "expiresAt": timestamp(), "revokedAt": optional(timestamp()), "consumedBy": optional(uuid()), "consumedAt": optional(timestamp()), "version": number()}, "id", "tenantId", "createdBy", "createdAt", "expiresAt", "version")
	s["JoinLink"] = object(obj{"id": uuid(), "tenantId": uuid(), "createdBy": uuid(), "createdAt": timestamp(), "expiresAt": timestamp(), "revokedAt": optional(timestamp()), "version": number()}, "id", "tenantId", "createdBy", "createdAt", "expiresAt", "version")
	s["JoinRequest"] = object(obj{"id": uuid(), "tenantId": uuid(), "userId": uuid(), "linkId": uuid(), "status": enumeration("pending", "approved", "rejected"), "createdAt": timestamp(), "decidedAt": optional(timestamp()), "decidedBy": optional(uuid()), "version": number(), "name": str(), "displayName": str()}, "id", "tenantId", "userId", "linkId", "status", "createdAt", "version")
	// issue_run.thread_appended / issue_run.thread_changed address one agent run's Thread (Thread
	// D5): issueId and runId name it and lastSeq is only a hint — the Thread GET alone moves a
	// client's cursor. Every other event omits the three fields.
	s["SpaceEvent"] = object(obj{"type": enumeration("space.updated", "space.member_updated", "project.created", "project.updated", "project.archived", "space.plugins_updated", "plugins.catalog_updated", "issue_run.thread_appended", "issue_run.thread_changed"), "spaceId": uuid(), "projectId": optional(uuid()), "version": number(), "issueId": uuid(), "runId": uuid(), "lastSeq": number()}, "type", "spaceId")
	// GitIdentity is the identity the caller's Agent runs commit as (identity-access git identity
	// D1). version is 0 while isDefault is true: no identity is stated and the default applies.
	s["GitIdentity"] = object(obj{"name": str(), "email": str(), "isDefault": boolean(), "version": number()}, "name", "email", "isDefault", "version")
	s["Project"] = resource("id tenantId ownerUserId spaceId name repositoryUrl defaultBranch credentialRefId lifecycle version createdAt deletedAt", "credentialRefId deletedAt")
	properties(s, "Project")["repositoryCredentialRefId"] = optional(uuid())
	s["Workspace"] = resource("id tenantId ownerUserId projectId kind desiredState observedState runtimeGeneration version admissionOpen admissionEpoch createdAt deletedAt requestedRef baseCommitId creatorUserId creatorOperationId creatorEvidence", "deletedAt baseCommitId creatorUserId creatorOperationId")
	// branchName is the retired linked-worktree branch; only Workspaces created before the Node
	// clone flow have one.
	s["WorkspaceListItem"] = object(fields("id tenantId ownerUserId projectId kind desiredState observedState runtimeGeneration version createdAt deletedAt creatorUserId creatorOperationId creatorEvidence canUse requestedRef baseCommitId branchName title admissionOpen admissionEpoch"), strings.Fields("id tenantId ownerUserId projectId kind desiredState observedState runtimeGeneration version createdAt deletedAt creatorUserId creatorEvidence canUse")...)
	for _, key := range []string{"deletedAt", "creatorUserId", "creatorOperationId", "branchName", "title"} {
		properties(s, "WorkspaceListItem")[key] = optional(asObject(properties(s, "WorkspaceListItem")[key]))
	}
	properties(s, "WorkspaceListItem")["canUse"] = boolean()

	s["Comment"] = resource("id tenantId issueId authorUserId authorType authorId parentId body seq version createdAt updatedAt deletedAt", "authorUserId authorId parentId deletedAt")
	commentProps := properties(s, "Comment")
	commentProps["seq"] = number()
	s["Label"] = resource("id tenantId name color version createdAt updatedAt deletedAt", "deletedAt")
	s["Workflow"] = resource("id tenantId name description graph version createdAt updatedAt deletedAt", "deletedAt")
	properties(s, "Workflow")["graph"] = obj{"type": "object", "additionalProperties": true, "description": "The authored graph document: nodes, edges, viewport, editor annotations and global variables. Stored and returned whole; the editor is its only reader."}
	s["WorkflowSnapshot"] = resource("id tenantId workflowId version name graph createdAt", "")
	properties(s, "WorkflowSnapshot")["graph"] = obj{"type": "object", "additionalProperties": true, "description": "The graph document frozen at publish time; restoring this snapshot writes it back to the workflow's live graph."}
	s["WorkflowRun"] = resource("id tenantId workflowId snapshotId name status input nodeStates rounds error startedAt finishedAt createdAt updatedAt workflowName", "startedAt finishedAt")
	workflowRunProps := properties(s, "WorkflowRun")
	workflowRunProps["status"] = enumeration("pending", "running", "awaiting_input", "succeeded", "failed", "cancelled")
	workflowRunProps["input"] = obj{"type": "object", "additionalProperties": true, "description": "The kickoff input passed to the run's start node."}
	workflowRunProps["nodeStates"] = obj{"type": "object", "additionalProperties": true, "description": "Per-node execution state keyed by node id: status, output, error, timestamps."}
	workflowRunProps["rounds"] = array(obj{"type": "object", "additionalProperties": true})
	// definitionSnapshot lives only on a detail read (the history list trips without it);
	// it is optional so one schema serves both list rows and the composed run page.
	workflowRunProps["definitionSnapshot"] = optional(obj{"type": "object", "additionalProperties": true, "description": "The frozen graph document this run executed against, from the snapshot it captured."})
	s["IssueStatus"] = resource("id tenantId key name description category color icon isSystem position version createdAt updatedAt deletedAt", "deletedAt")
	statusProps := properties(s, "IssueStatus")
	statusProps["isSystem"] = boolean()
	statusProps["category"] = enumeration("unstarted", "started", "done", "closed")
	statusProps["position"] = obj{"type": "number", "format": "double"}
	s["IssueView"] = resource("id tenantId ownerUserId name filter version createdAt updatedAt deletedAt", "deletedAt")
	viewProps := properties(s, "IssueView")
	viewProps["filter"] = obj{"type": "object", "additionalProperties": true}
	s["Issue"] = resource("id tenantId creatorUserId assigneeUserId assigneeType assigneeId parentIssueId title description status priority position number version createdAt updatedAt deletedAt properties labels projectRef", "assigneeUserId assigneeId parentIssueId deletedAt projectRef")
	issueProps := properties(s, "Issue")
	issueProps["status"] = obj{"type": "string", "pattern": "^[a-z0-9][a-z0-9_]{0,31}$"}
	issueProps["priority"] = enumeration("urgent", "high", "medium", "low", "none")
	issueProps["position"] = obj{"type": "number", "format": "double"}
	issueProps["number"] = number()
	issueProps["properties"] = obj{"type": "object", "additionalProperties": true}
	issueProps["labels"] = array(ref("Label"))
	issueProps["assigneeType"] = enumeration("user", "agent", "team")
	s["IssueRun"] = resource("id tenantId issueId version executorType executorId externalExecutionId executionContextRef workflowInvocationRef triggerEvidenceKind triggerEvidenceRefId status parentRunId retryOfRunId rerunOfRunId delegatedFromRunId attempt maxAttempts input result error failureReason triggerSummary queuedAt dispatchedAt startedAt completedAt fireAt leaseExpiresAt createdAt updatedAt deletedAt", "executionContextRef workflowInvocationRef triggerEvidenceRefId parentRunId retryOfRunId rerunOfRunId delegatedFromRunId result dispatchedAt startedAt completedAt fireAt leaseExpiresAt deletedAt")
	runProps := properties(s, "IssueRun")
	runProps["executorType"] = enumeration("agent", "team", "workflow")
	runProps["status"] = enumeration("queued", "dispatched", "running", "completed", "failed", "cancelled", "deferred")
	runProps["attempt"] = number()
	runProps["maxAttempts"] = number()
	runProps["input"] = obj{"type": "object", "additionalProperties": true}
	runProps["result"] = optional(obj{"type": "object", "additionalProperties": true})
	runProps["externalExecutionId"] = str()
	// revision is the run's registered Revision as metadata only (Cloud Revision D5): no object key,
	// ref, digest or URL is public. Null until a Revision is registered, and for every non-agent run.
	runProps["revision"] = optional(object(obj{"id": uuid(), "baseCommit": str(), "finalCommit": str(), "changed": boolean(), "bundleSize": optional(number()), "historySize": number(), "createdAt": timestamp()}, "id", "baseCommit", "finalCommit", "changed", "bundleSize", "historySize", "createdAt"))
	runProps["executionContextRef"] = optional(uuid())
	runProps["workflowInvocationRef"] = optional(uuid())
	// ThreadEntry is one ordered record of an Agent run's Thread (Thread D1/D2/D3). `seq` is the
	// run-scoped conversation order, starting at 1 and gapless; `record` is the verbatim ora-history
	// line, which Cloud stores and returns without ever rewriting it. `turnId` and `status` describe
	// the per-turn lifecycle and exist only for a user turn — a `queued` entry becomes `delivered`
	// when the Node echoes that turn_id — so Node and system entries carry neither. The Node's own
	// execution id and sequence are deliberately not part of this resource.
	s["ThreadEntry"] = object(obj{
		"seq":       obj{"type": "integer", "format": "int64", "minimum": 1},
		"source":    enumeration("node", "user", "system"),
		"kind":      str(),
		"record":    obj{"type": "object", "additionalProperties": true},
		"turnId":    optional(uuid()),
		"status":    optional(obj{"type": "string", "enum": []string{"queued", "delivered", "discarded"}, "nullable": true, "description": "User-turn lifecycle. Null for node and system entries."}),
		"createdAt": timestamp(),
	}, "seq", "source", "kind", "record", "createdAt")
	s["ContextRef"] = resource("id tenantId issueId refType refId createdAt", "")
	contextRefProps := properties(s, "ContextRef")
	contextRefProps["refType"] = contextRefTypeEnum()
	s["InteractionDescriptor"] = object(obj{"mode": enumeration("mention", "task", "form"), "requiresTask": boolean(), "formRef": optional(str())}, "mode", "requiresTask")
	s["FormOption"] = object(obj{"value": str(), "label": str()}, "value", "label")
	s["FormField"] = object(obj{
		"key": str(), "label": str(),
		"type":         enumeration("text", "textarea", "number", "boolean", "select", "multi_select"),
		"required":     boolean(),
		"description":  optional(str()),
		"placeholder":  optional(str()),
		"defaultValue": optional(obj{"nullable": true, "description": "Type-consistent with `type`; an array of strings for multi_select."}),
		"options":      optional(array(ref("FormOption"))),
	}, "key", "label", "type", "required")
	s["FormDescriptor"] = object(obj{"formRef": str(), "title": optional(str()), "description": optional(str()), "fields": array(ref("FormField"))}, "formRef", "fields")
	s["AssistSuggestion"] = object(obj{
		"suggestedValues":      obj{"type": "object", "additionalProperties": true},
		"suggestedContextRefs": array(ref("ContextRefRef")),
		"explanations":         optional(obj{"type": "object", "additionalProperties": true}),
	}, "suggestedValues", "suggestedContextRefs")
	s["ContextRefRef"] = object(obj{"refType": contextRefTypeEnum(), "refId": uuid()}, "refType", "refId")
	s["CollaborationTarget"] = object(obj{"type": enumeration("user", "agent", "team", "workflow"), "id": uuid(), "displayName": str(), "description": str(), "interactionDescriptor": ref("InteractionDescriptor")}, "type", "id", "displayName", "description", "interactionDescriptor")
	s["IssueInteraction"] = object(obj{"id": uuid(), "tenantId": uuid(), "issueId": uuid(), "commentId": uuid(), "targetType": enumeration("user", "agent", "team", "workflow"), "targetId": uuid(), "mode": enumeration("mention", "task", "form"), "task": str(), "runId": optional(uuid()), "input": obj{"type": "object", "additionalProperties": true}, "createdAt": timestamp()}, "id", "tenantId", "issueId", "commentId", "targetType", "targetId", "mode", "task", "runId", "input", "createdAt")
	s["TimelineEntry"] = object(obj{"kind": enumeration("comment", "activity"), "id": uuid(), "seq": number(), "createdAt": timestamp(), "authorType": enumeration("user", "agent", "team", "system"), "authorId": optional(uuid()), "authorUserId": optional(uuid()), "body": optional(str()), "parentId": optional(uuid()), "action": optional(str()), "details": optional(obj{"type": "object", "additionalProperties": true})}, "kind", "id", "seq", "createdAt", "authorType", "authorId", "authorUserId", "body", "parentId", "action", "details")
	s["AdminResource"] = resource("id projectId ownerUserId kind desiredState observedState runtimeGeneration version", "")
	s["AdminOperation"] = resource("id tenantId projectId workspaceId kind state step version createdAt updatedAt", "workspaceId")
	// Plugin marketplace catalog snapshot: cloud-authoritative listing rows the
	// UI renders without ever touching the network. id is the canonical
	// namespace/identifier pair the install API addresses.
	s["PluginCatalogEntry"] = resource("id sourceNamespace identifier title kind version description homepage license logo url sha256 targets packMembers readme marketplaceVisible sourceUrl indexedAt", "homepage license logo url sha256 targets packMembers readme")
	pluginEntryProps := properties(s, "PluginCatalogEntry")
	// Plugin versions are semver strings, not the optimistic integer `version`
	// of mutable resources; fields() typed it as a number by name.
	pluginEntryProps["version"] = str()
	pluginEntryProps["kind"] = enumeration("workbench", "agent", "webview", "skill", "mcp", "hook", "pack", "workflow")
	pluginEntryProps["marketplaceVisible"] = boolean()
	pluginEntryProps["indexedAt"] = timestamp()
	// Nullable oneOf must be inline: OpenAPI 3.0 ignores siblings of $ref, so
	// optional(ref(...)) would drop the nullable flag and reject null logos.
	pluginEntryProps["logo"] = obj{"oneOf": []any{
		object(obj{"universal": ref("PluginLogoCandidate")}, "universal"),
		object(obj{"light": ref("PluginLogoCandidate"), "dark": ref("PluginLogoCandidate")}, "light", "dark"),
	}, "nullable": true}
	pluginEntryProps["targets"] = optional(array(ref("PluginReleaseTarget")))
	pluginEntryProps["packMembers"] = optional(array(str()))
	s["PluginCatalog"] = object(obj{"items": array(ref("PluginCatalogEntry")), "syncedAt": optional(timestamp())}, "items")
	s["SpacePlugin"] = resource("id spaceId tenantId sourceNamespace identifier desiredState desiredVersion observedState observedVersion installError version createdAt updatedAt", "observedVersion installError")
	spacePluginProps := properties(s, "SpacePlugin")
	for _, key := range []string{"affectedCount", "completedCount", "waitingStartCount", "waitingControlCount", "unavailableCount", "failedCount", "desiredRevision"} {
		spacePluginProps[key] = number()
	}
	spacePluginProps["requestedByUserId"] = optional(uuid())
	spacePluginProps["desiredState"] = enumeration("installed", "removed")
	spacePluginProps["observedState"] = enumeration("pending", "installing", "installed", "failed", "removing", "removed")
	spacePluginProps["observedVersion"] = optional(str())
	spacePluginProps["installError"] = optional(str())
	s["SpacePluginList"] = object(obj{"items": array(ref("SpacePlugin"))}, "items")
	s["PluginUniversalRelease"] = object(obj{"url": str(), "sha256": str()}, "url", "sha256")
	s["PluginReleaseTarget"] = object(obj{"target": str(), "url": str(), "sha256": str()}, "target", "url", "sha256")
	s["PluginLogoCandidate"] = object(obj{"role": enumeration("universal", "light", "dark"), "extension": enumeration("svg", "png", "webp", "jpg", "jpeg")}, "role", "extension")
	s["OperationRequest"] = object(obj{"previous": obj{"type": "object", "additionalProperties": ref("Workspace")}, "pluginId": str(), "version": str(), "desiredRevision": number(), "release": ref("PluginCatalogEntry"), "plugins": array(obj{"type": "object", "additionalProperties": true}), "pluginMeta": array(object(obj{"pluginId": str(), "desiredRevision": number()}, "pluginId", "desiredRevision"))})
	s["OperationResult"] = object(obj{"resourceId": uuid(), "forceStopId": uuid()})
	s["Operation"] = resource("id tenantId actorUserId projectId workspaceId kind state step request result errorCode idempotencyKey requestHash controllerEpoch retryAt version createdAt updatedAt", "workspaceId errorCode controllerEpoch retryAt")
	opProps := properties(s, "Operation")
	opProps["request"] = ref("OperationRequest")
	opProps["result"] = ref("OperationResult")
	opProps["state"] = enumeration("queued", "running", "retry_wait", "blocked", "succeeded", "failed")
	// storage, worktree and storage_delete are retired steps that only historical operations carry.
	opProps["step"] = enumeration("storage", "worktree", "sandbox", "node", "clone", "ready", "quiesce", "terminate", "cleanup", "storage_delete", "plugin", "done")
	for _, name := range []string{"Workspace", "WorkspaceListItem", "AdminResource"} {
		p := properties(s, name)
		p["kind"] = enumeration("main", "isolated")
		p["desiredState"] = enumeration("running", "stopped", "deleted")
		p["observedState"] = enumeration("provisioning", "starting", "ready", "stopping", "stopped", "unavailable", "deleting", "deleted")
	}
	s["RuntimeForceStop"] = object(obj{"id": uuid(), "tenantId": uuid(), "workspaceId": uuid(), "actorUserId": uuid(), "reason": str(), "state": enumeration("registered", "terminating", "succeeded"), "controlEpoch": number(), "runtimeGeneration": number(), "version": number(), "controllerEpoch": optional(number()), "createdAt": timestamp(), "confirmedAt": optional(timestamp())}, "id", "tenantId", "workspaceId", "actorUserId", "reason", "state", "controlEpoch", "runtimeGeneration", "version", "createdAt")
	s["RuntimeControl"] = object(obj{"workspaceId": uuid(), "state": enumeration("idle", "acquiring", "held", "draining", "reconciling", "maintenance"), "controlEpoch": number(), "holderUserId": optional(uuid()), "expiresAt": optional(timestamp()), "version": number(), "sessionId": optional(uuid())}, "workspaceId", "state", "controlEpoch", "holderUserId", "expiresAt", "version")
	s["Lease"] = resource("name holderId epoch expiresAt", "")
	properties(s, "Lease")["holderId"] = str()
	properties(s, "Lease")["epoch"] = number()
	s["Sandbox"] = resource("id workspaceId generation substrateSandboxId observedState createdAt terminatedAt version", "substrateSandboxId terminatedAt")
	properties(s, "Sandbox")["substrateSandboxId"] = optional(str())
	s["Node"] = resource("id sandboxInstanceId serviceSubject connectionState protocolVersion initialized lastSeenAt endedAt idleAdmissionEpoch version workspaceId nodeId nodeIncarnationId", "endedAt idleAdmissionEpoch nodeId nodeIncarnationId")
	properties(s, "Node")["nodeId"] = optional(str())
	properties(s, "Node")["nodeIncarnationId"] = optional(str())
	s["Ticket"] = resource("id tenantId workspaceId nodeInstanceId actorUserId admissionEpoch kind state createdAt finishedAt version controlSessionId controlEpoch terminatedByForceStopId", "finishedAt controlSessionId controlEpoch terminatedByForceStopId")
	properties(s, "Ticket")["controlEpoch"] = optional(number())
	// storage_ensure, worktree_ensure, worktree_delete and storage_delete are retired kinds that only
	// historical effects carry.
	s["EffectRequest"] = object(obj{"kind": enumeration("storage_ensure", "worktree_ensure", "sandbox_ensure", "sandbox_terminate", "worktree_delete", "storage_delete", "workspace_data_delete", "plugin_ensure", "plugin_delete"), "projectId": uuid(), "workspaceId": uuid(), "repositoryUrl": str(), "requestedRef": str(), "sandboxInstanceId": uuid(), "pluginId": str(), "version": str(), "universal": ref("PluginUniversalRelease"), "targets": array(ref("PluginReleaseTarget"))}, "kind", "projectId")
	s["EffectResult"] = object(obj{"layoutVersion": number(), "commitId": obj{"type": "string", "pattern": "^([0-9a-f]{40}|[0-9a-f]{64})$"}, "jobTerminated": boolean(), "removed": boolean(), "terminated": boolean(), "lateEnsureFenced": boolean(), "installed": boolean(), "sandboxInstanceId": uuid(), "nodeId": str(), "version": str(), "error": str(), "diagnostic": str()})
	s["Effect"] = resource("id operationId projectId workspaceId kind state externalId request result reconciledEpoch createdAt version", "workspaceId externalId")
	ep := properties(s, "Effect")
	ep["externalId"] = optional(str())
	ep["request"] = ref("EffectRequest")
	ep["result"] = ref("EffectResult")
	s["ControllerProject"] = resource("id tenantId ownerUserId spaceId name repositoryUrl defaultBranch credentialRefId lifecycle version createdAt deletedAt secretRef", "credentialRefId deletedAt secretRef")
	properties(s, "ControllerProject")["repositoryCredentialRefId"] = optional(uuid())
	s["ControllerWorkspace"] = resource("id tenantId ownerUserId projectId kind desiredState observedState runtimeGeneration version admissionOpen admissionEpoch createdAt deletedAt requestedRef baseCommitId creatorUserId creatorOperationId creatorEvidence", "deletedAt baseCommitId creatorUserId creatorOperationId")
	properties(s, "ControllerWorkspace")["issueRunId"] = optional(uuid())
	for _, name := range []string{"Workspace", "WorkspaceListItem", "ControllerWorkspace"} {
		properties(s, name)["baseCommitId"] = optional(obj{"type": "string", "pattern": "^([0-9a-f]{40}|[0-9a-f]{64})$"})
	}
	// A Workspace operation's clone executions, registered through the gRPC ExecutionService.
	s["CloneExecution"] = resource("executionId operationId workspaceId cloneRequestId nodeId input result dispatchedEpoch createdAt updatedAt nodeOperationId terminatedByForceStopId", "workspaceId cloneRequestId result terminatedByForceStopId")
	ce := properties(s, "CloneExecution")
	ce["credentialRefId"] = optional(uuid())
	ce["credentialRefVersion"] = optional(number())
	ce["nodeId"] = str()
	ce["nodeOperationId"] = str()
	ce["executionId"] = str()
	ce["input"] = obj{"type": "object", "additionalProperties": true}
	ce["result"] = optional(obj{"type": "object", "additionalProperties": true})
	ce["dispatchedEpoch"] = number()
	// Plugin, session and delivery executions share one table. The JSON snapshot only carries the
	// plugin step's executions; session and delivery work is claimed through gRPC.
	s["NodeExecution"] = resource("executionId kind operationId workId workspaceId nodeId nodeOperationId input result dispatchedEpoch lastEventSequence terminatedByForceStopId createdAt updatedAt", "workId workspaceId result terminatedByForceStopId")
	ne := properties(s, "NodeExecution")
	ne["executionId"] = str()
	ne["kind"] = str()
	ne["nodeId"] = str()
	ne["nodeOperationId"] = str()
	ne["input"] = obj{"type": "object", "additionalProperties": true}
	ne["result"] = optional(obj{"type": "object", "additionalProperties": true})
	ne["dispatchedEpoch"] = number()
	ne["lastEventSequence"] = number()
	s["Snapshot"] = object(obj{"operation": ref("Operation"), "project": ref("ControllerProject"), "workspaces": array(ref("ControllerWorkspace")), "sandboxes": array(ref("Sandbox")), "nodes": array(ref("Node")), "effects": array(ref("Effect")), "clones": array(ref("CloneExecution")), "pluginExecutions": array(ref("NodeExecution")), "pluginInput": obj{"type": "object", "additionalProperties": true}}, "operation", "project", "workspaces", "sandboxes", "nodes", "effects", "clones", "pluginExecutions")
	s["EmptyClaim"] = object(obj{"operation": obj{"type": "object", "nullable": true, "enum": []any{nil}}}, "operation")
	s["Access"] = object(obj{"userId": uuid(), "tenantId": uuid(), "workspaceId": uuid(), "allowedAction": enumeration("read", "execute"), "executable": boolean(), "runtimeGeneration": number()}, "userId", "tenantId", "workspaceId", "allowedAction", "executable", "runtimeGeneration")
	s["IdleRefusal"] = object(obj{"accepted": boolean(), "errorCode": enumeration("resource_in_use")}, "accepted", "errorCode")
	// Clone requests mirror the transitional Controller DTO: the tagged state carries the terminal
	// fact, and identities assigned at dispatch are null until a Controller records it.
	s["CloneState"] = object(obj{"kind": enumeration("pending", "succeeded", "failed"), "path": str(), "commit": str(), "reason": enumeration("sourceUnavailable", "branchNotFound", "destinationConflict", "operationFailed", "interrupted", "unspecified"), "retainedPath": str()}, "kind")
	cloneProps := fields("operationId createdAt updatedAt")
	for _, name := range []string{"requestId", "repository", "branch"} {
		cloneProps[name] = str()
	}
	cloneProps["executionId"], cloneProps["nodeId"], cloneProps["state"] = optional(str()), optional(str()), ref("CloneState")
	s["CloneOperation"] = object(cloneProps, "operationId", "requestId", "repository", "branch", "executionId", "nodeId", "state", "createdAt", "updatedAt")
	paths := obj{}
	for _, r := range router.Routes() {
		path := r.Path
		parameters := []any{}
		for _, p := range strings.Split(path, "/") {
			if strings.HasPrefix(p, ":") {
				name := p[1:]
				path = strings.ReplaceAll(path, p, "{"+name+"}")
				parameters = append(parameters, obj{"name": name, "in": "path", "required": true, "schema": pathParamSchema(name)})
			}
		}
		public := r.Action == ""
		security := []any{obj{"serviceCredential": []string{}}}
		if public || r.Action == "access" || r.Action == "admit" {
			security = []any{obj{"serviceCredential": []string{}, "userCredential": []string{}}}
		}
		description := description(r)
		response, status := responseSchema(r)
		responses := obj{status: obj{"description": "Successful command or resource response", "content": obj{"application/json": obj{"schema": response}}}}
		for _, code := range []string{"400", "401", "403", "404", "409", "428", "500", "503"} {
			responses[code] = obj{"description": errorDescription(code), "content": obj{"application/json": obj{"schema": ref("Error")}}}
		}
		operation := obj{"operationId": strings.ToLower(r.Method) + strings.NewReplacer("/", "_", ":", "").Replace(r.Path), "tags": []string{tag(r)}, "summary": summary(r), "description": description, "security": security, "responses": responses}
		// Restoring the default git identity needs no idempotency record (there is no tenant to scope
		// one to): an identity that is already the default restores as a no-op, so a retry is safe.
		if public && (r.Method == "POST" || r.Method == "DELETE") && r.Path != "/api/v1/me/git-identity" {
			keyScope := "Scoped to tenant and user."
			if strings.HasPrefix(r.Path, "/api/v1/join/") {
				keyScope = "Scoped to the verified user before tenant membership exists."
			}
			parameters = append(parameters, obj{"name": "Idempotency-Key", "in": "header", "required": true, "schema": obj{"type": "string", "minLength": 1, "maxLength": 200}, "description": keyScope + " Same key and canonical method/path/body returns the original response before version validation; changed request is 409."})
		}
		if isList(r) {
			parameters = append(parameters, obj{"name": "limit", "in": "query", "schema": obj{"type": "integer", "minimum": 1, "maximum": 100, "default": 50}}, obj{"name": "after", "in": "query", "schema": uuid(), "description": "Exclusive UUID cursor, ascending stable ordering."})
		}
		if r.Method == "GET" && r.Path == router.ThreadPath {
			// The Thread read has its own cursor vocabulary and window: `seq` is a decimal
			// conversation position, not a UUID, and D5 caps the window at 500 rather than the
			// shared 100. All three parameters are described in full in the operation description.
			parameters = append(parameters,
				obj{"name": "limit", "in": "query", "schema": obj{"type": "integer", "minimum": 1, "maximum": core.ThreadPageLimit, "default": core.ThreadPageDefault}},
				obj{"name": "after", "in": "query", "schema": obj{"type": "integer", "format": "int64", "minimum": 0}, "description": "Forward Thread seq cursor: the window starts after this seq. Mutually exclusive with before; neither cursor reads the tail."},
				obj{"name": "before", "in": "query", "schema": obj{"type": "integer", "format": "int64", "minimum": 0}, "description": "Backward Thread seq cursor: the window ends just before this seq, taking the entries closest to it from below. Mutually exclusive with after."})
		}
		if strings.Contains(r.Path, "/collaboration/forms/") {
			// Optional, and absent by default: a caller that omits it gets the descriptor without the
			// platform fields, which is what every client served before they existed still sends.
			parameters = append(parameters, obj{"name": "issueId", "in": "query", "schema": uuid(), "description": "Issue the form is being configured for. Tailors the descriptor with the platform fields (repository, prompt) and prefills them from the issue's project repository and the workflow's Start prompt. An unknown or foreign issue is 404."})
		}
		if r.Path == "/api/v1/tenants/:tid/people" {
			parameters = append(parameters, obj{"name": "keyword", "in": "query", "required": true, "schema": obj{"type": "string", "minLength": 2, "maxLength": 100}})
		}
		if len(parameters) > 0 {
			operation["parameters"] = parameters
		}
		if r.Method != "GET" {
			properties := obj{}
			required := []string{}
			for _, name := range r.Fields {
				properties[name] = inputSchema(name, r)
				if !optionalField(name, r) {
					required = append(required, name)
				}
			}
			operation["requestBody"] = obj{"required": true, "content": obj{"application/json": obj{"schema": object(properties, required...)}}}
		}
		if paths[path] == nil {
			paths[path] = obj{}
		}
		asObject(paths[path])[strings.ToLower(r.Method)] = operation
	}
	// The SSE stream is not part of router.Routes(); it is documented manually with the
	// same authorization contract as REST (membership verified before the stream opens).
	paths["/api/v1/tenants/{tid}/spaces/{spaceId}/events"] = obj{"get": obj{
		"operationId": "getSpaceEvents",
		"tags":        []string{"spaces"},
		"summary":     "Stream collaboration space events over server-sent events",
		"description": "Membership is verified before the stream opens. Events are lightweight invalidation notices published after commit; clients refetch authoritative state over REST.",
		"parameters": []any{
			obj{"name": "tid", "in": "path", "required": true, "schema": uuid()},
			obj{"name": "spaceId", "in": "path", "required": true, "schema": uuid()},
		},
		"security": []any{obj{"serviceCredential": []string{}, "userCredential": []string{}}},
		"responses": obj{
			"200": obj{"description": "Server-sent event stream", "content": obj{"text/event-stream": obj{"schema": ref("SpaceEvent")}}},
			"401": obj{"description": errorDescription("401"), "content": obj{"application/json": obj{"schema": ref("Error")}}},
			"403": obj{"description": errorDescription("403"), "content": obj{"application/json": obj{"schema": ref("Error")}}},
			"404": obj{"description": errorDescription("404"), "content": obj{"application/json": obj{"schema": ref("Error")}}},
		},
	}}
	paths["/healthz"] = obj{"get": obj{"operationId": "health", "tags": []string{"health"}, "summary": "PostgreSQL readiness and optional dependency configuration", "responses": obj{"200": obj{"description": "Database reachable", "content": obj{"application/json": obj{"schema": object(obj{"status": enumeration("ok"), "dependencies": object(obj{"objectStore": enumeration("configured", "unconfigured")}, "objectStore")}, "status")}}}, "503": obj{"description": "Database unavailable", "content": obj{"application/json": obj{"schema": ref("Error")}}}}}}
	return obj{"openapi": "3.0.3", "info": obj{"title": "Ora Cloud phase one", "version": "1.0.0", "description": "Authoritative PostgreSQL core. Simulation is separate; no production Controller/Node/Kubernetes implementation is implied."}, "servers": []any{obj{"url": "http://localhost:8080"}}, "paths": paths, "components": obj{"schemas": s, "securitySchemes": obj{"serviceCredential": obj{"type": "http", "scheme": "bearer", "bearerFormat": "EdDSA JWT", "description": "Pinned issuer/kid/kind=service/role, aud=ora-cloud, exp and iat required, <=5 minute lifetime. Public API requires gateway; internal control requires controller; nodes require scoped node role."}, "userCredential": obj{"type": "apiKey", "in": "header", "name": "X-Ora-User-Token", "description": "Separately signed EdDSA JWT: kind=user, source+sub, caller must equal authenticated service sub, aud=ora-cloud. User and membership status checked in PostgreSQL."}}}}
}

// pathParamSchema types a path parameter. `formRef` is deliberately NOT a UUID: it is an opaque
// provider-scoped token that Issues never parses (§38.17), constrained only by the grammar that keeps
// it safe in a path segment.
func pathParamSchema(name string) obj {
	if name == "formRef" {
		return obj{"type": "string", "minLength": 1, "maxLength": 200, "pattern": "^[A-Za-z0-9._:@-]+$"}
	}
	return uuid()
}

// tag groups each operation into the OpenAPI tag module the generated client splits on.
func tag(r router.Route) string {
	switch {
	case strings.HasPrefix(r.Path, "/internal/"):
		return "internal"
	case strings.HasPrefix(r.Path, "/api/v1/me"):
		return "me"
	case strings.Contains(r.Path, "/clones"):
		return "clones"
	case strings.Contains(r.Path, "/spaces"):
		return "spaces"
	case strings.Contains(r.Path, "/workflows"):
		return "workflows"
	case strings.Contains(r.Path, "/workspaces"):
		return "workspaces"
	case strings.Contains(r.Path, "/projects"):
		return "projects"
	case strings.Contains(r.Path, "/members"):
		return "members"
	case strings.Contains(r.Path, "/operations"):
		return "operations"
	}
	return "tenants"
}

func isList(r router.Route) bool {
	return r.Method == "GET" && (strings.HasSuffix(r.Path, "/tenants") || strings.HasSuffix(r.Path, "/members") || strings.HasSuffix(r.Path, "/projects") || strings.HasSuffix(r.Path, "/workspaces") || strings.HasSuffix(r.Path, "/spaces") || strings.HasSuffix(r.Path, "/resource-status") || strings.HasSuffix(r.Path, "/issue-statuses") || strings.HasSuffix(r.Path, "/labels") || strings.HasSuffix(r.Path, "/issue-views") || strings.HasSuffix(r.Path, "/workflows") || strings.HasSuffix(r.Path, "/snapshots") || strings.HasSuffix(r.Path, "/runs") || strings.HasSuffix(r.Path, "/comments") || strings.HasSuffix(r.Path, "/subscribers") || strings.HasSuffix(r.Path, "/invitations") || strings.HasSuffix(r.Path, "/join-links") || strings.HasSuffix(r.Path, "/join-requests") || strings.HasSuffix(r.Path, "/clones"))
}

func responseSchema(r router.Route) (schema obj, status string) {
	if r.Path == "/api/v1/me/git-identity" {
		return ref("GitIdentity"), "200"
	}
	if r.Action != "" {
		switch r.Action {
		case "access":
			return ref("Access"), "200"
		case "admit", "node_finish":
			return ref("Ticket"), "200"
		case "node_register", "node_status":
			return ref("Node"), "200"
		case "node_idle":
			return obj{"oneOf": []any{ref("Node"), ref("IdleRefusal")}}, "200"
		case "lease_acquire", "lease_renew", "lease_release":
			return ref("Lease"), "200"
		case "claim":
			return obj{"oneOf": []any{ref("Snapshot"), ref("EmptyClaim")}}, "200"
		case "snapshot":
			return ref("Snapshot"), "200"
		case "plan", "effect_result":
			return object(obj{"effect": ref("Effect"), "operation": ref("Operation")}, "effect", "operation"), "200"
		default:
			return ref("Operation"), "200"
		}
	}
	if r.Path == "/api/v1/tenants/:tid/people" {
		return object(obj{"items": array(ref("DirectoryPerson"))}, "items"), "200"
	}
	if r.Path == "/api/v1/tenants/:tid/members/huawei" {
		return ref("HuaweiMember"), "200"
	}
	if r.Path == "/api/v1/join/invitations/redeem" {
		return ref("JoinedMembership"), "200"
	}
	if r.Path == "/api/v1/join/requests" {
		return ref("JoinRequest"), "201"
	}
	if strings.HasSuffix(r.Path, "/invitations") || strings.Contains(r.Path, "/invitations/:iid") {
		if isList(r) {
			return object(obj{"items": array(ref("Invitation")), "nextCursor": str()}, "items", "nextCursor"), "200"
		}
		if r.Method == "POST" {
			return ref("Invitation"), "201"
		}
		return ref("Invitation"), "200"
	}
	if strings.HasSuffix(r.Path, "/join-links") || strings.Contains(r.Path, "/join-links/:lid") {
		if isList(r) {
			return object(obj{"items": array(ref("JoinLink")), "nextCursor": str()}, "items", "nextCursor"), "200"
		}
		if r.Method == "POST" {
			return ref("JoinLink"), "201"
		}
		return ref("JoinLink"), "200"
	}
	if strings.Contains(r.Path, "/join-requests") {
		if isList(r) {
			return object(obj{"items": array(ref("JoinRequest")), "nextCursor": str()}, "items", "nextCursor"), "200"
		}
		return ref("JoinRequest"), "200"
	}
	if r.Path == "/api/v1/me/spaces" {
		return object(obj{"items": array(ref("SpaceListItem")), "nextCursor": str()}, "items", "nextCursor"), "200"
	}
	switch {
	case strings.Contains(r.Path, "/spaces"):
		switch {
		case strings.Contains(r.Path, "/plugins"):
			if r.Method == "GET" && strings.HasSuffix(r.Path, "/plugins/catalog") {
				return ref("PluginCatalog"), "200"
			}
			if r.Method == "GET" {
				return ref("SpacePluginList"), "200"
			}
			return object(obj{"resource": ref("SpacePlugin")}, "resource"), "200"
		case strings.Contains(r.Path, "/projects"):
			if r.Method == "GET" {
				return object(obj{"items": array(ref("Project")), "nextCursor": str()}, "items", "nextCursor"), "200"
			}
			return object(obj{"resource": ref("Project"), "workspace": ref("Workspace"), "operation": ref("Operation")}, "resource", "workspace", "operation"), "202"
		default:
			if r.Method == "GET" && isList(r) {
				return object(obj{"items": array(ref("SpaceListItem")), "nextCursor": str()}, "items", "nextCursor"), "200"
			}
			return ref("Space"), "200"
		}
	case strings.HasSuffix(r.Path, "/collaboration/targets"):
		return object(obj{"items": array(ref("CollaborationTarget")), "nextCursor": str()}, "items", "nextCursor"), "200"
	case strings.Contains(r.Path, "/collaboration/forms/"):
		return ref("FormDescriptor"), "200"
	case strings.HasSuffix(r.Path, "/timeline"):
		return object(obj{"items": array(ref("TimelineEntry")), "nextCursor": str()}, "items", "nextCursor"), "200"
	case strings.Contains(r.Path, "/collaboration/assist"):
		return ref("AssistSuggestion"), "200"
	case strings.Contains(r.Path, "/interactions"):
		if r.Method == "POST" {
			return object(obj{"resource": ref("IssueRun")}, "resource"), "200"
		}
		return object(obj{"items": array(ref("IssueInteraction")), "nextCursor": str()}, "items", "nextCursor"), "200"
	case strings.Contains(r.Path, "/comments"):
		if r.Method == "GET" {
			return object(obj{"items": array(ref("Comment")), "nextCursor": str()}, "items", "nextCursor"), "200"
		}
		if r.Method == "POST" {
			return object(obj{"resource": ref("Comment")}, "resource"), "200"
		}
		return ref("Comment"), "200"
	case strings.Contains(r.Path, "/subscribers"):
		if r.Method == "GET" {
			return object(obj{"items": array(ref("User")), "nextCursor": str()}, "items", "nextCursor"), "200"
		}
		return object(obj{"resource": ref("User")}, "resource"), "200"
	case strings.Contains(r.Path, "/issues") && strings.Contains(r.Path, "/labels"):
		if r.Method == "GET" {
			return object(obj{"items": array(ref("Label")), "nextCursor": str()}, "items", "nextCursor"), "200"
		}
		if r.Method == "POST" {
			return object(obj{"resource": ref("Label")}, "resource"), "200"
		}
		return ref("Label"), "200"
	case strings.HasSuffix(r.Path, "/issues/batch"):
		return object(obj{"items": array(ref("Issue")), "nextCursor": str()}, "items", "nextCursor"), "200"
	case strings.Contains(r.Path, "/issue-statuses"):
		if r.Method == "GET" {
			return object(obj{"items": array(ref("IssueStatus")), "nextCursor": str()}, "items", "nextCursor"), "200"
		}
		if r.Method == "POST" {
			return object(obj{"resource": ref("IssueStatus")}, "resource"), "200"
		}
		return ref("IssueStatus"), "200"
	case strings.Contains(r.Path, "/issue-views"):
		if r.Method == "GET" {
			return object(obj{"items": array(ref("IssueView")), "nextCursor": str()}, "items", "nextCursor"), "200"
		}
		if r.Method == "POST" {
			return object(obj{"resource": ref("IssueView")}, "resource"), "200"
		}
		return ref("IssueView"), "200"
	case strings.Contains(r.Path, "/issue-groups"):
		return object(obj{"groups": array(object(obj{"key": str(), "items": array(ref("Issue"))}, "key", "items"))}, "groups"), "200"
	case strings.Contains(r.Path, "/workflows"):
		if strings.HasSuffix(r.Path, "/publish") {
			return object(obj{"resource": ref("WorkflowSnapshot")}, "resource"), "200"
		}
		if strings.HasSuffix(r.Path, "/restore") {
			return ref("Workflow"), "200"
		}
		if strings.Contains(r.Path, "/runs") {
			// Workflow runs branch before snapshots/detail: the run paths carry no `:snapshotId`
			// and must not be read as Workflow resources.
			if r.Method == "GET" && strings.HasSuffix(r.Path, "/runs") {
				return object(obj{"items": array(ref("WorkflowRun")), "nextCursor": str()}, "items", "nextCursor"), "200"
			}
			if r.Method == "POST" {
				return object(obj{"resource": ref("WorkflowRun")}, "resource"), "200"
			}
			return ref("WorkflowRun"), "200"
		}
		if strings.Contains(r.Path, "/snapshots") {
			if r.Method == "GET" && strings.HasSuffix(r.Path, "/snapshots") {
				return object(obj{"items": array(ref("WorkflowSnapshot")), "nextCursor": str()}, "items", "nextCursor"), "200"
			}
			return ref("WorkflowSnapshot"), "200"
		}
		if r.Method == "GET" && strings.HasSuffix(r.Path, "/workflows") {
			return object(obj{"items": array(ref("Workflow")), "nextCursor": str()}, "items", "nextCursor"), "200"
		}
		if r.Method == "POST" {
			return object(obj{"resource": ref("Workflow")}, "resource"), "200"
		}
		return ref("Workflow"), "200"
	case strings.Contains(r.Path, "/labels"):
		if r.Method == "GET" {
			return object(obj{"items": array(ref("Label")), "nextCursor": str()}, "items", "nextCursor"), "200"
		}
		if r.Method == "POST" {
			return object(obj{"resource": ref("Label")}, "resource"), "200"
		}
		return ref("Label"), "200"
	case strings.HasSuffix(r.Path, "/thread/messages"):
		// Appending a user turn returns the entry it created, not the run: the caller already has
		// the run and the message is the new resource.
		return object(obj{"resource": ref("ThreadEntry")}, "resource"), "201"
	case strings.HasSuffix(r.Path, "/thread/end"):
		// Ending a Thread returns the Thread's new state, not a resource: the endpoint creates
		// nothing to point at, and the caller's next move is to watch `threadState` reach `ended`.
		// 202 because the request is accepted for asynchronous shutdown — the session is asked to
		// stop, it has not stopped.
		return object(obj{"threadState": enumeration("ending")}, "threadState"), "202"
	case strings.HasSuffix(r.Path, "/thread"):
		// Reading a Thread returns one ascending-by-seq window of entries plus the run's Thread
		// state and idle instant, all from one snapshot. `threadState` and `idleSince` are projected
		// here rather than read off the run resource, which keeps stripping these columns from
		// IssueRun (T4C-10). The window's cursors are nullable because an empty Thread has neither.
		return object(obj{
			"items":       array(ref("ThreadEntry")),
			"threadState": enumeration("pending", "active", "idle", "ending", "ended"),
			"idleSince":   obj{"type": "string", "format": "date-time", "nullable": true, "description": "When the Thread became idle; null in every other state."},
			"nextCursor":  obj{"type": "integer", "format": "int64", "nullable": true, "description": "The window's last seq, to be sent back as `after`. Null for an empty window."},
			"prevCursor":  obj{"type": "integer", "format": "int64", "nullable": true, "description": "The window's first seq, to be sent back as `before`. Null for an empty window."},
		}, "items", "threadState", "idleSince", "nextCursor", "prevCursor"), "200"
	case strings.Contains(r.Path, "/runs"):
		if r.Method == "GET" && strings.HasSuffix(r.Path, "/runs") {
			return object(obj{"items": array(ref("IssueRun")), "nextCursor": str()}, "items", "nextCursor"), "200"
		}
		if r.Method == "POST" {
			return object(obj{"resource": ref("IssueRun")}, "resource"), "200"
		}
		return ref("IssueRun"), "200"
	case strings.Contains(r.Path, "/context-refs"):
		if r.Method == "GET" {
			return object(obj{"items": array(ref("ContextRef")), "nextCursor": str()}, "items", "nextCursor"), "200"
		}
		if r.Method == "POST" {
			return object(obj{"resource": ref("ContextRef")}, "resource"), "200"
		}
		return ref("ContextRef"), "200"
	case strings.Contains(r.Path, "/issues"):
		if r.Method == "POST" && strings.HasSuffix(r.Path, "/issues") {
			return object(obj{"resource": ref("Issue")}, "resource"), "200"
		}
		if r.Method == "GET" && strings.HasSuffix(r.Path, "/issues") {
			return object(obj{"items": array(ref("Issue")), "nextCursor": str()}, "items", "nextCursor"), "200"
		}
		return ref("Issue"), "200"
	case strings.Contains(r.Path, "/clones"):
		switch {
		case isList(r):
			return object(obj{"items": array(ref("CloneOperation")), "nextCursor": str()}, "items", "nextCursor"), "200"
		case r.Method == "GET":
			return ref("CloneOperation"), "200"
		default:
			return ref("CloneOperation"), "202"
		}
	case r.Path == "/api/v1/tenants" && r.Method == "POST":
		return ref("TenantCreated"), "201"
	}
	if strings.Contains(r.Path, "/workspaces/") && (strings.HasSuffix(r.Path, "/control") || strings.Contains(r.Path, "/control/")) {
		return ref("RuntimeControl"), "200"
	}
	name := "Project"
	switch {
	case r.Path == "/api/v1/me":
		name = "User"
	case strings.HasSuffix(r.Path, "/tenants"):
		name = "Tenant"
	case strings.Contains(r.Path, "/members"):
		name = "Member"
		if r.Method == "GET" {
			name = "MemberListItem"
		}
	case strings.Contains(r.Path, "/operations"):
		name = "Operation"
	case strings.HasSuffix(r.Path, "/resource-status"):
		name = "AdminResource"
	case strings.HasSuffix(r.Path, "/force-stop"):
		if r.Method == "GET" {
			return object(obj{"forceStop": optional(ref("RuntimeForceStop"))}, "forceStop"), "200"
		}
		return object(obj{"resource": ref("AdminResource"), "forceStop": ref("RuntimeForceStop")}, "resource", "forceStop"), "202"
	case strings.HasSuffix(r.Path, "/administrative-stop"):
		name = "AdminResource"
	case strings.Contains(r.Path, "/workspaces"):
		name = "Workspace"
		if isList(r) {
			name = "WorkspaceListItem"
		}
	}
	if isList(r) {
		return object(obj{"items": array(ref(name)), "nextCursor": str()}, "items", "nextCursor"), "200"
	}
	if r.Method == "GET" || r.Method == "PATCH" || r.Method == "PUT" {
		if name == "Operation" {
			return obj{"oneOf": []any{ref("Operation"), ref("AdminOperation")}}, "200"
		}
		return ref(name), "200"
	}
	operation := ref("Operation")
	if name == "AdminResource" {
		operation = ref("AdminOperation")
	}
	if strings.HasSuffix(r.Path, "/retry") {
		return object(obj{"operation": obj{"oneOf": []any{ref("Operation"), ref("AdminOperation")}}}, "operation"), "202"
	}
	properties := obj{"resource": ref(name), "operation": operation}
	required := []string{"resource", "operation"}
	if strings.HasSuffix(r.Path, "/projects") && r.Method == "POST" {
		properties["workspace"] = ref("Workspace")
		required = append(required, "workspace")
	}
	return object(properties, required...), "202"
}

func optionalField(name string, r router.Route) bool {
	if strings.Contains(r.Path, "/issues") {
		switch name {
		case "title":
			return r.Method == "PUT"
		case "description", "status", "priority", "assigneeUserId", "assigneeType", "assigneeId", "parentIssueId", "projectRef", "beforeId", "afterId", "properties":
			return true
		}
	}
	switch name {
	case "description", "category", "color", "icon", "filter", "position", "parentId", "input", "targets", "contextRefs", "graph":
		return true
	case "name":
		// Both publish and run creation default the name to the workflow's, so the
		// request may carry none.
		return strings.HasSuffix(r.Path, "/publish") || strings.HasSuffix(r.Path, "/runs")
	case "snapshotId":
		// A run may omit the snapshot to pin the latest published one.
		return strings.HasSuffix(r.Path, "/runs")
	case "pluginVersion":
		// Omitted install pins the catalog's current version.
		return true
	case "values":
		// Confirm must state what it is confirming; assist may be asked with a still-empty form.
		return strings.HasSuffix(r.Path, "/assist")
	}
	return name == "version" && r.Method == "POST" && strings.HasSuffix(r.Path, "/plugins") || name == "credentialRefId" || name == "role" && strings.HasSuffix(r.Path, "/members/huawei") || name == "version" && r.Method == "PUT" || name == "epoch" && r.Action == "access" || name == "workspaceId" && r.Action == "plan" || name == "externalId" && r.Action == "effect_result"
}

func inputSchema(name string, r router.Route) obj {
	switch name {
	case "version", "epoch", "admissionEpoch":
		return obj{"type": "integer", "format": "int64", "minimum": 0}
	case "retrySeconds":
		return obj{"type": "integer", "minimum": 1, "maximum": 3600}
	case "protocolVersion":
		return obj{"type": "integer", "enum": []int{1}}
	case "initialized", "idle", "impactConfirmed":
		return boolean()
	case "result":
		return ref("EffectResult")
	case "action":
		return enumeration("read", "execute")
	case "role":
		return enumeration("admin", "member")
	case "status":
		if strings.Contains(r.Path, "/issues") {
			return obj{"type": "string", "pattern": "^[a-z0-9][a-z0-9_]{0,31}$"}
		}
		return enumeration("active", "disabled")
	case "priority":
		return enumeration("urgent", "high", "medium", "low", "none")
	case "assigneeType":
		return enumeration("user", "agent", "team")
	case "executorType":
		return enumeration("agent", "team", "workflow")
	case "refType":
		return contextRefTypeEnum()
	case "connectionState":
		return enumeration("connected", "disconnected")
	case "state":
		if r.Action == "defer" {
			return enumeration("blocked", "retry_wait")
		}
		return enumeration("running", "succeeded", "failed", "absent")
	case "kind":
		if r.Action == "admit" {
			return enumeration("task", "interaction")
		}
		return enumeration("sandbox_ensure", "sandbox_terminate", "workspace_data_delete", "plugin_ensure", "plugin_delete")
	case "errorCode":
		return enumeration("substrate_timeout", "termination_unconfirmed", "git_cleanup_failed", "node_unavailable", "external_failure", "clone_failed", "clone_result_unknown", "plugin_execution_failed", "plugin_result_unknown")
	case "tenantId", "operationId", "ticketId", "credentialRefId":
		return uuid()
	case "category":
		return enumeration("unstarted", "started", "done", "closed")
	case "labelId", "userId", "assigneeId", "executorId", "refId", "parentId", "projectRef", "targetId":
		return uuid()
	case "ids":
		return array(uuid())
	case "filter", "properties", "input", "values", "graph":
		return obj{"type": "object", "additionalProperties": true}
	case "contextRefs":
		return array(ref("ContextRefRef"))
	case "position":
		return number()
	case "slug":
		return obj{"type": "string", "pattern": "^[a-z0-9][a-z0-9-]{0,63}$", "description": "Lowercase, immutable, globally unique."}
	case "workspaceId":
		if r.Action == "plan" {
			return str()
		}
		return uuid()
	case "targets":
		return array(object(obj{"type": enumeration("user", "agent", "team", "workflow"), "id": uuid(), "task": str()}, "type", "id"))
	case "content":
		// Thread D3 message content. v1 defines exactly one block type, so the enum is the whole
		// vocabulary rather than an open string, and the 64 KiB bound is on the total text — a
		// property of the whole array, which OpenAPI cannot express per-property.
		return array(object(obj{"type": enumeration("text"), "text": str()}, "type", "text"))
	}
	return str()
}

func summary(r router.Route) string {
	if r.Action != "" {
		return strings.ReplaceAll(r.Action, "_", " ")
	}
	return r.Method + " " + r.Path
}

func description(r router.Route) string {
	switch r.Path {
	case "/api/v1/me/spaces":
		return "Lists every active collaboration space whose tenant has an active membership for the verified user. A space corresponds to exactly one tenant; clients follow all pages before presenting the switcher."
	case "/api/v1/me/git-identity":
		switch r.Method {
		case "PUT":
			return "States the git commit identity the verified user's Agent runs commit as. name is 1..200 characters with no line break or angle bracket, otherwise 400 invalid_git_name; email is at most 254 bytes shaped local@domain with no whitespace or angle bracket, otherwise 400 invalid_git_email. Only the shape is checked: the identity is a commit signature, never an authentication identity, and grants nothing. version is optimistic concurrency over the stated identity: 0 creates it, the current version replaces it, anything else is 409 version_conflict. Only the user can set their own identity; there is no administrator path. A session resolves the trigger user's identity when it starts, so a change applies to sessions that start afterwards."
		case "DELETE":
			return "Restores the default git commit identity (display name and a per-user noreply address). A stated identity is removed only at its current version, otherwise 409 version_conflict; an identity that is already the default restores as a no-op whatever version is sent, which makes a retry safe without an Idempotency-Key."
		}
		return "Returns the git commit identity the verified user's Agent runs commit as: the stated one, or the default (display name and a per-user noreply address) with isDefault true and version 0."
	case "/api/v1/me/join-requests":
		return "Lists the verified user's pending and decided join applications without requiring prior tenant membership."
	case "/api/v1/join/invitations/redeem":
		return "Redeems a valid, unrevoked, seven-day single-use invitation after login. Atomically grants ordinary tenant membership; a second user cannot redeem the same link."
	case "/api/v1/join/requests":
		return "Submits an application from a valid, unrevoked, thirty-day reusable link after login. The applicant receives no tenant access before an administrator approves it."
	case "/api/v1/tenants/:tid/people":
		return "Tenant administrators search the fixed Tianzhou endpoint through Cloud. Machine credentials stay server-side; only employed people and limited directory fields are returned."
	case "/api/v1/tenants/:tid/members/huawei":
		return "Tenant administrators add a selected Huawei person by stable globalUserId. Cloud searches Tianzhou again and verifies current employment before creating or reactivating membership."
	}
	base := "Public requests require a gateway service credential plus a caller-bound user credential. Active tenant membership is checked before lookup. Shared projects expose safe runtime summaries; runtime content and use require the verified creator or current tenant administrator. Conflicting mutations additionally require an effective server-confirmed control session. "
	if r.Action != "" {
		base = "Controller requests require an independent controller service credential; holder, active database-time lease epoch and operation version are checked. "
	}
	switch r.Action {
	case "access":
		return "Checks final user, active tenant membership and tenant scope. Execute additionally requires current controller lease epoch, open admission, ready workspace and a fresh initialized Node. This lookup is not an execution reservation; use admissions."
	case "admit":
		return "Atomically reserves an active task/interaction ticket on the current Node under the same transaction lock as stop/delete. Requires current controller holder+epoch and caller-bound final-user token. Unknown/uncompleted tickets remain active; bound Node explicitly finishes them. Repeated ticket UUID with identical scope returns it while admission remains open."
	case "lease_acquire", "lease_renew", "lease_release":
		return "Controller subject is holderId. Global lease lasts 30 seconds using PostgreSQL clock_timestamp(); renew every 10 seconds. Expired acquisition increments epoch, active same-holder acquisition returns current lease. Release and renew require exact live holder+epoch."
	case "claim":
		return base + "Claims queued/due retry/any running operation; reclaiming with the same epoch increments the operation version and fences stale in-memory workers. Returns a full scoped recovery snapshot. Reconcile every existing effect with Substrate by stable ID before planning or advancing. No automatic prompt replay."
	case "plan":
		return base + "Only the effect kind appropriate to the current step is allowed. Scope is restricted to operation workspaces. Plan persists BEFORE dispatch; sandbox plan atomically increments generation and allocates a unique live instance. Old instance must be confirmed terminated. Same plan returns the same effect ID."
	case "effect_result":
		return base + "Reports/reconciles one scoped external effect. External ID cannot change; succeeded evidence is immutable. absent is allowed only for a planned effect. Sandbox success requires its preallocated instance ID and the nodeId its Node will present; termination requires terminated; Workspace data deletion requires removed. This endpoint trusts the authenticated controller's Substrate observation, not client-supplied status."
	case "advance":
		return base + "Derives the next step server-side. Requires current-epoch successful effects. Create goes sandbox, node, clone; start goes sandbox, node. quiesce requires all tickets finished and fresh exact-epoch idle proof from each live Node. The clone step requires the operation's latest clone execution (registered over gRPC) to have succeeded on the current Node; it records the baseline commit and commits Workspace Ready/admission with operation success, re-checking the fresh initialized current Node. start commits the same readiness at its node step. Workspace data deletion is planned and completed only after termination confirmation."
	case "defer":
		return base + "Preserves operation/effect/resource references and current step; sets blocked or retry_wait with bounded retry delay. Never reports cleanup success on timeout."
	case "node_register", "node_status", "node_idle", "node_finish":
		return "Kept for the Go simulator's Node: desktop Nodes hold no Cloud credential and are reported by their Controller over gRPC NodeReportService. Requires node service credential whose sub is a process UUID equal to the sandbox's ensured nodeId and whose workspaceId/sandboxId/generation match the current unterminated instance. Node identity cannot be replaced while live. Status/idle use Node version; ticket finish uses Ticket version and a completed replay is idempotent. initialized cannot regress. Idle is scoped to operationId and exact Workspace admissionEpoch; true requires no active tickets. false fails that quiesce operation with resource_in_use and restores original admission. Registration requires protocolVersion=1; Pod Running alone cannot make Ready."
	}
	if strings.Contains(r.Path, "/spaces") {
		switch {
		case strings.Contains(r.Path, "/projects"):
			base += "Project collection scoped to the tenant's sole collaboration space; active tenant membership is required. "
		case r.Method == "PATCH":
			base += "Name and description may change; tenant and space names update together. Slug is immutable. Requires tenant admin and a matching version. "
		default:
			base += "Every tenant has one collaboration space. Active tenant members can read it. "
		}
	}
	if strings.Contains(r.Path, "/workflows") {
		base += "Workflows are tenant-owned graph documents. `graph` is the authored document (nodes, edges, viewport, editor annotations, global variables), stored and returned whole — the editor is its only reader, so no field inside it is validated or indexed here. A live workflow name is unique per tenant; archiving one frees its name. "
	}
	if r.Path == "/api/v1/tenants" && r.Method == "POST" {
		base = "Public requests require a gateway service credential plus a caller-bound user credential. The verified identity authorizes self-service provisioning without prior membership. Atomically creates a tenant and its sole visible collaboration space with the same name and the given globally unique, immutable slug; the caller becomes its first administrator. The idempotency key is matched per user across tenants and recorded under the new tenant. "
	}
	if strings.Contains(r.Path, "/clones") {
		base += "Unscoped clone submission is retired in production and returns 410 runtime_scope_required; existing requests remain readable by their original submitter. An explicit development store can exercise the legacy coordination fixture without enabling a production bypass. requestId is the caller's durable request identity: repeating it with the same repository and branch returns the original request, a different input is 409 idempotency_conflict. repository must be an https or ssh URL the Controller can clone; branch is a short branch name, never HEAD. executionId and nodeId are null until a dispatch is recorded; a pending state means awaiting reconciliation, never failure. "
	}
	if strings.HasSuffix(r.Path, "/thread/messages") {
		// The Thread POST's own fault vocabulary. It is stated here rather than added to the shared
		// per-status descriptions, which every unrelated route also carries.
		base += "Appends one user turn to an agent run's Thread and returns the entry that was created, with the Cloud-generated turnId the Node will echo back. Authorized like a comment: any active tenant member who can read the Issue. content accepts text blocks only and their total text must not exceed 64 KiB, otherwise 400 content_too_large; an unrecognized block type is 400 invalid_field_type, never a silent drop. Accepted while the Thread is pending, active or idle and no cancellation has been requested; a Thread that is ending or ended, and a run whose cancellation request is already recorded, both answer 409 thread_closed — a turn accepted after a cancellation would be persisted and never executed. The entry and the delivery command for it commit together with the idempotency record, so a failure anywhere in that transaction leaves nothing behind and the same key may be retried as a first request. A missing or mismatched tenant, Issue or run is 404 not_found. "
	}
	if strings.HasSuffix(r.Path, "/thread/end") {
		// The user-initiated end's own fault vocabulary, stated here rather than in the shared
		// per-status descriptions that unrelated routes also carry.
		base += "Ends an agent run's Thread at the user's request, which is what makes the Thread's upper lifecycle reachable from the UI rather than only from an expired idle window or a cancellation. The body is an empty JSON object and any field is 400 unknown_field. Accepted while the Thread is pending, active or idle, answering 202 with the Thread's new state ending; a Thread already ending or ended answers 409 thread_closed. The transition and the EndSession command it releases commit together with the idempotency record, so a failure anywhere in that transaction leaves nothing behind and the same key may be retried as a first request. This endpoint never advances the Thread to ended, never marks a queued turn discarded and never touches the run's phase, status, result or Workspace: ending a Thread asks the session to stop and the session's own terminal state decides what follows. Authorized like a comment: any active tenant member who can read the Issue; a missing or mismatched tenant, Issue or run, a soft-deleted run, a run that is not an agent run, and a run whose session has not been declared yet are all 404 not_found. "
	}
	if strings.HasSuffix(r.Path, "/thread") {
		// The Thread GET's own fault vocabulary and cursor semantics, stated here rather than in the
		// shared per-status descriptions that unrelated routes also carry.
		base += "Reads one ascending-by-seq window of an agent run's Thread together with the run's threadState and idleSince, all taken from one database snapshot, so the entries and the state are never mixed across instants. after and before are exclusive decimal seq cursors in opposite directions and are mutually exclusive: after=0 reads from the first entry, before=N returns the entries closest to N from below, and sending both is 400 invalid_pagination. Sending neither reads the tail — the newest limit entries — which is the window a panel opening on a live conversation wants. The response is always ascending by seq whichever cursor was used, and it reports the window's own nextCursor and prevCursor so a client pages in both directions by feeding each back as after and before. limit defaults to 200 and must not exceed 500; a limit outside 1..500 is 400 invalid_pagination and a cursor that is not a non-negative decimal integer is 400 invalid_cursor. Windows are chosen by seq range, never by OFFSET, so a concurrent append neither shifts a page nor duplicates an entry; entries are append-only and seq is gapless, so a client that re-reads with after set to its highest seen seq never misses or repeats an entry. There is no cross-request snapshot guarantee. This read changes nothing: it allocates no seq, writes no state and publishes no event. Authorized like a comment: any active tenant member who can read the Issue; a missing or mismatched tenant, Issue or run, a soft-deleted run, a run that is not an agent run, and a run whose session has not been declared yet are all 404 not_found. "
	}
	if strings.Contains(r.Path, "members") && !strings.Contains(r.Path, "/spaces") {
		if r.Method == "GET" {
			base += "Active tenant members may read the roster. "
		} else {
			base += "Administrator only. Existing memberships require matching version; new and disabled memberships must enter through a fresh directory check, invitation redemption or approved application. Last effective administrator cannot be disabled/demoted, including concurrent changes. "
		}
	}
	if strings.Contains(r.Path, "resource-status") || strings.Contains(r.Path, "administrative-stop") {
		base += "Administrator response explicitly excludes repository URL, worktree details, credentials, execution output and operation request/result/error details. Administrative stop still requires idle evidence. "
	}
	if strings.Contains(r.Path, "operations") {
		base += "Active tenant members may inspect project operations; administrative-stop remains administrator-only with a restricted projection. Retry only accepts blocked/retry_wait, exact operation version, and an idempotency key. "
	}
	if r.Method == "PATCH" && !strings.Contains(r.Path, "/spaces") {
		base += "Only project name may change; version must match. "
	}
	if (r.Method == "DELETE" || strings.HasSuffix(r.Path, "/stop")) && !strings.Contains(r.Path, "/spaces") {
		base += "Requires matching resource version and no active project operation. Atomically closes new execution admission. Active tickets return 409 resource_in_use without changing admission. Unknown Node activity requires later proof and remains pending/blocked. main Workspace cannot be independently deleted. "
	}
	if strings.HasSuffix(r.Path, "/projects") && r.Method == "POST" {
		base += "Creates Project/main Workspace/operation atomically in the tenant's sole collaboration space. repositoryUrl allows HTTPS or SSH with no password/query/fragment. defaultBranch is required and must name a branch, not HEAD (Cloud never reads the remote repository); credentialRefId must belong to tenant and owner. Sandbox, Node and clone initialization is asynchronous. "
	}
	if strings.HasSuffix(r.Path, "/workspaces") && r.Method == "POST" {
		base += "Creates one isolated Workspace and Task display identity. title/baseRef required; baseRef becomes the Workspace's requestedRef, which its Node clones; HEAD means the Project's defaultBranch. "
	}
	pagination := "Lists use ascending UUID pagination."
	if r.Path == "/api/v1/me/tenants" {
		// The member's tenant list orders by creation so items[0] is the earliest
		// tenant deterministically; the cursor stays an exclusive tenant UUID.
		pagination = "Lists page in ascending creation order; the after cursor is an exclusive tenant UUID."
	}
	return base + "Mutation version conflicts return 409; a missing required version returns 428. Unknown fields are rejected. " + pagination
}

func errorDescription(code string) string {
	switch code {
	case "400":
		return "Invalid JSON/field/input, missing idempotency key, invalid pagination or evidence"
	case "401":
		return "Invalid, forged, expired, wrong-audience, untrusted, or caller-mismatched credential"
	case "403":
		return "Disabled user, inactive/missing membership, wrong service role, or admin required"
	case "404":
		return "Resource absent or outside authorized tenant/owner scope"
	case "409":
		return "Version/idempotency conflict, resource_in_use, closed admission, stale epoch/Node/sandbox, incomplete effect, invalid transition, unconfirmed termination/idle, last_admin, space_last_owner, space_slug_conflict, or default_space_protected"
	case "428":
		return "Version precondition required"
	case "503":
		return "External capability not wired in this deployment (a port is Unavailable), e.g. form_descriptor_unavailable or assist_unavailable"
	default:
		return "Internal error; no SQL or secret details are exposed"
	}
}
