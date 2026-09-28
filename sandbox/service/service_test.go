package main

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

const (
	projectID   = "11111111-1111-4111-8111-111111111111"
	workspaceID = "22222222-2222-4222-8222-222222222222"
	effectID    = "33333333-3333-4333-8333-333333333333"
)

func testService(t *testing.T) *service {
	t.Helper()
	s, err := newService(&config{
		atespace:  "test-space",
		statePath: filepath.Join(t.TempDir(), "journal.json"),
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestValidateRequest(t *testing.T) {
	valid := effectRequest{Kind: "sandbox_ensure", ProjectID: projectID, WorkspaceID: workspaceID}
	if got := validateRequest(valid); got != "" {
		t.Fatalf("valid ensure rejected: %s", got)
	}
	valid.Kind = "sandbox_terminate"
	valid.SandboxInstanceID = effectID
	if got := validateRequest(valid); got != "" {
		t.Fatalf("valid terminate rejected: %s", got)
	}
	valid.SandboxInstanceID = "not-a-uuid"
	if got := validateRequest(valid); got != "invalid_request" {
		t.Fatalf("bad sandbox id: got %q", got)
	}
	valid = effectRequest{Kind: "sandbox_suspend", ProjectID: projectID, WorkspaceID: workspaceID}
	if got := validateRequest(valid); got != "unsupported_kind" {
		t.Fatalf("unsupported kind: got %q", got)
	}
}

func TestUnknownEffectGETIsReadOnly404(t *testing.T) {
	s := testService(t)
	req := httptest.NewRequest(http.MethodGet, "/effects/"+effectID, nil)
	w := httptest.NewRecorder()
	s.handleEffects(w, req)
	if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), `"error":"not_found"`) {
		t.Fatalf("response = %d %s", w.Code, w.Body.String())
	}
	if len(s.db.Effects) != 0 || len(s.db.Workspaces) != 0 {
		t.Fatal("GET created lifecycle state")
	}
}

func TestSucceededEffectIsIdempotentAndConflictsOnChangedRequest(t *testing.T) {
	s := testService(t)
	request := effectRequest{Kind: "sandbox_ensure", ProjectID: projectID, WorkspaceID: workspaceID}
	s.db.Effects[effectID] = &effectEntry{
		ID: effectID, ExternalID: effectID, State: "succeeded", Request: request,
		Result: map[string]any{"sandboxInstanceId": effectID, "nodeId": nodeID(workspaceID)},
	}

	entry, status, code := s.putEffect(effectID, request)
	if code != "" || status != http.StatusOK || entry.State != "succeeded" {
		t.Fatalf("idempotent retry = status %d code %q entry %#v", status, code, entry)
	}
	changed := request
	changed.WorkspaceID = "99999999-9999-4999-8999-999999999999"
	_, status, code = s.putEffect(effectID, changed)
	if status != http.StatusConflict || code != "effect_conflict" {
		t.Fatalf("changed retry = status %d code %q", status, code)
	}
}

func TestRouteIdentityRejectsTombstonedSandbox(t *testing.T) {
	s := testService(t)
	request := effectRequest{Kind: "sandbox_ensure", ProjectID: projectID, WorkspaceID: workspaceID}
	s.db.Effects[effectID] = &effectEntry{ID: effectID, State: "succeeded", Request: request}
	s.db.Workspaces[workspaceID] = &workspaceState{
		ProjectID: projectID, WorkspaceID: workspaceID, ActiveSandboxID: effectID,
		Tombstones: map[string]bool{},
	}
	gotNode, status := s.routeIdentity(effectID)
	if status != 0 || gotNode != nodeID(workspaceID) {
		t.Fatalf("active route = node %q status %d", gotNode, status)
	}
	s.db.Workspaces[workspaceID].Tombstones[effectID] = true
	if _, status = s.routeIdentity(effectID); status != http.StatusNotFound {
		t.Fatalf("tombstoned route status = %d", status)
	}
}

func TestTerminateCannotCrossWorkspaceScope(t *testing.T) {
	s := testService(t)
	ensure := effectRequest{Kind: "sandbox_ensure", ProjectID: projectID, WorkspaceID: workspaceID}
	s.db.Effects[effectID] = &effectEntry{ID: effectID, State: "succeeded", Request: ensure}

	request := effectRequest{
		Kind: "sandbox_terminate", ProjectID: projectID,
		WorkspaceID: "99999999-9999-4999-8999-999999999999", SandboxInstanceID: effectID,
	}
	_, status, code := s.putEffect("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", request)
	if status != http.StatusConflict || code != "scope_conflict" {
		t.Fatalf("cross-workspace terminate = status %d code %q", status, code)
	}
	if _, exists := s.db.Effects["aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"]; exists {
		t.Fatal("rejected terminate was persisted as an accepted effect")
	}
}

func TestWorkspaceOwnershipSurvivesDataDeletion(t *testing.T) {
	s := testService(t)
	s.db.Effects[effectID] = &effectEntry{
		ID: effectID, State: "succeeded",
		Request: effectRequest{Kind: "workspace_data_delete", ProjectID: projectID, WorkspaceID: workspaceID},
	}
	request := effectRequest{
		Kind: "sandbox_ensure", ProjectID: "99999999-9999-4999-8999-999999999999", WorkspaceID: workspaceID,
	}
	_, status, code := s.putEffect("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", request)
	if status != http.StatusConflict || code != "scope_conflict" {
		t.Fatalf("workspace reassignment = status %d code %q", status, code)
	}
}
