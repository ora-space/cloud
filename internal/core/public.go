package core

import (
	"context"
	"net/url"
	"strconv"
	"strings"
)

func itoa(n int) string { return strconv.Itoa(n) }

// PublicRequest is populated only after service and final-user credentials are verified.
type PublicRequest struct {
	Method, Path, TenantID, ProjectID, WorkspaceID, OperationID, UserID, Key, After string
	Limit                                                                           int
	Body                                                                            Object
	Identity                                                                        *Claims
}

// Public executes one authorized public request in a short database transaction.
func (s *Store) Public(ctx context.Context, r *PublicRequest) (Object, int, error) {
	status := 200
	result, e := s.transact(ctx, func(t *transaction) Object {
		u := identity(t, r.Identity.Source, r.Identity.Subject, r.Identity.DisplayName)
		uid := u.S("id")
		if r.Path == "/api/v1/me" {
			return u
		}
		if r.Path == "/api/v1/me/tenants" {
			return page(t, "SELECT t.id,t.name,t.status,m.role FROM tenants t JOIN tenant_memberships m ON m.tenant_id=t.id WHERE m.user_id=$1 AND m.status='active' AND t.status='active' AND t.deleted_at IS NULL", []any{uid}, "t.id", r)
		}
		isAdmin := strings.HasSuffix(r.Path, "/resource-status") || strings.HasSuffix(r.Path, "/administrative-stop") || (r.UserID != "" && r.Method == "PUT") || strings.HasSuffix(r.Path, "/members")
		membership(t, r.TenantID, uid, isAdmin)
		if r.Method == "GET" {
			return readPublic(t, r, uid)
		}
		hash := requestHash(r.Method, r.Path, r.Body)
		idempotent := r.Method == "POST" || r.Method == "DELETE"
		if idempotent {
			require(r.Key != "" && len(r.Key) <= 200, 400, "idempotency_key_required")
			old := t.one("SELECT * FROM idempotency_records WHERE tenant_id=$1 AND user_id=$2 AND key=$3", r.TenantID, uid, r.Key)
			if old != nil {
				require(old.S("requestHash") == hash, 409, "idempotency_conflict")
				status = int(old.N("status"))
				return old.O("response")
			}
		}
		var out Object
		switch {
		case r.UserID != "" && r.Method == "PUT":
			out = putMember(t, r, uid)
		case r.ProjectID == "" && r.WorkspaceID == "" && r.OperationID == "" && strings.HasSuffix(r.Path, "/projects") && r.Method == "POST":
			out = createProject(t, r, uid, hash)
			status = 202
		case r.OperationID != "":
			out = retryOperation(t, r, uid)
			status = 202
		case r.WorkspaceID != "":
			out = workspaceAction(t, r, uid, hash, isAdmin)
			status = 202
		case r.ProjectID != "":
			p := project(t, r.TenantID, uid, r.ProjectID)
			switch {
			case strings.HasSuffix(r.Path, "/workspaces"):
				out = createWorkspace(t, r, p, uid, hash)
				status = 202
			case r.Method == "PATCH":
				version(p, r.Body.N("version"))
				name := validText(r.Body.S("name"), 200)
				require(p.S("lifecycle") != "deleting", 409, "resource_unavailable")
				t.exec("UPDATE projects SET name=$2,version=version+1 WHERE id=$1", p.S("id"), name)
				out = project(t, r.TenantID, uid, p.S("id"))
			default:
				require(r.Method == "DELETE", 405, "method_not_allowed")
				version(p, r.Body.N("version"))
				idleProject(t, p.S("id"))
				require(p.S("lifecycle") == "active", 409, "resource_unavailable")
				ws := t.list("SELECT * FROM workspaces WHERE project_id=$1 AND deleted_at IS NULL ORDER BY id", p.S("id"))
				previous := Object{}
				for _, w := range ws {
					checkActivities(t, w)
					previous[w.S("id")] = w
				}
				for _, w := range ws {
					closeAdmission(t, w, "deleted")
				}
				t.exec("UPDATE projects SET lifecycle='deleting',version=version+1 WHERE id=$1", p.S("id"))
				req := Object{"previous": previous}
				op := newOperation(t, r, uid, p.S("id"), "", "delete_project", "quiesce", hash, req)
				out = Object{"resource": project(t, r.TenantID, uid, p.S("id")), "operation": op}
				status = 202
			}
		default:
			reject(404, "not_found")
		}
		if idempotent {
			t.exec("INSERT INTO idempotency_records(tenant_id,user_id,key,request_hash,response,status) VALUES($1,$2,$3,$4,$5,$6)", r.TenantID, uid, r.Key, hash, jsonText(out), status)
		}
		return out
	})
	return result, status, e
}

func page(t *transaction, q string, args []any, col string, r *PublicRequest) Object {
	limit := r.Limit
	if limit == 0 {
		limit = 50
	}
	require(limit > 0 && limit <= 100, 400, "invalid_pagination")
	if r.After != "" {
		require(validID(r.After), 400, "invalid_cursor")
		args = append(args, r.After)
		q += " AND " + col + " > $" + itoa(len(args)) + "::uuid"
	}
	args = append(args, limit+1)
	q += " ORDER BY " + col + " LIMIT $" + itoa(len(args))
	items := t.list(q, args...)
	next := ""
	if len(items) > limit {
		items = items[:limit]
		next = items[len(items)-1].S("id")
	}
	return Object{"items": items, "nextCursor": next}
}

func readPublic(t *transaction, r *PublicRequest, uid string) Object {
	switch {
	case strings.HasSuffix(r.Path, "/members"):
		return page(t, "SELECT m.user_id AS id,m.tenant_id,m.user_id,m.role,m.status,m.version,u.display_name FROM tenant_memberships m JOIN users u ON u.id=m.user_id WHERE m.tenant_id=$1", []any{r.TenantID}, "m.user_id", r)
	case strings.HasSuffix(r.Path, "/resource-status"):
		return page(t, "SELECT w.id,w.project_id,w.owner_user_id,w.kind,w.desired_state,w.observed_state,w.runtime_generation,w.version FROM workspaces w WHERE w.tenant_id=$1 AND w.deleted_at IS NULL", []any{r.TenantID}, "w.id", r)
	case r.OperationID != "":
		return ownedOperation(t, r, uid)
	case r.WorkspaceID != "":
		return workspace(t, r.TenantID, uid, r.WorkspaceID, false)
	case r.ProjectID != "":
		p := project(t, r.TenantID, uid, r.ProjectID)
		if strings.HasSuffix(r.Path, "/workspaces") {
			return page(t, "SELECT w.*,wt.branch_name,wt.base_commit_id,task.title FROM workspaces w JOIN workspace_worktrees wt ON wt.workspace_id=w.id LEFT JOIN tasks task ON task.workspace_id=w.id WHERE w.project_id=$1 AND w.tenant_id=$2 AND w.owner_user_id=$3 AND w.deleted_at IS NULL", []any{p.S("id"), r.TenantID, uid}, "w.id", r)
		}
		return p
	default:
		return page(t, "SELECT * FROM projects WHERE tenant_id=$1 AND owner_user_id=$2 AND deleted_at IS NULL", []any{r.TenantID, uid}, "id", r)
	}
}

func putMember(t *transaction, r *PublicRequest, uid string) Object {
	require(validID(r.UserID), 400, "invalid_user")
	require(t.one("SELECT id FROM users WHERE id=$1 AND status='active' AND deleted_at IS NULL", r.UserID) != nil, 404, "not_found")
	role, status := r.Body.S("role"), r.Body.S("status")
	require((role == "admin" || role == "member") && (status == "active" || status == "disabled"), 400, "invalid_member")
	old := t.one("SELECT * FROM tenant_memberships WHERE tenant_id=$1 AND user_id=$2", r.TenantID, r.UserID)
	if old != nil {
		version(old, r.Body.N("version"))
		if old.S("role") == "admin" && old.S("status") == "active" && (role != "admin" || status != "active") {
			require(t.one("SELECT m.user_id FROM tenant_memberships m JOIN users u ON u.id=m.user_id WHERE m.tenant_id=$1 AND m.user_id<>$2 AND m.role='admin' AND m.status='active' AND u.status='active' AND u.deleted_at IS NULL", r.TenantID, r.UserID) != nil, 409, "last_admin")
		}
		t.exec("UPDATE tenant_memberships SET role=$3,status=$4,version=version+1 WHERE tenant_id=$1 AND user_id=$2", r.TenantID, r.UserID, role, status)
	} else {
		require(r.Body.N("version") == 0, 409, "version_conflict")
		t.exec("INSERT INTO tenant_memberships(tenant_id,user_id,role,status) VALUES($1,$2,$3,$4)", r.TenantID, r.UserID, role, status)
	}
	return t.one("SELECT * FROM tenant_memberships WHERE tenant_id=$1 AND user_id=$2", r.TenantID, r.UserID)
}

func validText(s string, maximum int) string {
	s = strings.TrimSpace(s)
	require(s != "" && len(s) <= maximum, 400, "invalid_input")
	return s
}

func validRef(s string) string {
	s = validText(s, 200)
	require(!strings.HasPrefix(s, "-") && !strings.ContainsAny(s, "\x00\r\n ~^:?*[\\") && !strings.Contains(s, "..") && !strings.Contains(s, "@{"), 400, "invalid_ref")
	return s
}

func createProject(t *transaction, r *PublicRequest, uid, hash string) Object {
	name := validText(r.Body.S("name"), 200)
	repo := validText(r.Body.S("repositoryUrl"), 2048)
	parsed, e := url.Parse(repo)
	require(e == nil && (parsed.Scheme == "https" || parsed.Scheme == "ssh") && parsed.Host != "" && parsed.RawQuery == "" && parsed.Fragment == "", 400, "invalid_repository_url")
	if parsed.User != nil {
		_, password := parsed.User.Password()
		require(!password && parsed.Scheme == "ssh", 400, "embedded_credentials_forbidden")
	}
	branch := r.Body.S("defaultBranch")
	if branch == "" {
		branch = "HEAD"
	}
	branch = validRef(branch)
	var cred any
	if id := r.Body.S("credentialRefId"); id != "" {
		require(validID(id), 400, "invalid_credential_ref")
		require(t.one("SELECT id FROM credential_refs WHERE id=$1 AND tenant_id=$2 AND owner_user_id=$3 AND purpose='git' AND deleted_at IS NULL", id, r.TenantID, uid) != nil, 404, "credential_ref_not_found")
		cred = id
	}
	pid, wid := newID(), newID()
	t.exec("INSERT INTO projects(id,tenant_id,owner_user_id,name,repository_url,default_branch,credential_ref_id,lifecycle) VALUES($1,$2,$3,$4,$5,$6,$7,'provisioning')", pid, r.TenantID, uid, name, repo, branch, cred)
	t.exec("INSERT INTO project_storage(project_id,observed_state) VALUES($1,'pending')", pid)
	insertWorkspace(t, r.TenantID, uid, pid, wid, "main", branch, "")
	op := newOperation(t, r, uid, pid, wid, "create_project", "storage", hash, Object{})
	return Object{"resource": project(t, r.TenantID, uid, pid), "workspace": workspace(t, r.TenantID, uid, wid, false), "operation": op}
}

func insertWorkspace(t *transaction, tid, uid, pid, wid, kind, ref, title string) {
	t.exec("INSERT INTO workspaces(id,tenant_id,owner_user_id,project_id,kind,desired_state,observed_state) VALUES($1,$2,$3,$4,$5,'running','provisioning')", wid, tid, uid, pid, kind)
	t.exec("INSERT INTO workspace_worktrees(workspace_id,relative_path,branch_name,requested_ref,provisioning_state) VALUES($1,$2,$3,$4,'pending')", wid, "workspaces/"+wid+"/checkout", "ora/"+wid, ref)
	if kind == "isolated" {
		t.exec("INSERT INTO tasks(id,workspace_id,title) VALUES($1,$2,$3)", newID(), wid, title)
	}
}

func createWorkspace(t *transaction, r *PublicRequest, p Object, uid, hash string) Object {
	require(p.S("lifecycle") == "active", 409, "resource_unavailable")
	idleProject(t, p.S("id"))
	title := validText(r.Body.S("title"), 200)
	ref := validRef(r.Body.S("baseRef"))
	wid := newID()
	insertWorkspace(t, r.TenantID, uid, p.S("id"), wid, "isolated", ref, title)
	op := newOperation(t, r, uid, p.S("id"), wid, "create_workspace", "worktree", hash, Object{})
	return Object{"resource": workspace(t, r.TenantID, uid, wid, false), "operation": op}
}

func newOperation(t *transaction, r *PublicRequest, uid, pid, wid, kind, step, hash string, req Object) Object {
	id := newID()
	var w any
	if wid != "" {
		w = wid
	}
	t.exec("INSERT INTO operations(id,tenant_id,actor_user_id,project_id,workspace_id,kind,state,step,request,idempotency_key,request_hash) VALUES($1,$2,$3,$4,$5,$6,'queued',$7,$8,$9,$10)", id, r.TenantID, uid, pid, w, kind, step, jsonText(req), r.Key, hash)
	return t.one("SELECT * FROM operations WHERE id=$1", id)
}

func checkActivities(t *transaction, w Object) {
	require(t.one("SELECT id FROM execution_tickets WHERE workspace_id=$1 AND state='active'", w.S("id")) == nil, 409, "resource_in_use")
}

func closeAdmission(t *transaction, w Object, desired string) {
	state := "stopping"
	if desired == "deleted" {
		state = "deleting"
	}
	t.exec("UPDATE workspaces SET admission_open=false,admission_epoch=admission_epoch+1,desired_state=$2,observed_state=$3,version=version+1 WHERE id=$1", w.S("id"), desired, state)
}

func adminResource(w Object) Object {
	o := Object{}
	for _, k := range []string{"id", "projectId", "ownerUserId", "kind", "desiredState", "observedState", "runtimeGeneration", "version"} {
		o[k] = w[k]
	}
	return o
}

func adminOperation(o Object) Object {
	out := Object{}
	for _, k := range []string{"id", "tenantId", "projectId", "workspaceId", "kind", "state", "step", "version", "createdAt", "updatedAt"} {
		out[k] = o[k]
	}
	return out
}

func workspaceAction(t *transaction, r *PublicRequest, uid, hash string, admin bool) Object {
	w := workspace(t, r.TenantID, uid, r.WorkspaceID, admin)
	p := t.one("SELECT * FROM projects WHERE id=$1", w.S("projectId"))
	require(p.S("lifecycle") == "active", 409, "resource_unavailable")
	version(w, r.Body.N("version"))
	idleProject(t, p.S("id"))
	kind, step := "stop", "quiesce"
	req := Object{"previous": Object{w.S("id"): w}}
	if strings.HasSuffix(r.Path, "/start") {
		kind, step = "start", "sandbox"
		require(w.S("desiredState") == "stopped" && w.S("observedState") == "stopped", 409, "resource_unavailable")
		t.exec("UPDATE workspaces SET desired_state='running',observed_state='starting',version=version+1 WHERE id=$1", w.S("id"))
	} else {
		checkActivities(t, w)
		require(w.S("observedState") == "ready" || w.S("observedState") == "stopped" || w.S("observedState") == "unavailable", 409, "resource_unavailable")
		desired := "stopped"
		if r.Method == "DELETE" {
			require(w.S("kind") == "isolated", 409, "main_workspace_required")
			kind, desired = "delete_workspace", "deleted"
		}
		if admin {
			kind = "administrative_stop"
		}
		closeAdmission(t, w, desired)
	}
	op := newOperation(t, r, uid, p.S("id"), w.S("id"), kind, step, hash, req)
	resource := workspace(t, r.TenantID, uid, w.S("id"), admin)
	if admin {
		resource = adminResource(resource)
		op = adminOperation(op)
	}
	return Object{"resource": resource, "operation": op}
}

func ownedOperation(t *transaction, r *PublicRequest, uid string) Object {
	require(validID(r.OperationID), 404, "not_found")
	o := t.one("SELECT o.* FROM operations o JOIN projects p ON p.id=o.project_id WHERE o.id=$1 AND o.tenant_id=$2 AND (p.owner_user_id=$3 OR (o.kind='administrative_stop' AND o.actor_user_id=$3))", r.OperationID, r.TenantID, uid)
	require(o != nil, 404, "not_found")
	if o.S("kind") == "administrative_stop" {
		membership(t, r.TenantID, uid, true)
		return adminOperation(o)
	}
	return o
}

func retryOperation(t *transaction, r *PublicRequest, uid string) Object {
	o := ownedOperation(t, r, uid)
	version(o, r.Body.N("version"))
	require(o.S("state") == "blocked" || o.S("state") == "retry_wait", 409, "operation_not_retryable")
	t.exec("UPDATE operations SET state='queued',retry_at=NULL,error_code=NULL,version=version+1,updated_at=now() WHERE id=$1", r.OperationID)
	return Object{"operation": ownedOperation(t, r, uid)}
}
