package integration

import (
	"testing"

	"github.com/google/uuid"
)

// The HTTP boundary must preserve the task message identity through authentication, strict body
// decoding, the public transaction and a later read. Core-only tests cannot cover that chain.
func TestTaskConversationHTTPReadsAndIdempotentlyAcceptsAddressedMessages(t *testing.T) {
	f := setup(t)
	scene := seedThreadScene(t, f)
	_, err := f.store.Pool.Exec(`INSERT INTO space_agents(id,space_id,tenant_id,plugin_id,display_name,status)
		VALUES($1,$2,$3,'official/http-test','HTTP Agent','active')`, scene.seed.agentID, scene.seed.spaceID, scene.tenantID)
	must(t, err)
	_, err = f.store.Pool.Exec("UPDATE issues SET project_ref=$2 WHERE id=$1", scene.issueID, scene.seed.projectID)
	must(t, err)
	path := "/api/v1/tenants/" + scene.tenantID + "/issues/" + scene.issueID + "/conversation"
	body := mustJSONRaw(map[string]any{"agentId": scene.seed.agentID, "body": "Review the saved change"})
	key := uuid.NewString()
	status, first, err := f.threadRequest("POST", path+"/messages", body, key)
	must(t, err)
	if status != 201 || first.O("resource").S("state") != "waiting" {
		t.Fatalf("addressed HTTP send failed: %d %v", status, first)
	}
	status, replay, err := f.threadRequest("POST", path+"/messages", body, key)
	must(t, err)
	if status != 201 || replay.O("resource").S("id") != first.O("resource").S("id") {
		t.Fatalf("HTTP retry created a second message: %d %v", status, replay)
	}
	status, page, err := f.threadRequest("GET", path, "", "")
	must(t, err)
	items, ok := page["items"].([]any)
	if status != 200 || !ok || len(items) != 1 {
		t.Fatalf("task read did not return one durable message: %d %v", status, page)
	}
	status, invalid, err := f.threadRequest("POST", path+"/messages", `{"agentId":"`+scene.seed.agentID+`","body":"valid","state":"submitted"}`, uuid.NewString())
	must(t, err)
	if status != 400 {
		t.Fatalf("HTTP accepted caller-selected delivery state: %d %v", status, invalid)
	}
	status, stopped, err := f.threadRequest("POST", path+"/stop", mustJSONRaw(map[string]any{"runId": scene.runID}), uuid.NewString())
	must(t, err)
	if status != 202 || stopped.S("state") != "stopping" {
		t.Fatalf("HTTP task stop: %d %v", status, stopped)
	}
	status, page, err = f.threadRequest("GET", path, "", "")
	must(t, err)
	if status != 200 || page.S("pausedRunId") != scene.runID || page.B("canStop") {
		t.Fatalf("HTTP task pause was not durable: %d %v", status, page)
	}
}

func TestTaskConversationHTTPRecoversMissingPreparationResult(t *testing.T) {
	f := setup(t)
	scene := seedThreadScene(t, f)
	_, err := f.store.Pool.Exec("UPDATE issue_runs SET status='failed',phase='done',thread_state=NULL,result=NULL WHERE id=$1", scene.runID)
	must(t, err)
	_, err = f.store.Pool.Exec("UPDATE workspaces SET admission_open=false,deleted_at=clock_timestamp() WHERE id=$1", scene.seed.runWorkspaceID)
	must(t, err)
	path := "/api/v1/tenants/" + scene.tenantID + "/issues/" + scene.issueID + "/conversation/discard"
	for _, acknowledgement := range []struct {
		value any
		code  string
	}{
		{false, "lost_work_acknowledgement_required"},
		{"true", "invalid_field_type"},
		{1, "invalid_field_type"},
		{nil, "invalid_field_type"},
	} {
		body := mustJSONRaw(map[string]any{"runId": scene.runID, "acknowledgeLostWork": acknowledgement.value})
		status, out, err := f.threadRequest("POST", path, body, uuid.NewString())
		must(t, err)
		if status != 400 || out.S("code") != acknowledgement.code {
			t.Fatalf("HTTP acknowledgement %v: %d %v; want %s", acknowledgement.value, status, out, acknowledgement.code)
		}
	}
	body := mustJSONRaw(map[string]any{"runId": scene.runID, "acknowledgeLostWork": true})
	key := uuid.NewString()
	for range 2 {
		status, out, err := f.threadRequest("POST", path, body, key)
		must(t, err)
		if status != 200 || out.S("acknowledgedRunId") != scene.runID {
			t.Fatalf("HTTP preparation recovery: %d %v", status, out)
		}
	}
}
