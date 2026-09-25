// Package contract builds the explicit OpenAPI contract for the implemented route allowlist.
package contract

import (
	"strings"

	"github.com/wanglongan587/cloud/internal/api/router"
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
	s["SpaceMember"] = resource("workspaceId userId role status version createdBy joinedAt", "createdBy")
	s["SpaceMemberListItem"] = resource("id workspaceId userId role status version displayName joinedAt", "")
	properties(s, "SpaceMember")["role"] = enumeration("owner", "admin", "member")
	s["SpaceEvent"] = object(obj{"type": enumeration("space.updated", "space.member_updated", "project.created", "project.updated", "project.archived", "space.plugins_updated", "plugins.catalog_updated"), "spaceId": uuid(), "projectId": optional(uuid()), "version": number()}, "type", "spaceId")
	s["Project"] = resource("id tenantId ownerUserId spaceId name repositoryUrl defaultBranch credentialRefId lifecycle version createdAt deletedAt", "spaceId credentialRefId deletedAt")
	s["Workspace"] = resource("id tenantId ownerUserId projectId kind desiredState observedState runtimeGeneration version admissionOpen admissionEpoch createdAt deletedAt", "deletedAt")
	s["WorkspaceListItem"] = resource("id tenantId ownerUserId projectId kind desiredState observedState runtimeGeneration version admissionOpen admissionEpoch createdAt deletedAt branchName baseCommitId title", "deletedAt baseCommitId title")
	s["Comment"] = resource("id tenantId issueId authorUserId authorType authorId parentId body seq version createdAt updatedAt deletedAt", "authorUserId authorId parentId deletedAt")
	commentProps := properties(s, "Comment")
	commentProps["seq"] = number()
	s["Label"] = resource("id tenantId name color version createdAt updatedAt deletedAt", "deletedAt")
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
	runProps["executionContextRef"] = optional(uuid())
	runProps["workflowInvocationRef"] = optional(uuid())
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
	spacePluginProps["desiredState"] = enumeration("installed", "removed")
	spacePluginProps["observedState"] = enumeration("pending", "installing", "installed", "failed", "removing", "removed")
	spacePluginProps["observedVersion"] = optional(str())
	spacePluginProps["installError"] = optional(str())
	s["SpacePluginList"] = object(obj{"items": array(ref("SpacePlugin"))}, "items")
	s["PluginUniversalRelease"] = object(obj{"url": str(), "sha256": str()}, "url", "sha256")
	s["PluginReleaseTarget"] = object(obj{"target": str(), "url": str(), "sha256": str()}, "target", "url", "sha256")
	s["PluginLogoCandidate"] = object(obj{"role": enumeration("universal", "light", "dark"), "extension": enumeration("svg", "png", "webp", "jpg", "jpeg")}, "role", "extension")
	s["OperationRequest"] = object(obj{"previous": obj{"type": "object", "additionalProperties": ref("Workspace")}, "pluginId": str(), "version": str()})
	s["OperationResult"] = object(obj{"resourceId": uuid()})
	s["Operation"] = resource("id tenantId actorUserId projectId workspaceId kind state step request result errorCode idempotencyKey requestHash controllerEpoch retryAt version createdAt updatedAt", "workspaceId errorCode controllerEpoch retryAt")
	opProps := properties(s, "Operation")
	opProps["request"] = ref("OperationRequest")
	opProps["result"] = ref("OperationResult")
	opProps["state"] = enumeration("queued", "running", "retry_wait", "blocked", "succeeded", "failed")
	opProps["step"] = enumeration("storage", "worktree", "sandbox", "node", "ready", "quiesce", "terminate", "cleanup", "storage_delete", "plugin", "done")
	for _, name := range []string{"Workspace", "WorkspaceListItem", "AdminResource"} {
		p := properties(s, name)
		p["kind"] = enumeration("main", "isolated")
		p["desiredState"] = enumeration("running", "stopped", "deleted")
		p["observedState"] = enumeration("provisioning", "starting", "ready", "stopping", "stopped", "unavailable", "deleting", "deleted")
	}
	s["Lease"] = resource("name holderId epoch expiresAt", "")
	properties(s, "Lease")["holderId"] = str()
	properties(s, "Lease")["epoch"] = number()
	s["Storage"] = resource("projectId substrateStorageId storageProfile layoutVersion observedState version", "substrateStorageId")
	properties(s, "Storage")["substrateStorageId"] = optional(str())
	s["Sandbox"] = resource("id workspaceId generation substrateSandboxId observedState createdAt terminatedAt version", "substrateSandboxId terminatedAt")
	properties(s, "Sandbox")["substrateSandboxId"] = optional(str())
	s["Node"] = resource("id sandboxInstanceId serviceSubject connectionState protocolVersion initialized lastSeenAt endedAt idleAdmissionEpoch version workspaceId", "endedAt idleAdmissionEpoch")
	s["Ticket"] = resource("id tenantId workspaceId nodeInstanceId actorUserId admissionEpoch kind state createdAt finishedAt version", "finishedAt")
	s["EffectRequest"] = object(obj{"kind": enumeration("storage_ensure", "worktree_ensure", "sandbox_ensure", "sandbox_terminate", "worktree_delete", "storage_delete", "plugin_ensure", "plugin_delete"), "projectId": uuid(), "workspaceId": uuid(), "repositoryUrl": str(), "requestedRef": str(), "sandboxInstanceId": uuid(), "pluginId": str(), "version": str(), "universal": ref("PluginUniversalRelease"), "targets": array(ref("PluginReleaseTarget"))}, "kind", "projectId")
	s["EffectResult"] = object(obj{"layoutVersion": number(), "commitId": obj{"type": "string", "pattern": "^([0-9a-f]{40}|[0-9a-f]{64})$"}, "jobTerminated": boolean(), "removed": boolean(), "terminated": boolean(), "installed": boolean(), "sandboxInstanceId": uuid(), "nodeId": uuid(), "version": str(), "error": str(), "diagnostic": str()})
	s["Effect"] = resource("id operationId projectId workspaceId kind state externalId request result reconciledEpoch createdAt version", "workspaceId externalId")
	ep := properties(s, "Effect")
	ep["externalId"] = optional(str())
	ep["request"] = ref("EffectRequest")
	ep["result"] = ref("EffectResult")
	s["ControllerProject"] = resource("id tenantId ownerUserId spaceId name repositoryUrl defaultBranch credentialRefId lifecycle version createdAt deletedAt secretRef", "spaceId credentialRefId deletedAt secretRef")
	s["ControllerWorkspace"] = resource("id tenantId ownerUserId projectId kind desiredState observedState runtimeGeneration version admissionOpen admissionEpoch createdAt deletedAt relativePath branchName requestedRef baseCommitId", "deletedAt baseCommitId")
	for _, name := range []string{"WorkspaceListItem", "ControllerWorkspace"} {
		properties(s, name)["baseCommitId"] = optional(obj{"type": "string", "pattern": "^([0-9a-f]{40}|[0-9a-f]{64})$"})
	}
	s["Snapshot"] = object(obj{"operation": ref("Operation"), "project": ref("ControllerProject"), "storage": ref("Storage"), "workspaces": array(ref("ControllerWorkspace")), "sandboxes": array(ref("Sandbox")), "nodes": array(ref("Node")), "effects": array(ref("Effect"))}, "operation", "project", "storage", "workspaces", "sandboxes", "nodes", "effects")
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
		if public && (r.Method == "POST" || r.Method == "DELETE") {
			parameters = append(parameters, obj{"name": "Idempotency-Key", "in": "header", "required": true, "schema": obj{"type": "string", "minLength": 1, "maxLength": 200}, "description": "Scoped to tenant and user. Same key and canonical method/path/body returns the original response before version validation; changed request is 409."})
		}
		if isList(r) {
			parameters = append(parameters, obj{"name": "limit", "in": "query", "schema": obj{"type": "integer", "minimum": 1, "maximum": 100, "default": 50}}, obj{"name": "after", "in": "query", "schema": uuid(), "description": "Exclusive UUID cursor, ascending stable ordering."})
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
	paths["/healthz"] = obj{"get": obj{"operationId": "health", "tags": []string{"health"}, "summary": "PostgreSQL readiness", "responses": obj{"200": obj{"description": "Database reachable", "content": obj{"application/json": obj{"schema": object(obj{"status": enumeration("ok")}, "status")}}}, "503": obj{"description": "Database unavailable", "content": obj{"application/json": obj{"schema": ref("Error")}}}}}}
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
	return r.Method == "GET" && (strings.HasSuffix(r.Path, "/tenants") || strings.HasSuffix(r.Path, "/members") || strings.HasSuffix(r.Path, "/projects") || strings.HasSuffix(r.Path, "/workspaces") || strings.HasSuffix(r.Path, "/spaces") || strings.HasSuffix(r.Path, "/resource-status") || strings.HasSuffix(r.Path, "/issue-statuses") || strings.HasSuffix(r.Path, "/labels") || strings.HasSuffix(r.Path, "/issue-views") || strings.HasSuffix(r.Path, "/comments") || strings.HasSuffix(r.Path, "/subscribers") || strings.HasSuffix(r.Path, "/clones"))
}

func responseSchema(r router.Route) (schema obj, status string) {
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
		case strings.Contains(r.Path, "/members"):
			if r.Method == "GET" {
				return object(obj{"items": array(ref("SpaceMemberListItem")), "nextCursor": str()}, "items", "nextCursor"), "200"
			}
			return ref("SpaceMember"), "200"
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
	case strings.Contains(r.Path, "/labels"):
		if r.Method == "GET" {
			return object(obj{"items": array(ref("Label")), "nextCursor": str()}, "items", "nextCursor"), "200"
		}
		if r.Method == "POST" {
			return object(obj{"resource": ref("Label")}, "resource"), "200"
		}
		return ref("Label"), "200"
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
	case "description", "category", "color", "icon", "filter", "position", "parentId", "input", "targets", "contextRefs":
		return true
	case "pluginVersion":
		// Omitted install pins the catalog's current version.
		return true
	case "values":
		// Confirm must state what it is confirming; assist may be asked with a still-empty form.
		return strings.HasSuffix(r.Path, "/assist")
	}
	return name == "defaultBranch" || name == "credentialRefId" || name == "version" && r.Method == "PUT" || name == "epoch" && r.Action == "access" || name == "workspaceId" && r.Action == "plan" || name == "externalId" && r.Action == "effect_result"
}

func inputSchema(name string, r router.Route) obj {
	switch name {
	case "version", "epoch", "admissionEpoch":
		return obj{"type": "integer", "format": "int64", "minimum": 0}
	case "retrySeconds":
		return obj{"type": "integer", "minimum": 1, "maximum": 3600}
	case "protocolVersion":
		return obj{"type": "integer", "enum": []int{1}}
	case "initialized", "idle":
		return boolean()
	case "result":
		return ref("EffectResult")
	case "action":
		return enumeration("read", "execute")
	case "role":
		if strings.Contains(r.Path, "/spaces/") {
			return enumeration("owner", "admin", "member")
		}
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
		return enumeration("storage_ensure", "worktree_ensure", "sandbox_ensure", "sandbox_terminate", "worktree_delete", "storage_delete")
	case "errorCode":
		return enumeration("substrate_timeout", "termination_unconfirmed", "git_cleanup_failed", "node_unavailable", "external_failure")
	case "tenantId", "operationId", "ticketId", "credentialRefId":
		return uuid()
	case "category":
		return enumeration("unstarted", "started", "done", "closed")
	case "labelId", "userId", "assigneeId", "executorId", "refId", "parentId", "projectRef", "targetId":
		return uuid()
	case "ids":
		return array(uuid())
	case "filter", "properties", "input", "values":
		return obj{"type": "object", "additionalProperties": true}
	case "contextRefs":
		return array(ref("ContextRefRef"))
	case "position":
		return number()
	case "slug":
		return obj{"type": "string", "pattern": "^[a-z0-9][a-z0-9-]{0,63}$", "description": "Lowercase, immutable, unique per tenant."}
	case "workspaceId":
		if r.Action == "plan" {
			return str()
		}
		return uuid()
	case "targets":
		return array(object(obj{"type": enumeration("user", "agent", "team", "workflow"), "id": uuid(), "task": str()}, "type", "id"))
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
	base := "Public requests require a gateway service credential plus a caller-bound user credential. Tenant membership is checked before lookup; resource reads filter tenant in SQL, and space-scoped projects and their runtime workspaces additionally require active membership of that workspace, while unscoped projects stay owner-scoped. "
	if r.Action != "" {
		base = "Controller requests require an independent controller service credential; holder, active database-time lease epoch and operation version are checked. "
	}
	switch r.Action {
	case "access":
		return "Checks final user, active membership, tenant and owner. read checks ownership; execute additionally requires current controller lease epoch, open admission, ready workspace and a fresh initialized Node. This lookup is not an execution reservation; use admissions."
	case "admit":
		return "Atomically reserves an active task/interaction ticket on the current Node under the same transaction lock as stop/delete. Requires current controller holder+epoch and caller-bound final-user token. Unknown/uncompleted tickets remain active; bound Node explicitly finishes them. Repeated ticket UUID with identical scope returns it while admission remains open."
	case "lease_acquire", "lease_renew", "lease_release":
		return "Controller subject is holderId. Global lease lasts 30 seconds using PostgreSQL clock_timestamp(); renew every 10 seconds. Expired acquisition increments epoch, active same-holder acquisition returns current lease. Release and renew require exact live holder+epoch."
	case "claim":
		return base + "Claims queued/due retry/any running operation; reclaiming with the same epoch increments the operation version and fences stale in-memory workers. Returns a full scoped recovery snapshot. Reconcile every existing effect with Substrate by stable ID before planning or advancing. No automatic prompt replay."
	case "plan":
		return base + "Only the effect kind appropriate to the current step is allowed. Scope is restricted to operation workspaces. Plan persists BEFORE dispatch; sandbox plan atomically increments generation and allocates a unique live instance. Old instance must be confirmed terminated. Same plan returns the same effect ID."
	case "effect_result":
		return base + "Reports/reconciles one scoped external effect. External ID cannot change; succeeded evidence is immutable. absent is allowed only for a planned effect. Worktree success requires real commitId and jobTerminated; cleanup requires removed and jobTerminated; termination requires terminated; storage requires layoutVersion=1; sandbox requires its preallocated instance ID. This endpoint trusts the authenticated controller's Substrate observation, not client-supplied status."
	case "advance":
		return base + "Derives the next step server-side. Requires current-epoch successful effects. quiesce requires all tickets finished and fresh exact-epoch idle proof from each live Node. node step atomically commits worktree readiness, Workspace Ready/admission, and operation success after fresh initialized current Node. Cleanup and storage deletion cannot complete before termination confirmation."
	case "defer":
		return base + "Preserves operation/effect/resource references and current step; sets blocked or retry_wait with bounded retry delay. Never reports cleanup success on timeout."
	case "node_register", "node_status", "node_idle", "node_finish":
		return "Requires node service credential whose sub is a process UUID and whose workspaceId/sandboxId/generation match the current unterminated instance. Node identity cannot be replaced while live. Status/idle use Node version; ticket finish uses Ticket version and a completed replay is idempotent. initialized cannot regress. Idle is scoped to operationId and exact Workspace admissionEpoch; true requires no active tickets. false fails that quiesce operation with resource_in_use and restores original admission. Registration requires protocolVersion=1; Pod Running alone cannot make Ready."
	}
	if strings.Contains(r.Path, "/spaces") {
		switch {
		case strings.Contains(r.Path, "/members") && r.Method == "POST":
			base += "Adds an already-registered user to the space as a plain member by email, resolved in the caller's identity source. Admin or owner only. The target is atomically ensured tenant membership (existing role kept) and thereby gains access to the Projects and Runtime Workspaces shared in that workspace. Unknown or inactive email is 404 user_not_registered; adding an existing member returns the current membership unchanged. "
		case strings.Contains(r.Path, "/members") && r.Method == "PUT":
			base += "Updates a member's role (admin/member) or status. Role management is owner-only — admins add members through POST, they cannot change roles. The owner role is immutable: granting owner or any write touching an owner row is 409 ownership_transfer_not_supported (ownership transfer is not implemented). The target user must be an active member of the same tenant; a matching version is required. "
		case strings.Contains(r.Path, "/members") && r.Method == "DELETE":
			base += "Removes a member's workspace membership (hard delete); owner only, admins and members cannot remove anyone. The user account, tenant membership and their resources are untouched and remain in the workspace; the removed member's access to the workspace, its projects and runtime workspaces is revoked. An owner row can never be removed, including self-removal (409 cannot_remove_workspace_owner). Requires a matching version and an idempotency key. "
		case strings.Contains(r.Path, "/members"):
			base += "Lists the space's members; any active member of the space can read the member list. "
		case r.Method == "POST" && strings.HasSuffix(r.Path, "/spaces"):
			base += "Creates the collaboration space and its first owner atomically. slug is lowercase, immutable and unique per tenant. "
		case strings.Contains(r.Path, "/projects"):
			base += "Project collection scoped to one collaboration space; membership is required, and project visibility follows workspace membership — any active member of the space can see every active project in it. Deleting a project requires its creator or a space owner/admin. New projects created at the tenant level default into the tenant's default space. "
		case r.Method == "PATCH":
			base += "Only name and description may change; slug is immutable. Requires admin or owner and a matching version. "
		case r.Method == "DELETE":
			base += "Archives the space (soft delete); requires owner and a matching version. The default space cannot be archived. Projects are unaffected. "
		default:
			base += "Only joined members can read a space. "
		}
	}
	if r.Path == "/api/v1/tenants" && r.Method == "POST" {
		base = "Public requests require a gateway service credential plus a caller-bound user credential. No tenant membership is required: the verified identity alone authorizes provisioning. Atomically creates a tenant named after the space, makes the caller its first administrator, creates the space with the given slug and makes the caller its owner. The tenant is an implicit container the product never shows. The idempotency key is matched per user across tenants and recorded under the created tenant. "
	}
	if strings.Contains(r.Path, "/clones") {
		base += "Clone requests are independent accepted work items outside the project/workspace operation model: Cloud accepts them in its own transaction, a Controller claims and dispatches them over the internal control contract, and only the submitting user can read them. requestId is the caller's durable request identity: repeating it with the same repository and branch returns the original request, a different input is 409 idempotency_conflict. repository must be an https or ssh URL the Controller can clone; branch is a short branch name, never HEAD. executionId and nodeId are null until a dispatch is recorded; a pending state means awaiting reconciliation, never failure. "
	}
	if strings.Contains(r.Path, "members") && !strings.Contains(r.Path, "/spaces") {
		base += "Administrator only. Updating an existing membership requires matching version; new membership uses version=0. Last effective administrator cannot be disabled/demoted, including concurrent changes. "
	}
	if strings.Contains(r.Path, "resource-status") || strings.Contains(r.Path, "administrative-stop") {
		base += "Administrator response explicitly excludes repository URL, worktree details, credentials, execution output and operation request/result/error details. Administrative stop still requires idle evidence. "
	}
	if strings.Contains(r.Path, "operations") {
		base += "Operation lookup follows project owner; administrative-stop actor receives only the restricted projection. Retry only accepts blocked/retry_wait, exact operation version, and an idempotency key. "
	}
	if r.Method == "PATCH" && !strings.Contains(r.Path, "/spaces") {
		base += "Only project name may change; version must match. "
	}
	if (r.Method == "DELETE" || strings.HasSuffix(r.Path, "/stop")) && !strings.Contains(r.Path, "/spaces") {
		base += "Requires matching resource version and no active project operation. Atomically closes new execution admission. Active tickets return 409 resource_in_use without changing admission. Unknown Node activity requires later proof and remains pending/blocked. main Workspace cannot be independently deleted. "
	}
	if strings.HasSuffix(r.Path, "/projects") && r.Method == "POST" {
		base += "Creates Project/storage/main Workspace/operation atomically. repositoryUrl allows HTTPS or SSH with no password/query/fragment. defaultBranch defaults to HEAD; credentialRefId must belong to tenant and owner. Storage/worktree/sandbox initialization is asynchronous. A project created at the tenant level defaults into the tenant's default collaboration space; the schema keeps space_id nullable for pre-existing unscoped projects, which stay owner-only. "
	}
	if strings.HasSuffix(r.Path, "/workspaces") && r.Method == "POST" {
		base += "Creates one isolated Workspace and Task display identity. title/baseRef required; branch and relative path are server-generated. "
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
