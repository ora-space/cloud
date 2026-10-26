package integration

import (
	"testing"

	"github.com/google/uuid"
)

// These HTTP tests pin the scheduled-task model and re-consent boundaries through the real Gin
// router and the Store.Public dispatch, not Store.Public directly: the transport's field whitelist
// (unknown_field) and the 409 fault shapes are exactly what a Store.Public-only test cannot hold.

// seedScheduledTaskHTTPScene seeds a skeleton in its own tenant, joins the fixture's verified user as
// an admin, and lays down the agent and the creator's model chain, so the create/enable path runs the
// real eligibility table against the caller's membership and model configuration.
func seedScheduledTaskHTTPScene(t *testing.T, f *fixture) (skeletonSeed, string) {
	t.Helper()
	f.store.ScheduledTaskAcceptanceEnabled = true
	s := seedAgentIssueRunSkeleton(t, f.store.Pool)
	_, err := f.store.Pool.Exec(`INSERT INTO tenant_memberships(tenant_id,user_id,role,status) VALUES($1,$2,'admin','active')`, s.tenantID, f.uid)
	must(t, err)
	_, err = f.store.Pool.Exec(`UPDATE issues SET project_ref=$1 WHERE id=$2`, s.projectID, s.issueID)
	must(t, err)
	agentID := seedScheduledTaskAgent(t, f.store.Pool, s)
	seedScheduledTaskModelChain(t, f.store, f.uid)
	return s, agentID
}

// TestScheduledTaskModelOverHTTP pins D1 model selection through the transport: the create body
// carries model and it round-trips, and a PATCH model change persists with the revision bump.
func TestScheduledTaskModelOverHTTP(t *testing.T) {
	f := setup(t)
	s, agentID := seedScheduledTaskHTTPScene(t, f)
	path := "/api/v1/tenants/" + s.tenantID + "/scheduled-tasks"

	create := mustJSONRaw(map[string]any{
		"issueId": s.issueID, "agentId": agentID, "name": "Model task", "prompt": "Sync",
		"model":    scheduledTaskModelID,
		"schedule": map[string]any{"kind": "every_hours", "intervalHours": 1},
		"timezone": "Asia/Shanghai",
		"enabled":  false,
	})
	status, out, err := f.threadRequest("POST", path, create, uuid.NewString())
	must(t, err)
	if status != 200 || out.O("resource").S("model") != scheduledTaskModelID {
		t.Fatalf("HTTP create with model: want 200 + model %q, got %d %v", scheduledTaskModelID, status, out)
	}
	taskID := out.O("resource").S("id")

	patch := mustJSONRaw(map[string]any{"model": scheduledTaskAlternateModelID, "version": out.O("resource").N("version")})
	status, out, err = f.threadRequest("PATCH", path+"/"+taskID, patch, uuid.NewString())
	must(t, err)
	if status != 200 || out.O("resource").S("model") != scheduledTaskAlternateModelID {
		t.Fatalf("HTTP PATCH model: want 200 + model %q, got %d %v", scheduledTaskAlternateModelID, status, out)
	}
	var model string
	var revision int64
	must(t, f.store.Pool.QueryRow(`SELECT COALESCE(model,''), revision FROM scheduled_tasks WHERE id=$1`, taskID).Scan(&model, &revision))
	if model != scheduledTaskAlternateModelID || revision != 2 {
		t.Fatalf("model update must persist with revision bump, got model=%q revision=%d", model, revision)
	}
}

// TestScheduledTaskReconsentOverHTTP pins D5 re-consent through the transport: after revoke clears the
// consent, enable without approval is refused and enable with a fresh approval restores the consent.
func TestScheduledTaskReconsentOverHTTP(t *testing.T) {
	f := setup(t)
	s, agentID := seedScheduledTaskHTTPScene(t, f)
	path := "/api/v1/tenants/" + s.tenantID + "/scheduled-tasks"

	create := mustJSONRaw(map[string]any{
		"issueId": s.issueID, "agentId": agentID, "name": "Consent task", "prompt": "Sync",
		"schedule": map[string]any{"kind": "every_hours", "intervalHours": 1},
		"timezone": "Asia/Shanghai",
		"approval": map[string]any{"policy": "full_auto_in_cloud_workspace"},
		"enabled":  true,
	})
	status, out, err := f.threadRequest("POST", path, create, uuid.NewString())
	must(t, err)
	if status != 200 {
		t.Fatalf("HTTP create enabled: want 200, got %d %v", status, out)
	}
	taskID := out.O("resource").S("id")

	status, _, err = f.threadRequest("POST", path+"/"+taskID+"/revoke-consent", `{}`, uuid.NewString())
	must(t, err)
	if status != 200 {
		t.Fatalf("HTTP revoke-consent: want 200, got %d", status)
	}

	status, out, err = f.threadRequest("POST", path+"/"+taskID+"/enable", `{}`, uuid.NewString())
	must(t, err)
	if status != 409 || out.S("code") != "approval_policy_invalid" {
		t.Fatalf("HTTP enable without fresh consent: want 409 approval_policy_invalid, got %d %v", status, out)
	}

	status, out, err = f.threadRequest("POST", path+"/"+taskID+"/enable", mustJSONRaw(map[string]any{"approval": map[string]any{"policy": "full_auto_in_cloud_workspace"}}), uuid.NewString())
	must(t, err)
	if status != 200 {
		t.Fatalf("HTTP enable with fresh consent: want 200, got %d %v", status, out)
	}
	var enabled bool
	var policy string
	must(t, f.store.Pool.QueryRow(`SELECT enabled, COALESCE(unattended_policy,'') FROM scheduled_tasks WHERE id=$1`, taskID).Scan(&enabled, &policy))
	if !enabled || policy != "full_auto_in_cloud_workspace" {
		t.Fatalf("re-consent enable must restore consent, got enabled=%v policy=%q", enabled, policy)
	}
}
