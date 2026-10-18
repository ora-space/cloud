package core

import (
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func sessionListExec(t *testing.T, s *Store, query string, args ...any) {
	t.Helper()
	if _, err := s.Pool.ExecContext(t.Context(), query, args...); err != nil {
		t.Fatalf("stage session list fixture: %v", err)
	}
}

func sessionListRequest(t *testing.T, s *Store, seed dispSeed) PublicRequest {
	t.Helper()
	claims, uid := modelUser(t, s, "session-list-reader")
	sessionListExec(t, s, `INSERT INTO tenant_memberships(tenant_id,user_id,role,status) VALUES($1,$2,'member','active')`, seed.tenant, uid)
	return PublicRequest{Method: "GET", Path: "/api/v1/tenants/" + seed.tenant + "/agent-sessions", TenantID: seed.tenant, Identity: claims}
}

// These rows stage persisted runs, including ones with no declared Thread. The list must not
// create a Thread or derive a session time from a lifecycle transition while reading them.
func sessionListRun(t *testing.T, s *Store, seed dispSeed, id, executor, status, at, plugin string) string {
	t.Helper()
	if id == "" {
		id = uuid.NewString()
	}
	sessionListExec(t, s, `INSERT INTO issue_runs(id,tenant_id,issue_id,executor_type,executor_id,status,created_at,input)
		VALUES($1,$2,$3,$4,$5,$6,$7::timestamptz,jsonb_build_object('agentPluginId',$8::text,'task','synthetic private prompt'))`,
		id, seed.tenant, seed.issue, executor, uuid.NewString(), status, at, plugin)
	return id
}

func sessionListItems(t *testing.T, s *Store, request PublicRequest) ([]Object, string) {
	t.Helper()
	out, status, err := s.Public(t.Context(), &request)
	if err != nil || status != 200 {
		t.Fatalf("session list status=%d error=%v", status, err)
	}
	items, ok := out["items"].([]Object)
	if !ok {
		t.Fatalf("session items type=%T", out["items"])
	}
	return items, out.S("nextCursor")
}

func sessionListIDs(items []Object) []string {
	ids := make([]string, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.S("id"))
	}
	return ids
}

func TestSessionListScopeAndFilters(t *testing.T) {
	s := dispatcherDB(t)
	seed := seedDispatchScene(t, s.Pool, false)
	request := sessionListRequest(t, s, seed)
	sessionListExec(t, s, `UPDATE issue_runs SET created_at='2026-10-01T09:00:00Z' WHERE id=$1`, seed.run)
	completed := sessionListRun(t, s, seed, "", "agent", "completed", "2026-10-01T10:00:00Z", "official/test")
	failed := sessionListRun(t, s, seed, "", "agent", "failed", "2026-10-01T11:00:00Z", "official/test")
	otherAgent := sessionListRun(t, s, seed, "", "agent", "running", "2026-10-01T11:30:00Z", "official/other")
	for _, executor := range []string{"team", "workflow"} {
		sessionListRun(t, s, seed, "", executor, "completed", "2026-10-01T13:00:00Z", "official/test")
	}
	deleted := sessionListRun(t, s, seed, "", "agent", "completed", "2026-10-01T14:00:00Z", "official/test")
	sessionListExec(t, s, `UPDATE issue_runs SET deleted_at=now() WHERE id=$1`, deleted)

	otherIssue := uuid.NewString()
	sessionListExec(t, s, `INSERT INTO issues(id,tenant_id,creator_user_id,title,number)
		SELECT $1,tenant_id,creator_user_id,'No project',2 FROM issues WHERE id=$2`, otherIssue, seed.issue)
	unbound := seed
	unbound.issue = otherIssue
	noProject := sessionListRun(t, s, unbound, "", "agent", "running", "2026-10-01T12:00:00Z", "official/other")
	deletedIssue := uuid.NewString()
	sessionListExec(t, s, `INSERT INTO issues(id,tenant_id,creator_user_id,title,number,deleted_at)
		SELECT $1,tenant_id,creator_user_id,'Deleted',3,now() FROM issues WHERE id=$2`, deletedIssue, seed.issue)
	unbound.issue = deletedIssue
	sessionListRun(t, s, unbound, "", "agent", "completed", "2026-10-01T15:00:00Z", "official/test")
	foreign := seedDispatchScene(t, s.Pool, false)

	cases := []struct {
		name   string
		filter func(*PublicRequest)
		want   []string
	}{
		{"all agent runs", func(*PublicRequest) {}, []string{noProject, otherAgent, failed, completed, seed.run}},
		{"project", func(r *PublicRequest) { r.SessionProjectID = seed.project }, []string{otherAgent, failed, completed, seed.run}},
		{"foreign project", func(r *PublicRequest) { r.SessionProjectID = foreign.project }, []string{}},
		{"status", func(r *PublicRequest) { r.SessionStatus = "failed" }, []string{failed}},
		{"agent", func(r *PublicRequest) { r.SessionAgent = "official/test" }, []string{failed, completed}},
		{"inclusive from exclusive to", func(r *PublicRequest) {
			r.SessionFrom, r.SessionTo = "2026-10-01T18:00:00+08:00", "2026-10-01T11:00:00Z"
		}, []string{completed}},
		{"combined", func(r *PublicRequest) {
			r.SessionProjectID, r.SessionStatus, r.SessionAgent = seed.project, "completed", "official/test"
			r.SessionFrom, r.SessionTo = "2026-10-01T09:00:00Z", "2026-10-01T12:00:00Z"
		}, []string{completed}},
		{"literal agent filter", func(r *PublicRequest) { r.SessionAgent = "' OR 1=1 --" }, []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := request
			tc.filter(&r)
			items, next := sessionListItems(t, s, r)
			if got := sessionListIDs(items); !reflect.DeepEqual(got, tc.want) || next != "" {
				t.Fatalf("ids=%v next=%q, want=%v", got, next, tc.want)
			}
			for _, item := range items {
				if item["model"] != nil || item["threadState"] != nil || item.B("historyAvailable") || item["input"] != nil || item["task"] != nil {
					t.Fatalf("list invented metadata or leaked run input: %v", item)
				}
			}
		})
	}
}

func TestSessionListDescendingCursorBoundaries(t *testing.T) {
	s := dispatcherDB(t)
	seed := seedDispatchScene(t, s.Pool, false)
	request := sessionListRequest(t, s, seed)
	sessionListExec(t, s, `UPDATE issue_runs SET deleted_at=now() WHERE id=$1`, seed.run)
	ids := []string{
		"10000000-0000-4000-8000-000000000001", "10000000-0000-4000-8000-000000000002",
		"10000000-0000-4000-8000-000000000003", "10000000-0000-4000-8000-000000000004",
		"10000000-0000-4000-8000-000000000005",
	}
	for i, id := range ids {
		at := "2026-10-01T10:00:00Z"
		if i == 0 {
			at = "2026-10-01T09:00:00Z"
		}
		sessionListRun(t, s, seed, id, "agent", "completed", at, "official/test")
	}
	request.Limit = 2
	var got []string
	for _, want := range [][]string{{ids[4], ids[3]}, {ids[2], ids[1]}, {ids[0]}} {
		items, next := sessionListItems(t, s, request)
		page := sessionListIDs(items)
		if !reflect.DeepEqual(page, want) {
			t.Fatalf("page after %q=%v want=%v", request.After, page, want)
		}
		got = append(got, page...)
		if len(want) == 2 && next != want[1] || len(want) == 1 && next != "" {
			t.Fatalf("wrong continuation: page=%v next=%q", page, next)
		}
		request.After = next
	}
	if len(got) != len(ids) {
		t.Fatalf("pagination lost or repeated records: %v", got)
	}
	request.After = ids[0]
	items, next := sessionListItems(t, s, request)
	if len(items) != 0 || next != "" {
		t.Fatalf("terminal cursor returned more rows: %v %q", items, next)
	}
	foreign := seedDispatchScene(t, s.Pool, false)
	team := sessionListRun(t, s, seed, "", "team", "completed", "2026-10-01T10:00:00Z", "")
	for _, cursor := range []string{foreign.run, seed.run, team, uuid.NewString(), "not-a-uuid"} {
		request.After = cursor
		_, _, err := s.Public(t.Context(), &request)
		expectModelFault(t, err, "invalid_pagination")
		if ErrorCode(err).Status != 400 {
			t.Fatalf("invalid cursor status=%d", ErrorCode(err).Status)
		}
	}
}

func TestSessionListRejectsInvalidFiltersAndDoesNotProvisionReaders(t *testing.T) {
	s := dispatcherDB(t)
	seed := seedDispatchScene(t, s.Pool, false)
	request := sessionListRequest(t, s, seed)
	cases := []struct {
		name   string
		change func(*PublicRequest)
		code   string
	}{
		{"negative limit", func(r *PublicRequest) { r.Limit = -1 }, "invalid_pagination"},
		{"large limit", func(r *PublicRequest) { r.Limit = 101 }, "invalid_pagination"},
		{"before", func(r *PublicRequest) { r.Before = seed.run }, "invalid_pagination"},
		{"bad project", func(r *PublicRequest) { r.SessionProjectID = "invalid" }, "invalid_session_filter"},
		{"bad status", func(r *PublicRequest) { r.SessionStatus = "unknown" }, "invalid_session_filter"},
		{"long agent", func(r *PublicRequest) { r.SessionAgent = strings.Repeat("x", 201) }, "invalid_session_filter"},
		{"bad from", func(r *PublicRequest) { r.SessionFrom = "2026-10-01" }, "invalid_session_filter"},
		{"bad to", func(r *PublicRequest) { r.SessionTo = "tomorrow" }, "invalid_session_filter"},
		{"equal range", func(r *PublicRequest) { r.SessionFrom, r.SessionTo = "2026-10-01T10:00:00Z", "2026-10-01T10:00:00Z" }, "invalid_session_filter"},
		{"reversed range", func(r *PublicRequest) { r.SessionFrom, r.SessionTo = "2026-10-02T10:00:00Z", "2026-10-01T10:00:00Z" }, "invalid_session_filter"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := request
			tc.change(&r)
			_, _, err := s.Public(t.Context(), &r)
			expectModelFault(t, err, tc.code)
		})
	}
	// A read-only connection rejects identity refresh/provisioning and any hidden reader writes.
	s.Pool.SetMaxOpenConns(1)
	sessionListExec(t, s, "SET default_transaction_read_only=on")
	items, _ := sessionListItems(t, s, request)
	if len(items) != 1 {
		t.Fatal("read-only session list did not return the seeded run")
	}
	unknown := *request.Identity
	unknown.Subject = "never-provisioned-session-reader"
	request.Identity = &unknown
	_, _, err := s.Public(t.Context(), &request)
	expectModelFault(t, err, "membership_required")
	var count int
	if err := s.Pool.QueryRowContext(t.Context(), `SELECT count(*) FROM user_identities WHERE subject=$1`, unknown.Subject).Scan(&count); err != nil || count != 0 {
		t.Fatalf("session read provisioned an identity: count=%d error=%v", count, err)
	}
}
