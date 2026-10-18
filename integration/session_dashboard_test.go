package integration

import (
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/wanglongan587/cloud/internal/core"
)

func sessionDashboardListPath(tenant string) string {
	return "/api/v1/tenants/" + tenant + "/agent-sessions"
}

func sessionDashboardHistoryPath(tenant, issue, run string) string {
	return "/api/v1/tenants/" + tenant + "/issues/" + issue + "/runs/" + run + "/history"
}

func sessionDashboardFault(t *testing.T, f *fixture, user core.Claims, path string, want int, code string) {
	t.Helper()
	status, out, err := f.threadRequestAs(user, "GET", path, "", "")
	must(t, err)
	if status != want || out.S("code") != code {
		t.Fatalf("GET %s: status=%d code=%q, want %d %q", path, status, out.S("code"), want, code)
	}
}

func TestSessionDashboardListHTTPAndStrictQueries(t *testing.T) {
	f := setup(t)
	scene := seedThreadScene(t, f)
	_, err := f.store.Pool.Exec(`UPDATE issue_runs SET status='running',created_at='2026-10-01T10:00:00Z',input='{"agentPluginId":"official/test"}' WHERE id=$1`, scene.runID)
	must(t, err)
	path := sessionDashboardListPath(scene.tenantID)
	query := url.Values{
		"status": {"running"}, "agent": {"official/test"}, "limit": {"1"},
		"from": {"2026-10-01T18:00:00+08:00"}, "to": {"2026-10-01T11:00:00Z"},
	}
	// The simulator transport validates the successful payload against the generated OpenAPI.
	out := f.call("GET", path+"?"+query.Encode(), nil, "", 200)
	items := threadItems(t, out)
	if len(items) != 1 || items[0].S("id") != scene.runID || items[0].S("threadState") != "pending" || out.S("nextCursor") != "" {
		t.Fatalf("list route or filter binding lost persisted session: %v", out)
	}
	for _, entry := range []struct {
		query string
		code  string
	}{
		{"unknown=value", "invalid_session_filter"},
		{"historyKey=revisions/private", "invalid_session_filter"},
		{"before=" + scene.runID, "invalid_session_filter"},
		{"status=running&status=failed", "invalid_session_filter"},
		{"status=running;status=failed", "invalid_session_filter"},
		{"status=running%zz", "invalid_session_filter"},
		{"limit=1&limit=2", "invalid_session_filter"},
		{"agent=", "invalid_session_filter"},
		{"limit=", "invalid_session_filter"},
		{"limit=0", "invalid_pagination"},
		{"limit=-1", "invalid_pagination"},
		{"limit=101", "invalid_pagination"},
		{"limit=1.5", "invalid_pagination"},
		{"after=not-a-uuid", "invalid_pagination"},
		{"projectId=not-a-uuid", "invalid_session_filter"},
		{"status=unknown", "invalid_session_filter"},
		{"agent=" + strings.Repeat("x", 201), "invalid_session_filter"},
		{"from=2026-10-01", "invalid_session_filter"},
		{"to=tomorrow", "invalid_session_filter"},
		{"from=2026-10-02T10:00:00Z&to=2026-10-01T10:00:00Z", "invalid_session_filter"},
	} {
		t.Run(entry.query, func(t *testing.T) {
			sessionDashboardFault(t, f, f.user, path+"?"+entry.query, 400, entry.code)
		})
	}
	query = url.Values{"projectId": {uuid.NewString()}}
	empty := f.call("GET", path+"?"+query.Encode(), nil, "", 200)
	if len(threadItems(t, empty)) != 0 {
		t.Fatal("projectId query was ignored")
	}
}

func TestSessionDashboardReadAuthorizationAndHistoryWiring(t *testing.T) {
	f := setup(t)
	scene := seedThreadScene(t, f)
	listPath := sessionDashboardListPath(scene.tenantID)
	historyPath := sessionDashboardHistoryPath(scene.tenantID, scene.issueID, scene.runID)
	stranger, _ := f.addUser(t, "session-outsider", "Session outsider")
	for _, path := range []string{listPath, historyPath} {
		sessionDashboardFault(t, f, stranger, path, 403, "membership_required")
	}
	unknown := f.user
	unknown.Subject = "session-unregistered"
	for _, path := range []string{listPath, historyPath} {
		sessionDashboardFault(t, f, unknown, path, 403, "membership_required")
	}
	if f.scalar(`SELECT count(*) FROM user_identities WHERE subject=$1`, unknown.Subject) != 0 {
		t.Fatal("dashboard GET provisioned an unregistered user")
	}
	sessionDashboardFault(t, f, f.user, sessionDashboardListPath("invalid"), 404, "not_found")
	foreignTenant := f.moreTenant()
	foreignList := f.call("GET", sessionDashboardListPath(foreignTenant), nil, "", 200)
	if len(threadItems(t, foreignList)) != 0 {
		t.Fatal("session list leaked another tenant's run")
	}
	sessionDashboardFault(t, f, f.user, sessionDashboardListPath(foreignTenant)+"?after="+scene.runID, 400, "invalid_pagination")
	sessionDashboardFault(t, f, f.user, historyPath, 404, "history_unavailable")
	for _, query := range []string{"limit=1", "historyKey=revisions/private", "url=http%3A%2F%2Fexample.invalid", "after=0", "unknown=", "source=archive"} {
		sessionDashboardFault(t, f, f.user, historyPath+"?"+query, 400, "invalid_session_filter")
	}
	for _, path := range []string{
		sessionDashboardHistoryPath(foreignTenant, scene.issueID, scene.runID),
		sessionDashboardHistoryPath(scene.tenantID, f.moreIssue(scene), scene.runID),
		sessionDashboardHistoryPath(scene.tenantID, scene.issueID, uuid.NewString()),
		sessionDashboardHistoryPath(scene.tenantID, scene.issueID, "not-a-uuid"),
	} {
		sessionDashboardFault(t, f, f.user, path, 404, "not_found")
	}
	team := uuid.NewString()
	_, err := f.store.Pool.Exec(`INSERT INTO issue_runs(id,tenant_id,issue_id,executor_type,executor_id) VALUES($1,$2,$3,'team',$4)`, team, scene.tenantID, scene.issueID, uuid.NewString())
	must(t, err)
	sessionDashboardFault(t, f, f.user, sessionDashboardHistoryPath(scene.tenantID, scene.issueID, team), 404, "not_found")
	_, err = f.store.Pool.Exec(`UPDATE issue_runs SET deleted_at=now() WHERE id=$1`, scene.runID)
	must(t, err)
	sessionDashboardFault(t, f, f.user, historyPath, 404, "not_found")
	empty := f.call("GET", listPath, nil, "", 200)
	if len(threadItems(t, empty)) != 0 {
		t.Fatal("session list included deleted or non-agent runs")
	}
}
