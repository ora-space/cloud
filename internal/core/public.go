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
	Method, Path, TenantID, ProjectID, WorkspaceID, SpaceID, OperationID, CloneID, UserID, IssueID, CommentID, LabelID, StatusID, ViewID, RunID, ContextRefID, InteractionID, WorkflowID, SnapshotID, FormRef, InvitationID, JoinLinkID, JoinRequestID, Key, After, Before, Query, GroupBy string
	// FormIssueID is the optional `issueId` query parameter of the form-descriptor route. It is
	// deliberately its own field rather than a fallback into IssueID: IssueID drives dispatch in both
	// readPublic and Public, so a query parameter folded into it would let `GET /issues?issueId=…`
	// hijack routing.
	FormIssueID string
	// ModelConnectionID addresses an owner-scoped personal connection outside tenant routing.
	ModelConnectionID string
	Limit             int
	Body              Object
	Identity          *Claims
	Person            *DirectoryPerson
}

// Public executes one authorized public request in a short database transaction.
// Committed collaboration-space mutations are broadcast to live space subscribers
// after the transaction succeeds, never before.
func (s *Store) Public(ctx context.Context, r *PublicRequest) (Object, int, error) {
	status := 200
	var dispatches []dispatchTarget
	var events []SpaceEvent
	var accepted []string
	result, e := s.transact(ctx, func(t *transaction) Object {
		u := identityWithAlias(t, r.Identity)
		uid := u.S("id")
		if r.Path == "/api/v1/me" {
			return u
		}
		if strings.HasPrefix(r.Path, "/api/v1/me/model-connections") || r.Path == "/api/v1/me/model-default" {
			var out Object
			out, status = personalModelsPublic(t, r, uid)
			return out
		}
		if r.Path == "/api/v1/me/git-identity" {
			switch r.Method {
			case "PUT":
				return putGitIdentity(t, r, uid)
			case "DELETE":
				return deleteGitIdentity(t, r, uid)
			}
			return gitIdentityView(t, uid)
		}
		if r.Path == "/api/v1/me/tenants" {
			// Preserve the historical creation-order contract for tenant clients.
			// The visible switcher uses /me/spaces and loads every page.
			return pageByCreation(t, "SELECT t.id,t.name,t.status,m.role FROM tenants t JOIN tenant_memberships m ON m.tenant_id=t.id WHERE m.user_id=$1 AND m.status='active' AND t.status='active' AND t.deleted_at IS NULL", []any{uid}, r)
		}
		if r.Path == "/api/v1/me/spaces" {
			return page(t, "SELECT w.*,m.role FROM collab_workspaces w JOIN tenant_memberships m ON m.tenant_id=w.tenant_id JOIN tenants tn ON tn.id=w.tenant_id WHERE m.user_id=$1 AND m.status='active' AND tn.status='active' AND tn.deleted_at IS NULL AND w.archived_at IS NULL", []any{uid}, "w.id", r)
		}
		if r.Path == "/api/v1/me/join-requests" || strings.HasPrefix(r.Path, "/api/v1/join/") {
			var out Object
			var publishMemberEvent bool
			out, status, publishMemberEvent = joinBeforeMembership(t, r, uid)
			if publishMemberEvent {
				events = append(events, SpaceEvent{Type: "space.member_updated", SpaceID: soleSpace(t, out.S("tenantId")).S("id")})
			}
			return out
		}
		if r.Path == "/api/v1/tenants" {
			// Tenant provisioning is the one mutation with no tenant scope to
			// check membership against: the caller's verified identity is the
			// whole authorization.
			var out Object
			out, status = createTenant(t, r, uid)
			return out
		}
		isAdmin := r.SpaceID == "" && (strings.HasSuffix(r.Path, "/resource-status") || strings.HasSuffix(r.Path, "/force-stop") || (r.UserID != "" && r.Method == "PUT") || strings.Contains(r.Path, "/invitations") || strings.Contains(r.Path, "/join-links") || strings.Contains(r.Path, "/join-requests") || strings.HasSuffix(r.Path, "/members/huawei"))
		membership(t, r.TenantID, uid, isAdmin)
		refreshRuntimeControls(t)
		if r.Method == "GET" {
			if strings.HasSuffix(r.Path, "/force-stop") {
				return forceStopPublic(t, r, uid)
			}
			return readPublic(t, r, uid)
		}
		authorizeRuntimeReplay(t, r, uid)
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
		case strings.Contains(r.Path, "/invitations") || strings.Contains(r.Path, "/join-links") || strings.Contains(r.Path, "/join-requests"):
			out, status = adminJoinWrite(t, r, uid)
			if r.JoinRequestID != "" && strings.HasSuffix(r.Path, "/approve") {
				events = append(events, SpaceEvent{Type: "space.member_updated", SpaceID: soleSpace(t, r.TenantID).S("id")})
			}
		case strings.HasSuffix(r.Path, "/members/huawei"):
			out = addHuaweiMember(t, r, uid)
			events = append(events, SpaceEvent{Type: "space.member_updated", SpaceID: soleSpace(t, r.TenantID).S("id")})
		case r.SpaceID != "" && strings.HasSuffix(r.Path, "/plugins") && r.Method == "POST":
			out = installSpacePlugin(t, r, uid)
			events = append(events, SpaceEvent{Type: "space.plugins_updated", SpaceID: r.SpaceID})
		case r.SpaceID != "" && strings.HasSuffix(r.Path, "/plugins") && r.Method == "DELETE":
			out = removeSpacePlugin(t, r, uid)
			events = append(events, SpaceEvent{Type: "space.plugins_updated", SpaceID: r.SpaceID})
		case r.SpaceID != "" && r.Method == "PATCH":
			out = patchSpace(t, r, uid)
			events = append(events, SpaceEvent{Type: "space.updated", SpaceID: r.SpaceID, Version: out.N("version")})
		case r.SpaceID != "" && strings.HasSuffix(r.Path, "/projects") && r.Method == "POST":
			out = createProject(t, r, uid, hash)
			status = 202
			events = append(events, SpaceEvent{Type: "project.created", SpaceID: r.SpaceID, ProjectID: out.O("resource").S("id")})
		case r.UserID != "" && r.Method == "PUT":
			out = putMember(t, r, uid)
			if w := soleSpace(t, r.TenantID); w != nil {
				events = append(events, SpaceEvent{Type: "space.member_updated", SpaceID: w.S("id")})
			}
		case r.ProjectID == "" && r.WorkspaceID == "" && r.OperationID == "" && strings.HasSuffix(r.Path, "/projects") && r.Method == "POST":
			out = createProject(t, r, uid, hash)
			status = 202
			events = append(events, SpaceEvent{Type: "project.created", SpaceID: out.O("resource").S("spaceId"), ProjectID: out.O("resource").S("id")})
		case strings.HasSuffix(r.Path, "/clones"):
			// Clone requests are accepted work items outside the operation model; they share the
			// public idempotency scope but never create an operation or effect.
			var queued string
			out, queued = clonesPublic(t, r, uid)
			status = 202
			if queued != "" {
				accepted = append(accepted, queued)
			}
		case r.OperationID != "":
			out = retryOperation(t, r, uid)
			status = 202
		case r.WorkspaceID != "" && strings.HasSuffix(r.Path, "/force-stop"):
			out = forceStopPublic(t, r, uid)
			status = 202
		case r.WorkspaceID != "" && strings.Contains(r.Path, "/control/"):
			out = runtimeControlPublic(t, r, uid)
		case r.WorkspaceID != "":
			out = workspaceAction(t, r, uid, hash, isAdmin)
			status = 202
		case r.ProjectID != "":
			p, m := projectInSpace(t, r.TenantID, uid, r.ProjectID)
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
				if p.S("spaceId") != "" {
					events = append(events, SpaceEvent{Type: "project.updated", SpaceID: p.S("spaceId"), ProjectID: p.S("id"), Version: out.N("version")})
				}
			default:
				require(r.Method == "DELETE", 405, "method_not_allowed")
				requireSpaceRole(m, "admin")
				version(p, r.Body.N("version"))
				idleProject(t, p.S("id"))
				require(p.S("lifecycle") == "active", 409, "resource_unavailable")
				ws := t.list("SELECT * FROM workspaces WHERE project_id=$1 AND deleted_at IS NULL ORDER BY id", p.S("id"))
				previous := Object{}
				for _, w := range ws {
					require(runtimeControl(t, w.S("id")).S("state") == "idle", 409, "runtime_control_held")
					checkActivities(t, w)
					previous[w.S("id")] = w
				}
				for _, w := range ws {
					closeAdmission(t, w, "deleted")
				}
				t.exec("UPDATE projects SET lifecycle='deleting',version=version+1 WHERE id=$1", p.S("id"))
				req := Object{"previous": previous}
				op := newOperation(t, r, uid, p.S("id"), "", "delete_project", "quiesce", hash, req)
				for _, w := range ws {
					reserveRuntimeMaintenance(t, w.S("id"), op.S("id"))
				}
				out = Object{"resource": t.one("SELECT * FROM projects WHERE id=$1", p.S("id")), "operation": op}
				status = 202
				if p.S("spaceId") != "" {
					events = append(events, SpaceEvent{Type: "project.archived", SpaceID: p.S("spaceId"), ProjectID: p.S("id")})
				}
			}
		case r.IssueID != "" || strings.HasSuffix(r.Path, "/issues"):
			switch {
			case strings.HasSuffix(r.Path, "/thread/messages"):
				// Thread D3: appending a user turn. Matched before the /runs arm because the path
				// contains "/runs/" too, and the thread sub-resource is a different write.
				require(r.Method == "POST", 404, "not_found")
				out = Object{"resource": appendThreadMessage(t, s, r, uid)}
				status = 201
			case strings.HasSuffix(r.Path, "/thread/end"):
				// Thread D4 (G-018): the user ending the conversation. Also matched before the /runs
				// arm, and answered 202 because the request is accepted for asynchronous shutdown —
				// the Thread is `ending`, not `ended`. The response is the Thread state itself rather
				// than a wrapped resource: the endpoint creates no new resource to point at.
				require(r.Method == "POST", 404, "not_found")
				out = endThreadByUser(t, s, r)
				status = 202
			case strings.Contains(r.Path, "/runs"):
				switch r.Method {
				case "POST":
					out = Object{"resource": createRun(t, r, uid)}
				default:
					reject(404, "not_found")
				}
			case strings.Contains(r.Path, "/context-refs"):
				switch r.Method {
				case "POST":
					out = Object{"resource": createContextRef(t, r)}
				case "DELETE":
					out = deleteContextRef(t, r)
				default:
					reject(404, "not_found")
				}
			case strings.Contains(r.Path, "/collaboration/assist"):
				require(r.Method == "POST", 404, "not_found")
				out = assistWorkflow(t, r)
			case strings.Contains(r.Path, "/interactions"):
				require(r.Method == "POST" && strings.HasSuffix(r.Path, "/confirm"), 404, "not_found")
				out = confirmInteraction(t, r, uid, &dispatches)
			case strings.Contains(r.Path, "/comments"):
				switch r.Method {
				case "POST":
					out = Object{"resource": createComment(t, r, uid, &dispatches)}
				case "PUT":
					out = updateComment(t, r)
				case "DELETE":
					out = deleteComment(t, r)
				default:
					reject(404, "not_found")
				}
			case strings.Contains(r.Path, "/subscribers"):
				switch r.Method {
				case "POST":
					out = Object{"resource": subscribe(t, r)}
				case "DELETE":
					out = Object{"resource": unsubscribe(t, r)}
				default:
					reject(404, "not_found")
				}
			case strings.Contains(r.Path, "/labels"):
				switch r.Method {
				case "POST":
					out = Object{"resource": attachLabel(t, r)}
				case "DELETE":
					out = detachLabel(t, r)
				default:
					reject(404, "not_found")
				}
			case r.Method == "POST" && strings.HasSuffix(r.Path, "/move"):
				out = moveIssue(t, r)
			case r.Method == "POST":
				out = Object{"resource": createIssue(t, r, uid)}
			case r.Method == "PUT":
				out = updateIssue(t, r)
			case r.Method == "DELETE":
				out = deleteIssue(t, r)
			default:
				reject(404, "not_found")
			}
		case strings.Contains(r.Path, "/issue-statuses"):
			switch r.Method {
			case "POST":
				out = Object{"resource": createIssueStatus(t, r)}
			case "PUT":
				out = updateIssueStatus(t, r)
			case "DELETE":
				out = deleteIssueStatus(t, r)
			default:
				reject(404, "not_found")
			}
		case strings.Contains(r.Path, "/issue-views"):
			switch r.Method {
			case "POST":
				out = Object{"resource": createView(t, r, uid)}
			case "PUT":
				out = updateView(t, r, uid)
			case "DELETE":
				out = deleteView(t, r, uid)
			default:
				reject(404, "not_found")
			}
		case r.Method == "POST" && strings.HasSuffix(r.Path, "/issues/batch"):
			out = batchUpdate(t, r)
		case strings.Contains(r.Path, "/labels"):
			switch r.Method {
			case "POST":
				out = Object{"resource": createLabel(t, r)}
			case "PUT":
				out = updateLabel(t, r)
			case "DELETE":
				out = deleteLabel(t, r)
			default:
				reject(404, "not_found")
			}
		case strings.Contains(r.Path, "/workflows"):
			switch {
			case strings.HasSuffix(r.Path, "/runs"):
				out = Object{"resource": createWorkflowRun(t, r)}
			case strings.HasSuffix(r.Path, "/publish"):
				out = Object{"resource": publishWorkflow(t, r)}
			case strings.HasSuffix(r.Path, "/restore"):
				out = restoreWorkflowSnapshot(t, r)
			default:
				switch r.Method {
				case "POST":
					out = Object{"resource": createWorkflow(t, r)}
				case "PUT":
					out = updateWorkflow(t, r)
				case "DELETE":
					out = deleteWorkflow(t, r)
				default:
					reject(404, "not_found")
				}
			}
		default:
			reject(404, "not_found")
		}
		if idempotent {
			t.exec("INSERT INTO idempotency_records(tenant_id,user_id,key,request_hash,response,status) VALUES($1,$2,$3,$4,$5,$6)", r.TenantID, uid, r.Key, hash, jsonText(out), status)
		}
		return out
	})
	if e == nil {
		for _, d := range dispatches {
			// Task Mode runs are dispatched after the comment transaction commits (the advisory
			// lock must not span external/observer writes). Best-effort: a failure leaves the run
			// queued, which is the correct "Unavailable" degradation.
			_ = s.dispatchRun(ctx, d.tenantID, d.runID)
		}
		if s.Events != nil {
			for _, ev := range events {
				s.Events.Publish(ev)
			}
		}
		for _, id := range accepted {
			s.signalWork(id)
		}
	}
	return result, status, e
}

func page(t *transaction, q string, args []any, col string, r *PublicRequest) Object {
	if r.After != "" {
		require(validID(r.After), 400, "invalid_cursor")
		args = append(args, r.After)
		q += " AND " + col + " > $" + itoa(len(args)) + "::uuid"
	}
	return window(t, q+" ORDER BY "+col, args, r)
}

// pageByCreation pages a member-facing list in ascending creation order with the
// row UUID as the tiebreaker. Unlike page the ordering column is not unique, so
// the cursor predicate resolves the anchor row's created_at from the tenants
// table instead of comparing the UUID alone; the public cursor therefore stays
// an opaque tenant UUID. An anchor row that no longer exists compares as NULL,
// which ends the walk with an empty page rather than skipping or repeating rows.
func pageByCreation(t *transaction, q string, args []any, r *PublicRequest) Object {
	if r.After != "" {
		require(validID(r.After), 400, "invalid_cursor")
		args = append(args, r.After)
		q += " AND (t.created_at, t.id) > ((SELECT a.created_at FROM tenants a WHERE a.id=$" + itoa(len(args)) + "::uuid), $" + itoa(len(args)) + "::uuid)"
	}
	return window(t, q+" ORDER BY t.created_at, t.id", args, r)
}

// window applies the bounded limit, runs the query and reports the next cursor.
func window(t *transaction, q string, args []any, r *PublicRequest) Object {
	limit := r.Limit
	if limit == 0 {
		limit = 50
	}
	require(limit > 0 && limit <= 100, 400, "invalid_pagination")
	args = append(args, limit+1)
	q += " LIMIT $" + itoa(len(args))
	items := t.list(q, args...)
	next := ""
	if len(items) > limit {
		items = items[:limit]
		next = items[len(items)-1].S("id")
	}
	return Object{"items": items, "nextCursor": next}
}

func readPublic(t *transaction, r *PublicRequest, uid string) Object {
	if r.SpaceID != "" {
		switch {
		case strings.HasSuffix(r.Path, "/projects"):
			spaceMember(t, r.SpaceID, uid)
			// The tenant's sole space shares every live project with its active members.
			return page(t, "SELECT p.* FROM projects p WHERE p.space_id=$1 AND p.deleted_at IS NULL", []any{r.SpaceID}, "p.id", r)
		case strings.HasSuffix(r.Path, "/plugins/catalog"):
			// The catalog snapshot is shared by every space of the deployment; space
			// membership gates the read exactly like any other space-scoped list.
			spaceMember(t, r.SpaceID, uid)
			return pluginCatalog(t)
		case strings.HasSuffix(r.Path, "/plugins"):
			spaceMember(t, r.SpaceID, uid)
			return spacePluginList(t, r.SpaceID)
		default:
			spaceMember(t, r.SpaceID, uid)
			return t.one("SELECT * FROM collab_workspaces WHERE id=$1", r.SpaceID)
		}
	}
	switch {
	case strings.HasSuffix(r.Path, "/invitations") || strings.HasSuffix(r.Path, "/join-links") || strings.HasSuffix(r.Path, "/join-requests"):
		return adminJoinRead(t, r)
	case r.CloneID != "" || strings.HasSuffix(r.Path, "/clones"):
		out, _ := clonesPublic(t, r, uid)
		return out
	case strings.HasSuffix(r.Path, "/spaces"):
		return listSpaces(t, r, uid)
	case strings.HasSuffix(r.Path, "/members"):
		return page(t, "SELECT m.user_id AS id,m.tenant_id,m.user_id,m.role,m.status,m.version,u.display_name FROM tenant_memberships m JOIN users u ON u.id=m.user_id WHERE m.tenant_id=$1", []any{r.TenantID}, "m.user_id", r)
	case strings.HasSuffix(r.Path, "/thread"):
		// Thread D5: reading a run's Thread. Matched before the /runs arm below because the path
		// contains "/runs/" too and this is a different resource — the Thread, not the run. The POST
		// side is dispatched in Public before readPublic; this arm is the GET.
		return threadRead(t, r)
	case r.WorkflowID != "" && (r.RunID != "" || strings.HasSuffix(r.Path, "/runs")):
		// Workflow runs resolve here, before the generic issue-`/runs` case below: that case
		// matches any path containing "/runs", so .../workflows/:wfid/runs would otherwise be
		// swallowed with an empty IssueID and die a 404.
		if r.RunID != "" {
			return workflowRun(t, r.TenantID, r.WorkflowID, r.RunID)
		}
		return workflowRunList(t, r)
	case strings.Contains(r.Path, "/runs"):
		if r.RunID != "" {
			return run(t, r.TenantID, r.IssueID, r.RunID)
		}
		return runList(t, r)
	case strings.Contains(r.Path, "/context-refs"):
		return contextRefList(t, r)
	case strings.HasSuffix(r.Path, "/timeline"):
		return timelineList(t, r)
	case strings.Contains(r.Path, "/interactions"):
		return interactionList(t, r)
	case strings.HasSuffix(r.Path, "/collaboration/targets"):
		return collaborationTargetList(t, r)
	case strings.Contains(r.Path, "/collaboration/forms/"):
		return formDescriptorByRef(t, r)
	case strings.HasSuffix(r.Path, "/comments"):
		return commentList(t, r)
	case strings.HasSuffix(r.Path, "/subscribers"):
		return subscriberList(t, r)
	case strings.HasSuffix(r.Path, "/labels") && r.IssueID != "":
		return issueLabelList(t, r)
	case r.IssueID != "":
		return issue(t, r.TenantID, r.IssueID)
	case strings.HasSuffix(r.Path, "/issue-statuses"):
		return statusCatalogList(t, r)
	case strings.HasSuffix(r.Path, "/labels"):
		return labelList(t, r)
	case strings.HasSuffix(r.Path, "/issue-views"):
		return viewList(t, r, uid)
	case strings.HasSuffix(r.Path, "/issue-groups"):
		return issueGroups(t, r)
	case strings.HasSuffix(r.Path, "/issues"):
		return issueList(t, r)
	case strings.HasSuffix(r.Path, "/resource-status"):
		return page(t, "SELECT w.id,w.project_id,w.owner_user_id,w.kind,w.desired_state,w.observed_state,w.runtime_generation,w.version FROM workspaces w WHERE w.tenant_id=$1 AND w.deleted_at IS NULL AND w.issue_run_id IS NULL", []any{r.TenantID}, "w.id", r)
	case strings.Contains(r.Path, "/workflows"):
		if r.SnapshotID != "" {
			return workflowSnapshot(t, r.TenantID, r.WorkflowID, r.SnapshotID)
		}
		if r.WorkflowID == "" {
			return workflowList(t, r)
		}
		if strings.HasSuffix(r.Path, "/snapshots") {
			return workflowSnapshotList(t, r)
		}
		return workflow(t, r.TenantID, r.WorkflowID)
	case r.OperationID != "":
		return ownedOperation(t, r, uid)
	case r.WorkspaceID != "" && strings.HasSuffix(r.Path, "/control"):
		return runtimeControlPublic(t, r, uid)
	case r.WorkspaceID != "":
		return workspace(t, r.TenantID, uid, r.WorkspaceID, false)
	case r.ProjectID != "":
		p := project(t, r.TenantID, uid, r.ProjectID)
		if strings.HasSuffix(r.Path, "/workspaces") {
			return runtimeListing(t, r, uid, p.S("id"))
		}
		return p
	default:
		return page(t, "SELECT p.* FROM projects p JOIN collab_workspaces w ON w.id=p.space_id WHERE p.tenant_id=$1 AND p.deleted_at IS NULL AND w.archived_at IS NULL", []any{r.TenantID}, "p.id", r)
	}
}

func putMember(t *transaction, r *PublicRequest, uid string) Object {
	require(validID(r.UserID), 400, "invalid_user")
	require(t.one("SELECT id FROM users WHERE id=$1 AND status='active' AND deleted_at IS NULL", r.UserID) != nil, 404, "not_found")
	role, status := r.Body.S("role"), r.Body.S("status")
	require((role == "admin" || role == "member") && (status == "active" || status == "disabled"), 400, "invalid_member")
	old := t.one("SELECT * FROM tenant_memberships WHERE tenant_id=$1 AND user_id=$2", r.TenantID, r.UserID)
	require(old != nil, 404, "not_found")
	version(old, r.Body.N("version"))
	require(old.S("status") != "disabled" || status != "active", 409, "member_rejoin_required")
	if old.S("role") == "admin" && old.S("status") == "active" && (role != "admin" || status != "active") {
		require(t.one("SELECT m.user_id FROM tenant_memberships m JOIN users u ON u.id=m.user_id WHERE m.tenant_id=$1 AND m.user_id<>$2 AND m.role='admin' AND m.status='active' AND u.status='active' AND u.deleted_at IS NULL", r.TenantID, r.UserID) != nil, 409, "last_admin")
	}
	t.exec("UPDATE tenant_memberships SET role=$3,status=$4,version=version+1 WHERE tenant_id=$1 AND user_id=$2", r.TenantID, r.UserID, role, status)
	refreshRuntimeControls(t)
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
	// Both paths resolve the tenant's sole collaboration space.
	spaceID := r.SpaceID
	if spaceID == "" {
		spaceID = soleSpace(t, r.TenantID).S("id")
	} else {
		spaceMember(t, spaceID, uid)
	}
	name := validText(r.Body.S("name"), 200)
	repo := validText(r.Body.S("repositoryUrl"), 2048)
	parsed, e := url.Parse(repo)
	require(e == nil && (parsed.Scheme == "https" || parsed.Scheme == "ssh") && parsed.Host != "" && parsed.RawQuery == "" && parsed.Fragment == "", 400, "invalid_repository_url")
	if parsed.User != nil {
		_, password := parsed.User.Password()
		require(!password && parsed.Scheme == "ssh", 400, "embedded_credentials_forbidden")
	}
	// The Node clones only a literal branch name and Cloud reads no remote repository, so the
	// caller names the branch; HEAD would leave the Workspace's clone step blocked for good.
	branch := r.Body.S("defaultBranch")
	require(strings.TrimSpace(branch) != "", 400, "default_branch_required")
	branch = validRef(branch)
	require(branch != "HEAD", 400, "default_branch_required")
	var cred any
	if id := r.Body.S("credentialRefId"); id != "" {
		require(validID(id), 400, "invalid_credential_ref")
		require(t.one("SELECT id FROM credential_refs WHERE id=$1 AND tenant_id=$2 AND owner_user_id=$3 AND purpose='git' AND deleted_at IS NULL", id, r.TenantID, uid) != nil, 404, "credential_ref_not_found")
		cred = id
	}
	pid, wid := newID(), newID()
	t.exec("INSERT INTO projects(id,tenant_id,owner_user_id,space_id,name,repository_url,default_branch,credential_ref_id,lifecycle) VALUES($1,$2,$3,$4,$5,$6,$7,$8,'provisioning')", pid, r.TenantID, uid, spaceID, name, repo, branch, cred)
	insertWorkspace(t, r.TenantID, uid, uid, pid, wid, "main", branch, "")
	op := newOperation(t, r, uid, pid, wid, "create_project", "sandbox", hash, Object{})
	return Object{"resource": t.one("SELECT * FROM projects WHERE id=$1", pid), "workspace": t.one("SELECT * FROM workspaces WHERE id=$1", wid), "operation": op}
}

func insertWorkspace(t *transaction, tid, owner, creator, pid, wid, kind, ref, title string) {
	// The Workspace's Node clones ref into the Workspace's own data; there is no shared Project
	// repository or linked worktree any more.
	t.exec("INSERT INTO workspaces(id,tenant_id,owner_user_id,creator_user_id,creator_evidence,project_id,kind,desired_state,observed_state,requested_ref) VALUES($1,$2,$3,$4,'verified_request',$5,$6,'running','provisioning',$7)", wid, tid, owner, creator, pid, kind, ref)
	if kind == "isolated" {
		t.exec("INSERT INTO tasks(id,workspace_id,title) VALUES($1,$2,$3)", newID(), wid, title)
	}
}

func createWorkspace(t *transaction, r *PublicRequest, p Object, uid, hash string) Object {
	require(p.S("lifecycle") == "active", 409, "resource_unavailable")
	idleProject(t, p.S("id"))
	title := validText(r.Body.S("title"), 200)
	ref := validRef(r.Body.S("baseRef"))
	if ref == "HEAD" {
		// HEAD means the Project's default branch, which is concrete for every Project created
		// since defaultBranch became required; an older Project that still stores HEAD cannot say.
		ref = p.S("defaultBranch")
		require(ref != "HEAD", 400, "default_branch_required")
	}
	wid := newID()
	// The project's durable owner is part of the workspace FK. A different
	// tenant member may initiate this action, recorded separately as actor.
	insertWorkspace(t, r.TenantID, p.S("ownerUserId"), uid, p.S("id"), wid, "isolated", ref, title)
	op := newOperation(t, r, uid, p.S("id"), wid, "create_workspace", "sandbox", hash, Object{})
	return Object{"resource": workspace(t, r.TenantID, uid, wid, false), "operation": op}
}

func newOperation(t *transaction, r *PublicRequest, uid, pid, wid, kind, step, hash string, req Object) Object {
	id := newID()
	var w any
	if wid != "" {
		w = wid
	}
	t.exec("INSERT INTO operations(id,tenant_id,actor_user_id,project_id,workspace_id,kind,state,step,request,idempotency_key,request_hash) VALUES($1,$2,$3,$4,$5,$6,'queued',$7,$8,$9,$10)", id, r.TenantID, uid, pid, w, kind, step, jsonText(req), r.Key, hash)
	t.queued = append(t.queued, id)
	if wid != "" && (kind == "create_project" || kind == "create_workspace") {
		reserveRuntimeMaintenance(t, wid, id)
	}
	return t.one("SELECT * FROM operations WHERE id=$1", id)
}

func checkActivities(t *transaction, w Object) {
	require(t.one("SELECT id FROM execution_tickets WHERE workspace_id=$1 AND state='active' AND terminated_by_force_stop_id IS NULL", w.S("id")) == nil, 409, "resource_in_use")
	require(t.one("SELECT execution_id FROM clone_executions WHERE workspace_id=$1 AND result IS NULL AND terminated_by_force_stop_id IS NULL", w.S("id")) == nil, 409, "resource_in_use")
	require(t.one("SELECT execution_id FROM node_executions WHERE workspace_id=$1 AND result IS NULL AND terminated_by_force_stop_id IS NULL", w.S("id")) == nil, 409, "resource_in_use")
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
	if !admin {
		requireRuntimeSession(t, w, uid, r.Body.S("sessionId"))
	}
	requireNoForceStop(t, w.S("id"))
	idleProject(t, p.S("id"))
	kind, step := "stop", "quiesce"
	req := Object{"previous": Object{w.S("id"): w}}
	if strings.HasSuffix(r.Path, "/start") {
		kind, step = "start", "sandbox"
		require(w.S("baseCommitId") != "", 409, "runtime_initialization_incomplete")
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
		if strings.HasSuffix(r.Path, "/restart") {
			kind = "restart"
		}
		closeAdmission(t, w, desired)
	}
	op := newOperation(t, r, uid, p.S("id"), w.S("id"), kind, step, hash, req)
	reserveRuntimeMaintenance(t, w.S("id"), op.S("id"))
	resource := workspace(t, r.TenantID, uid, w.S("id"), admin)
	if admin {
		resource = adminResource(resource)
		op = adminOperation(op)
	}
	return Object{"resource": resource, "operation": op}
}

func ownedOperation(t *transaction, r *PublicRequest, uid string) Object {
	require(validID(r.OperationID), 404, "not_found")
	o := t.one("SELECT o.* FROM operations o JOIN projects p ON p.id=o.project_id WHERE o.id=$1 AND o.tenant_id=$2 AND p.tenant_id=$2", r.OperationID, r.TenantID)
	require(o != nil, 404, "not_found")
	if o.S("workspaceId") != "" {
		workspace(t, r.TenantID, uid, o.S("workspaceId"), false)
	} else {
		membership(t, r.TenantID, uid, true)
	}
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
	t.queued = append(t.queued, r.OperationID)
	return Object{"operation": ownedOperation(t, r, uid)}
}
