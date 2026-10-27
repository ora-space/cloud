package integration

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/wanglongan587/cloud/internal/api/router"
	"github.com/wanglongan587/cloud/internal/core"
	"github.com/wanglongan587/cloud/internal/simulator"
)

// scheduledTaskEligibilityGap breaks one row of the D5 eligibility table for the seeded scene.
type scheduledTaskEligibilityGap struct {
	name  string
	sql   string
	param func(s skeletonSeed, agentID string) any
}

// scheduledTaskEligibilityGaps are the save-time rows of the D5 table, one at a time. Each statement
// takes a single parameter so a case reads as the one fact it breaks.
var scheduledTaskEligibilityGaps = []scheduledTaskEligibilityGap{
	{
		name: "issue deleted", sql: `UPDATE issues SET deleted_at=now() WHERE id=$1`,
		param: func(s skeletonSeed, _ string) any { return s.issueID },
	},
	{
		name: "issue without project", sql: `UPDATE issues SET project_ref=NULL WHERE id=$1`,
		param: func(s skeletonSeed, _ string) any { return s.issueID },
	},
	{
		name: "space archived", sql: `UPDATE collab_workspaces SET archived_at=now() WHERE id=$1`,
		param: func(s skeletonSeed, _ string) any { return s.spaceID },
	},
	{
		name: "agent retired", sql: `UPDATE space_agents SET status='retired' WHERE id=$1`,
		param: func(_ skeletonSeed, agentID string) any { return agentID },
	},
	{
		name: "creator model unconfigured", sql: `DELETE FROM personal_model_defaults WHERE user_id=$1`,
		param: func(s skeletonSeed, _ string) any { return s.userID },
	},
	{
		name: "model override not in the catalog", sql: `UPDATE personal_model_connections
		SET models=jsonb_path_query_array(models, '$[*] ? (@.id != "vendor/model-v2")') WHERE user_id=$1`,
		param: func(s skeletonSeed, _ string) any { return s.userID },
	},
}

// scheduledTaskCreateBody is a complete, enabled creation request with the alternate model override,
// so every eligibility row (including the override) is exercised by the same body.
func scheduledTaskCreateBody(s skeletonSeed, agentID string, enabled bool) core.Object {
	return core.Object{
		"issueId": s.issueID, "agentId": agentID, "name": "Eligibility", "prompt": "Sync",
		"model":    scheduledTaskAlternateModelID,
		"schedule": core.Object{"kind": "every_hours", "intervalHours": 1},
		"timezone": "Asia/Shanghai", "enabled": enabled,
		"approval": core.Object{"policy": "full_auto_in_cloud_workspace"},
	}
}

// TestScheduledTaskSaveChecksEachEligibilityRow pins the save half of D5 row by row: an enabled
// creation and an enable both refuse a definition that breaks any single row, answer with that row's
// code, and persist nothing. Admission-time consent is intentionally not re-checked (revoking consent
// disables the task instead), so the consent row is only a save/enable rejection.
func TestScheduledTaskSaveChecksEachEligibilityRow(t *testing.T) {
	want := map[string]struct {
		createStatus int
		createCode   string
		enableStatus int
		enableCode   string
	}{
		"issue deleted":         {404, "not_found", 409, "target_unavailable"},
		"issue without project": {409, "issue_project_required", 409, "issue_project_required"},
		// An archived Space is not an explicit eligibility row: the write resolves the tenant's live
		// Space to publish its invalidation and that resolution answers the Space API's archived
		// `404`, rolling the whole write back.
		"space archived":                    {404, "not_found", 404, "not_found"},
		"agent retired":                     {409, "target_unavailable", 409, "target_unavailable"},
		"creator model unconfigured":        {409, "model_unavailable", 409, "model_unavailable"},
		"model override not in the catalog": {409, "model_unavailable", 409, "model_unavailable"},
	}
	for _, gap := range scheduledTaskEligibilityGaps {
		t.Run("create/"+gap.name, func(t *testing.T) {
			store, s, agentID := scheduledTaskStore(t)
			creator := scheduledTaskCaller(t, store, s.tenantID)
			_, err := store.Pool.Exec(gap.sql, gap.param(s, agentID))
			must(t, err)

			_, _, err = store.Public(context.Background(), &core.PublicRequest{
				Method: "POST", Key: "eligibility-create", Path: "/api/v1/tenants/" + s.tenantID + "/scheduled-tasks",
				TenantID: s.tenantID, Identity: creator, Body: scheduledTaskCreateBody(s, agentID, true),
			})
			if fault := core.ErrorCode(err); fault.Status != want[gap.name].createStatus || fault.Code != want[gap.name].createCode {
				t.Fatalf("create with %s = %v, want %d %s", gap.name, err, want[gap.name].createStatus, want[gap.name].createCode)
			}
			var count int
			must(t, store.Pool.QueryRow(`SELECT count(*) FROM scheduled_tasks WHERE tenant_id=$1`, s.tenantID).Scan(&count))
			if count != 0 {
				t.Fatalf("a refused create must persist nothing, got %d task(s)", count)
			}
		})
		t.Run("enable/"+gap.name, func(t *testing.T) {
			store, s, agentID := scheduledTaskStore(t)
			creator := scheduledTaskCaller(t, store, s.tenantID)
			out, _, err := store.Public(context.Background(), &core.PublicRequest{
				Method: "POST", Key: "eligibility-draft", Path: "/api/v1/tenants/" + s.tenantID + "/scheduled-tasks",
				TenantID: s.tenantID, Identity: creator, Body: scheduledTaskCreateBody(s, agentID, false),
			})
			must(t, err)
			taskID := out.O("resource").S("id")
			_, err = store.Pool.Exec(gap.sql, gap.param(s, agentID))
			must(t, err)
			before := readScheduledTaskState(t, store, taskID)

			_, _, err = store.Public(context.Background(), &core.PublicRequest{
				Method: "POST", Key: "eligibility-enable", Path: "/api/v1/tenants/" + s.tenantID + "/scheduled-tasks/" + taskID + "/enable",
				TenantID: s.tenantID, ScheduledTaskID: taskID, Identity: creator,
				Body: core.Object{"approval": core.Object{"policy": "full_auto_in_cloud_workspace"}},
			})
			if fault := core.ErrorCode(err); fault.Status != want[gap.name].enableStatus || fault.Code != want[gap.name].enableCode {
				t.Fatalf("enable with %s = %v, want %d %s", gap.name, err, want[gap.name].enableStatus, want[gap.name].enableCode)
			}
			if after := readScheduledTaskState(t, store, taskID); after != before {
				t.Fatalf("a refused enable must not change the task: before=%+v after=%+v", before, after)
			}
		})
	}
}

// TestScheduledTaskSaveRequiresTheSupportedConsent pins the consent row of D5 at save time: an
// enabled creation without the one supported unattended policy is a malformed request, and enabling a
// task that holds no consent without supplying a fresh one is `approval_policy_invalid`.
func TestScheduledTaskSaveRequiresTheSupportedConsent(t *testing.T) {
	for _, tc := range []struct {
		name     string
		approval any
	}{
		{name: "no approval"},
		{name: "unsupported policy", approval: core.Object{"policy": "ask_every_tool_call"}},
	} {
		t.Run("create/"+tc.name, func(t *testing.T) {
			store, s, agentID := scheduledTaskStore(t)
			creator := scheduledTaskCaller(t, store, s.tenantID)
			body := scheduledTaskCreateBody(s, agentID, true)
			delete(body, "approval")
			if tc.approval != nil {
				body["approval"] = tc.approval
			}
			_, _, err := store.Public(context.Background(), &core.PublicRequest{
				Method: "POST", Key: "consent-create", Path: "/api/v1/tenants/" + s.tenantID + "/scheduled-tasks",
				TenantID: s.tenantID, Identity: creator, Body: body,
			})
			if fault := core.ErrorCode(err); fault.Status != 400 || fault.Code != "invalid_request" {
				t.Fatalf("enabled create with %s = %v, want 400 invalid_request", tc.name, err)
			}
		})
		t.Run("enable/"+tc.name, func(t *testing.T) {
			store, s, agentID := scheduledTaskStore(t)
			creator := scheduledTaskCaller(t, store, s.tenantID)
			body := scheduledTaskCreateBody(s, agentID, false)
			delete(body, "approval")
			out, _, err := store.Public(context.Background(), &core.PublicRequest{
				Method: "POST", Key: "consent-draft", Path: "/api/v1/tenants/" + s.tenantID + "/scheduled-tasks",
				TenantID: s.tenantID, Identity: creator, Body: body,
			})
			must(t, err)
			taskID := out.O("resource").S("id")
			enable := core.Object{}
			if tc.approval != nil {
				enable["approval"] = tc.approval
			}
			_, _, err = store.Public(context.Background(), &core.PublicRequest{
				Method: "POST", Key: "consent-enable", Path: "/api/v1/tenants/" + s.tenantID + "/scheduled-tasks/" + taskID + "/enable",
				TenantID: s.tenantID, ScheduledTaskID: taskID, Identity: creator, Body: enable,
			})
			if fault := core.ErrorCode(err); fault.Status != 409 || fault.Code != "approval_policy_invalid" {
				t.Fatalf("enable with %s = %v, want 409 approval_policy_invalid", tc.name, err)
			}
			if state := readScheduledTaskState(t, store, taskID); state.enabled || state.hasUnattendedConsent {
				t.Fatalf("a refused enable must leave the draft disabled and without consent, got %+v", state)
			}
		})
	}
}

// TestScheduledTaskReadFollowsTenantMembership pins D1 visibility: any effective tenant member can
// read the task and its history, not only the creator. A caller who cannot read the bound issue is
// refused before the task is looked up: a former member gets the tenant gate's `403
// membership_required` (the same gate the Thread read uses), and a member of another tenant who
// names the task under their own tenant gets `404 not_found`, indistinguishable from a missing task.
func TestScheduledTaskReadFollowsTenantMembership(t *testing.T) {
	store, s, agentID := scheduledTaskStore(t)
	taskID, _ := admitScheduledRun(t, store, s, agentID, "Member visibility")
	read := func(identity *core.Claims, tenantID, suffix string) (core.Object, int, error) {
		return store.Public(context.Background(), &core.PublicRequest{
			Method: "GET", Path: "/api/v1/tenants/" + tenantID + "/scheduled-tasks/" + taskID + suffix,
			TenantID: tenantID, ScheduledTaskID: taskID, Identity: identity,
		})
	}

	member := scheduledTaskOtherMember(t, store, s.tenantID)
	out, status, err := read(member, s.tenantID, "")
	must(t, err)
	if status != 200 || out.S("id") != taskID || out.S("creatorUserId") != s.userID {
		t.Fatalf("a non-creator member must read the task, got %d %v", status, out)
	}
	out, status, err = read(member, s.tenantID, "/history")
	must(t, err)
	if entries, ok := out["entries"].([]core.Object); status != 200 || !ok || len(entries) != 1 {
		t.Fatalf("a non-creator member must read the history, got %d %v", status, out)
	}

	former := scheduledTaskOtherMember(t, store, s.tenantID)
	_, err = store.Pool.Exec(`UPDATE tenant_memberships SET status='disabled'
		WHERE tenant_id=$1 AND user_id=(SELECT user_id FROM user_identities WHERE subject=$2)`, s.tenantID, former.Subject)
	must(t, err)
	for _, suffix := range []string{"", "/history"} {
		_, _, err = read(former, s.tenantID, suffix)
		if fault := core.ErrorCode(err); fault.Status != 403 || fault.Code != "membership_required" {
			t.Fatalf("a former member reading %q must be refused by the tenant gate, got %v", suffix, err)
		}
	}

	foreignTenantID := uuid.NewString()
	foreignUserID := uuid.NewString()
	// An active tenant needs its active administrator in the same transaction.
	_, err = store.Pool.Exec(`
		WITH u AS (INSERT INTO users(id, display_name, status) VALUES($2, 'Foreign administrator', 'active') RETURNING id),
		     tn AS (INSERT INTO tenants(id, name, status) VALUES($1, 'Foreign tenant', 'active') RETURNING id)
		INSERT INTO tenant_memberships(tenant_id, user_id, role, status) SELECT tn.id, u.id, 'admin', 'active' FROM tn, u`,
		foreignTenantID, foreignUserID)
	must(t, err)
	stranger := scheduledTaskCaller(t, store, foreignTenantID)
	for _, suffix := range []string{"", "/history"} {
		_, _, err = read(stranger, s.tenantID, suffix)
		if fault := core.ErrorCode(err); fault.Status != 403 || fault.Code != "membership_required" {
			t.Fatalf("another tenant's member reading %q under this tenant must be refused by the tenant gate, got %v", suffix, err)
		}
		_, _, err = read(stranger, foreignTenantID, suffix)
		if fault := core.ErrorCode(err); fault.Status != 404 || fault.Code != "not_found" {
			t.Fatalf("another tenant's member naming the task %q under their own tenant must get 404 not_found, got %v", suffix, err)
		}
	}
}

// TestScheduledTaskBindingIsImmutable pins D1 绑定在创建时固定、不可修改: the PATCH transport rejects
// the binding and ownership fields as unknown, and the domain never reads them even if a caller
// reaches it directly, so the creator cannot re-bind the issue or transfer the task. The
// execution placement has no field at all; it is `cloud` by construction.
func TestScheduledTaskBindingIsImmutable(t *testing.T) {
	store, s, agentID := scheduledTaskStore(t)
	taskID := seedEnabledScheduledTask(t, store, s, agentID, "Immutable binding", 5)
	credentials, err := simulator.NewCredentials()
	must(t, err)
	auth, err := core.NewAuthenticator("ora-cloud", credentials.Trust)
	must(t, err)
	server := httptest.NewServer(router.NewDevelopment(store, auth, zap.NewNop()))
	t.Cleanup(server.Close)
	client := &simulator.Client{
		URL: server.URL, Credentials: credentials,
		HTTP: &http.Client{Timeout: 10 * time.Second}, Subject: "controller-a",
	}
	creator := scheduledTaskCaller(t, store, s.tenantID)
	gateway := core.Claims{RegisteredClaims: jwt.RegisteredClaims{Subject: "gateway-a"}}
	path := "/api/v1/tenants/" + s.tenantID + "/scheduled-tasks/" + taskID
	otherIssueID, otherUserID := uuid.NewString(), uuid.NewString()
	before := readScheduledTaskState(t, store, taskID)

	for _, field := range []struct{ name, value string }{
		{"issueId", otherIssueID},
		{"creatorUserId", otherUserID},
	} {
		out, status, err := client.Call(context.Background(), "PATCH", path, "gateway", gateway, creator, "rebind-"+field.name,
			core.Object{"version": before.version, field.name: field.value})
		must(t, err)
		if status != 400 || out.S("code") != "unknown_field" || out.O("params").S("field") != field.name {
			t.Fatalf("PATCH carrying %s = %d %v, want 400 unknown_field", field.name, status, out)
		}
		if after := readScheduledTaskState(t, store, taskID); after != before {
			t.Fatalf("a rejected PATCH carrying %s must not change the task: before=%+v after=%+v", field.name, before, after)
		}
	}

	// Below the transport the domain owns the binding too: a body that slipped the field through is
	// applied as an ordinary edit that leaves the issue and the creator exactly as created.
	_, status, err := store.Public(context.Background(), &core.PublicRequest{
		Method: "PATCH", Key: "rebind-domain", Path: path, TenantID: s.tenantID, ScheduledTaskID: taskID, Identity: creator,
		Body: core.Object{"version": before.version, "name": "Renamed", "issueId": otherIssueID, "creatorUserId": otherUserID},
	})
	must(t, err)
	after := readScheduledTaskState(t, store, taskID)
	if status != 200 || after.name != "Renamed" {
		t.Fatalf("precondition: the creator's ordinary edit must apply, got %d %+v", status, after)
	}
	if after.issueID != before.issueID || after.creatorUserID != before.creatorUserID {
		t.Fatalf("the domain must never re-bind the task: before=%+v after=%+v", before, after)
	}
}
