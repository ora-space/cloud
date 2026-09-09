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
func optional(s obj) obj               { s["nullable"] = true; return s }
func object(properties obj, required ...string) obj {
	return obj{"type": "object", "properties": properties, "required": required, "additionalProperties": false}
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
	s["Member"] = resource("tenantId userId role status version createdAt", "")
	s["MemberListItem"] = resource("id tenantId userId role status version displayName", "")
	s["Project"] = resource("id tenantId ownerUserId name repositoryUrl defaultBranch credentialRefId lifecycle version createdAt deletedAt", "credentialRefId deletedAt")
	s["Workspace"] = resource("id tenantId ownerUserId projectId kind desiredState observedState runtimeGeneration version admissionOpen admissionEpoch createdAt deletedAt", "deletedAt")
	s["WorkspaceListItem"] = resource("id tenantId ownerUserId projectId kind desiredState observedState runtimeGeneration version admissionOpen admissionEpoch createdAt deletedAt branchName baseCommitId title", "deletedAt baseCommitId title")
	s["AdminResource"] = resource("id projectId ownerUserId kind desiredState observedState runtimeGeneration version", "")
	s["AdminOperation"] = resource("id tenantId projectId workspaceId kind state step version createdAt updatedAt", "workspaceId")
	s["OperationRequest"] = object(obj{"previous": obj{"type": "object", "additionalProperties": ref("Workspace")}})
	s["OperationResult"] = object(obj{"resourceId": uuid()})
	s["Operation"] = resource("id tenantId actorUserId projectId workspaceId kind state step request result errorCode idempotencyKey requestHash controllerEpoch retryAt version createdAt updatedAt", "workspaceId errorCode controllerEpoch retryAt")
	opProps := properties(s, "Operation")
	opProps["request"] = ref("OperationRequest")
	opProps["result"] = ref("OperationResult")
	opProps["state"] = enumeration("queued", "running", "retry_wait", "blocked", "succeeded", "failed")
	opProps["step"] = enumeration("storage", "worktree", "sandbox", "node", "ready", "quiesce", "terminate", "cleanup", "storage_delete", "done")
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
	s["EffectRequest"] = object(obj{"kind": enumeration("storage_ensure", "worktree_ensure", "sandbox_ensure", "sandbox_terminate", "worktree_delete", "storage_delete"), "projectId": uuid(), "workspaceId": uuid(), "repositoryUrl": str(), "requestedRef": str(), "sandboxInstanceId": uuid()}, "kind", "projectId")
	s["EffectResult"] = object(obj{"layoutVersion": number(), "commitId": obj{"type": "string", "pattern": "^([0-9a-f]{40}|[0-9a-f]{64})$"}, "jobTerminated": boolean(), "removed": boolean(), "terminated": boolean(), "sandboxInstanceId": uuid(), "nodeId": uuid()})
	s["Effect"] = resource("id operationId projectId workspaceId kind state externalId request result reconciledEpoch createdAt version", "workspaceId externalId")
	ep := properties(s, "Effect")
	ep["externalId"] = optional(str())
	ep["request"] = ref("EffectRequest")
	ep["result"] = ref("EffectResult")
	s["ControllerProject"] = resource("id tenantId ownerUserId name repositoryUrl defaultBranch credentialRefId lifecycle version createdAt deletedAt secretRef", "credentialRefId deletedAt secretRef")
	s["ControllerWorkspace"] = resource("id tenantId ownerUserId projectId kind desiredState observedState runtimeGeneration version admissionOpen admissionEpoch createdAt deletedAt relativePath branchName requestedRef baseCommitId", "deletedAt baseCommitId")
	for _, name := range []string{"WorkspaceListItem", "ControllerWorkspace"} {
		properties(s, name)["baseCommitId"] = optional(obj{"type": "string", "pattern": "^([0-9a-f]{40}|[0-9a-f]{64})$"})
	}
	s["Snapshot"] = object(obj{"operation": ref("Operation"), "project": ref("ControllerProject"), "storage": ref("Storage"), "workspaces": array(ref("ControllerWorkspace")), "sandboxes": array(ref("Sandbox")), "nodes": array(ref("Node")), "effects": array(ref("Effect"))}, "operation", "project", "storage", "workspaces", "sandboxes", "nodes", "effects")
	s["EmptyClaim"] = object(obj{"operation": obj{"type": "object", "nullable": true, "enum": []any{nil}}}, "operation")
	s["Access"] = object(obj{"userId": uuid(), "tenantId": uuid(), "workspaceId": uuid(), "allowedAction": enumeration("read", "execute"), "executable": boolean(), "runtimeGeneration": number()}, "userId", "tenantId", "workspaceId", "allowedAction", "executable", "runtimeGeneration")
	s["IdleRefusal"] = object(obj{"accepted": boolean(), "errorCode": enumeration("resource_in_use")}, "accepted", "errorCode")
	paths := obj{}
	for _, r := range router.Routes() {
		path := r.Path
		parameters := []any{}
		for _, p := range strings.Split(path, "/") {
			if strings.HasPrefix(p, ":") {
				name := p[1:]
				path = strings.ReplaceAll(path, p, "{"+name+"}")
				parameters = append(parameters, obj{"name": name, "in": "path", "required": true, "schema": uuid()})
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
		for _, code := range []string{"400", "401", "403", "404", "409", "428", "500"} {
			responses[code] = obj{"description": errorDescription(code), "content": obj{"application/json": obj{"schema": ref("Error")}}}
		}
		operation := obj{"operationId": strings.ToLower(r.Method) + strings.NewReplacer("/", "_", ":", "").Replace(r.Path), "summary": summary(r), "description": description, "security": security, "responses": responses}
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
	paths["/healthz"] = obj{"get": obj{"operationId": "health", "summary": "PostgreSQL readiness", "responses": obj{"200": obj{"description": "Database reachable", "content": obj{"application/json": obj{"schema": object(obj{"status": enumeration("ok")}, "status")}}}, "503": obj{"description": "Database unavailable", "content": obj{"application/json": obj{"schema": ref("Error")}}}}}}
	return obj{"openapi": "3.0.3", "info": obj{"title": "Ora Cloud phase one", "version": "1.0.0", "description": "Authoritative PostgreSQL core. Simulation is separate; no production Controller/Node/Kubernetes implementation is implied."}, "servers": []any{obj{"url": "http://localhost:8080"}}, "paths": paths, "components": obj{"schemas": s, "securitySchemes": obj{"serviceCredential": obj{"type": "http", "scheme": "bearer", "bearerFormat": "EdDSA JWT", "description": "Pinned issuer/kid/kind=service/role, aud=ora-cloud, exp and iat required, <=5 minute lifetime. Public API requires gateway; internal control requires controller; nodes require scoped node role."}, "userCredential": obj{"type": "apiKey", "in": "header", "name": "X-Ora-User-Token", "description": "Separately signed EdDSA JWT: kind=user, source+sub, caller must equal authenticated service sub, aud=ora-cloud. User and membership status checked in PostgreSQL."}}}}
}

func isList(r router.Route) bool {
	return r.Method == "GET" && (strings.HasSuffix(r.Path, "/tenants") || strings.HasSuffix(r.Path, "/members") || strings.HasSuffix(r.Path, "/projects") || strings.HasSuffix(r.Path, "/workspaces") || strings.HasSuffix(r.Path, "/resource-status"))
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
		return enumeration("admin", "member")
	case "status":
		return enumeration("active", "disabled")
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
	case "workspaceId":
		if r.Action == "plan" {
			return str()
		}
		return uuid()
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
	base := "Public requests require a gateway service credential plus a caller-bound user credential. Tenant membership is checked before lookup; resource reads filter tenant and owner in SQL. "
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
	if strings.Contains(r.Path, "members") {
		base += "Administrator only. Updating an existing membership requires matching version; new membership uses version=0. Last effective administrator cannot be disabled/demoted, including concurrent changes. "
	}
	if strings.Contains(r.Path, "resource-status") || strings.Contains(r.Path, "administrative-stop") {
		base += "Administrator response explicitly excludes repository URL, worktree details, credentials, execution output and operation request/result/error details. Administrative stop still requires idle evidence. "
	}
	if strings.Contains(r.Path, "operations") {
		base += "Operation lookup follows project owner; administrative-stop actor receives only the restricted projection. Retry only accepts blocked/retry_wait, exact operation version, and an idempotency key. "
	}
	if r.Method == "PATCH" {
		base += "Only project name may change; version must match. "
	}
	if r.Method == "DELETE" || strings.HasSuffix(r.Path, "/stop") {
		base += "Requires matching resource version and no active project operation. Atomically closes new execution admission. Active tickets return 409 resource_in_use without changing admission. Unknown Node activity requires later proof and remains pending/blocked. main Workspace cannot be independently deleted. "
	}
	if strings.HasSuffix(r.Path, "/projects") && r.Method == "POST" {
		base += "Creates Project/storage/main Workspace/operation atomically. repositoryUrl allows HTTPS or SSH with no password/query/fragment. defaultBranch defaults to HEAD; credentialRefId must belong to tenant and owner. Storage/worktree/sandbox initialization is asynchronous. "
	}
	if strings.HasSuffix(r.Path, "/workspaces") && r.Method == "POST" {
		base += "Creates one isolated Workspace and Task display identity. title/baseRef required; branch and relative path are server-generated. "
	}
	return base + "Mutation version conflicts return 409; a missing required version returns 428. Unknown fields are rejected. Lists use ascending UUID pagination."
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
		return "Version/idempotency conflict, resource_in_use, closed admission, stale epoch/Node/sandbox, incomplete effect, invalid transition, unconfirmed termination/idle, or last_admin"
	case "428":
		return "Version precondition required"
	default:
		return "Internal error; no SQL or secret details are exposed"
	}
}
