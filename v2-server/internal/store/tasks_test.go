package store_test

import (
	"errors"
	"testing"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
)

// taskProject records one prepared project the task tests can point at.
const taskProject = "00000000-0000-4000-8000-000000000012"

func addProject(t *testing.T, f *squadFixture, user string) {
	t.Helper()
	_, e := f.store.CreateProject(f.ctx, execution.Scope{TenantID: f.tenant, UserID: user}, store.Project{
		ID: taskProject, Name: "Demo", RepositoryURL: "fixture://demo", Commit: "abc", MirrorPath: "/tmp/tasks"})
	if e != nil {
		t.Fatal(e)
	}
}

func createTask(t *testing.T, f *squadFixture, user string, in store.TaskInput) store.Task {
	t.Helper()
	in.ProjectID = taskProject
	task, e := f.store.CreateTask(f.ctx, f.tenant, user, in)
	if e != nil {
		t.Fatalf("create task %q: %v", in.Title, e)
	}
	return task
}

func TestTaskLifecycle(t *testing.T) {
	f := newSquadFixture(t)
	ctx := f.ctx
	owner := f.login(t, "owner", "owner")
	addProject(t, f, owner)

	created := createTask(t, f, owner, store.TaskInput{Title: "整理演示数据", Description: "把三条示例任务写好"})
	if created.Status != store.TaskPending || created.Version != 1 || created.Assignee != nil ||
		created.CreatedBy != owner || created.CreatedByName != "owner" || created.ProjectID != taskProject {
		t.Fatalf("created = %+v", created)
	}
	list, e := f.store.Tasks(ctx, f.tenant, owner)
	if e != nil || len(list) != 1 || list[0].ID != created.ID {
		t.Fatalf("list = %+v %v", list, e)
	}
	if got, e := f.store.Task(ctx, f.tenant, owner, created.ID); e != nil || got.Title != "整理演示数据" {
		t.Fatalf("get = %+v %v", got, e)
	}

	// An edit bumps the version; the version the caller read is then stale.
	edited, e := f.store.UpdateTask(ctx, f.tenant, owner, created.ID, store.TaskInput{Title: "整理演示数据（改）", Description: "两行", ProjectID: taskProject, Version: 1})
	if e != nil || edited.Version != 2 || edited.Title != "整理演示数据（改）" {
		t.Fatalf("edit = %+v %v", edited, e)
	}
	if _, e := f.store.UpdateTask(ctx, f.tenant, owner, created.ID, store.TaskInput{Title: "再来", ProjectID: taskProject, Version: 1}); !errors.Is(e, store.ErrConflict) {
		t.Fatalf("stale edit = %v", e)
	}

	// The lifecycle moves forward one step at a time and never skips review.
	running, e := f.store.SetTaskStatus(ctx, f.tenant, owner, created.ID, store.TaskRunning, 2)
	if e != nil || running.Status != store.TaskRunning || running.Version != 3 {
		t.Fatalf("start = %+v %v", running, e)
	}
	if _, e := f.store.SetTaskStatus(ctx, f.tenant, owner, created.ID, store.TaskPending, 3); !errors.Is(e, store.ErrInvalidTransition) {
		t.Fatalf("running back to pending = %v", e)
	}
	review, e := f.store.SetTaskStatus(ctx, f.tenant, owner, created.ID, store.TaskReview, 3)
	if e != nil || review.Status != store.TaskReview {
		t.Fatalf("hand in = %+v %v", review, e)
	}
	done, e := f.store.SetTaskStatus(ctx, f.tenant, owner, created.ID, store.TaskDone, 4)
	if e != nil || done.Status != store.TaskDone || done.Version != 5 {
		t.Fatalf("confirm = %+v %v", done, e)
	}
	// done and cancelled are terminal.
	if _, e := f.store.SetTaskStatus(ctx, f.tenant, owner, created.ID, store.TaskCancelled, 5); !errors.Is(e, store.ErrInvalidTransition) {
		t.Fatalf("reopen = %v", e)
	}
	drop := createTask(t, f, owner, store.TaskInput{Title: "不做了"})
	cancelled, e := f.store.SetTaskStatus(ctx, f.tenant, owner, drop.ID, store.TaskCancelled, 1)
	if e != nil || cancelled.Status != store.TaskCancelled {
		t.Fatalf("cancelled = %+v %v", cancelled, e)
	}

	other := createTask(t, f, owner, store.TaskInput{Title: "跳过验收"})
	if _, e := f.store.SetTaskStatus(ctx, f.tenant, owner, other.ID, store.TaskDone, 1); !errors.Is(e, store.ErrInvalidTransition) {
		t.Fatalf("pending to done = %v", e)
	}
	if _, e := f.store.SetTaskStatus(ctx, f.tenant, owner, other.ID, store.TaskRunning, 9); !errors.Is(e, store.ErrConflict) {
		t.Fatalf("stale status write = %v", e)
	}

	// The execution service moves the task with the same rule and refuses a stale status.
	if got, e := f.store.TransitionTaskStatus(ctx, f.tenant, other.ID, store.TaskPending, store.TaskRunning); e != nil || got.Status != store.TaskRunning {
		t.Fatalf("system start = %+v %v", got, e)
	}
	if _, e := f.store.TransitionTaskStatus(ctx, f.tenant, other.ID, store.TaskPending, store.TaskRunning); !errors.Is(e, store.ErrConflict) {
		t.Fatalf("replayed system start = %v", e)
	}
	if _, e := f.store.TransitionTaskStatus(ctx, f.tenant, other.ID, store.TaskRunning, store.TaskDone); !errors.Is(e, store.ErrInvalidTransition) {
		t.Fatalf("system skip review = %v", e)
	}
}

func TestTaskAssignees(t *testing.T) {
	f := newSquadFixture(t)
	ctx := f.ctx
	owner := f.login(t, "owner", "owner")
	member := f.login(t, "member", "member")
	addProject(t, f, owner)

	var agentID string
	if e := f.db.QueryRow(ctx, `INSERT INTO v2_space_agents (tenant_id, name, source_namespace, identifier)
		VALUES ($1, 'OpenCode', 'official', 'ora-space.opencode') RETURNING id`, f.tenant).Scan(&agentID); e != nil {
		t.Fatal(e)
	}
	squad, e := f.store.CreateSquad(ctx, f.tenant, owner, store.SquadInput{Name: "产品小队"})
	if e != nil {
		t.Fatal(e)
	}
	task := createTask(t, f, owner, store.TaskInput{Title: "交给谁"})

	// All three typed assignees are accepted and read back with live names.
	human, e := f.store.SetTaskAssignee(ctx, f.tenant, owner, task.ID, &store.TaskAssigneeRef{Type: store.AssigneeHuman, ID: member}, 1)
	if e != nil || human.Assignee == nil || human.Assignee.DisplayName != "member" || human.Assignee.Identifier != "" {
		t.Fatalf("human assignee = %+v %v", human, e)
	}
	agent, e := f.store.SetTaskAssignee(ctx, f.tenant, owner, task.ID, &store.TaskAssigneeRef{Type: store.AssigneeAgent, ID: agentID}, 2)
	if e != nil || agent.Assignee == nil || agent.Assignee.DisplayName != "OpenCode" ||
		agent.Assignee.Identifier != "official/ora-space.opencode" {
		t.Fatalf("agent assignee = %+v %v", agent, e)
	}
	bySquad, e := f.store.SetTaskAssignee(ctx, f.tenant, owner, task.ID, &store.TaskAssigneeRef{Type: store.AssigneeSquad, ID: squad.ID}, 3)
	if e != nil || bySquad.Assignee == nil || bySquad.Assignee.DisplayName != "产品小队" {
		t.Fatalf("squad assignee = %+v %v", bySquad, e)
	}
	cleared, e := f.store.SetTaskAssignee(ctx, f.tenant, owner, task.ID, nil, 4)
	if e != nil || cleared.Assignee != nil || cleared.Version != 5 {
		t.Fatalf("clear assignee = %+v %v", cleared, e)
	}
	if _, e := f.store.SetTaskAssignee(ctx, f.tenant, owner, task.ID, nil, 4); !errors.Is(e, store.ErrConflict) {
		t.Fatalf("stale assignment = %v", e)
	}

	// A retired agent, an archived squad and an unknown kind are all refused.
	var retired string
	if e := f.db.QueryRow(ctx, `INSERT INTO v2_space_agents (tenant_id, name, status) VALUES ($1, '退役', 'retired') RETURNING id`, f.tenant).Scan(&retired); e != nil {
		t.Fatal(e)
	}
	if _, e := f.store.SetTaskAssignee(ctx, f.tenant, owner, task.ID, &store.TaskAssigneeRef{Type: store.AssigneeAgent, ID: retired}, 5); !errors.Is(e, store.ErrAgentUnavailable) {
		t.Fatalf("retired agent = %v", e)
	}
	if _, e := f.store.ArchiveSquad(ctx, f.tenant, owner, squad.ID, squad.Version); e != nil {
		t.Fatal(e)
	}
	if _, e := f.store.SetTaskAssignee(ctx, f.tenant, owner, task.ID, &store.TaskAssigneeRef{Type: store.AssigneeSquad, ID: squad.ID}, 5); !errors.Is(e, store.ErrInvalidAssignee) {
		t.Fatalf("archived squad = %v", e)
	}
	if _, e := f.store.SetTaskAssignee(ctx, f.tenant, owner, task.ID, &store.TaskAssigneeRef{Type: "robot", ID: member}, 5); !errors.Is(e, store.ErrInvalidAssignee) {
		t.Fatalf("unknown kind = %v", e)
	}

	// Someone who is not in this workspace cannot be assigned, whoever asks.
	var strangerTenant, stranger string
	if e := f.db.QueryRow(ctx, `INSERT INTO tenants (name) VALUES ('别的工作区') RETURNING id`).Scan(&strangerTenant); e != nil {
		t.Fatal(e)
	}
	if e := f.db.QueryRow(ctx, `INSERT INTO users (source, subject, display_name) VALUES ('test', 'stranger', 'stranger') RETURNING id`).Scan(&stranger); e != nil {
		t.Fatal(e)
	}
	if _, e := f.db.Exec(ctx, `INSERT INTO memberships (tenant_id, user_id, role) VALUES ($1, $2, 'member')`, strangerTenant, stranger); e != nil {
		t.Fatal(e)
	}
	if _, e := f.store.SetTaskAssignee(ctx, f.tenant, owner, task.ID, &store.TaskAssigneeRef{Type: store.AssigneeHuman, ID: stranger}, 5); !errors.Is(e, store.ErrUserUnavailable) {
		t.Fatalf("foreign human = %v", e)
	}
}

func TestTaskWorkspaceIsolation(t *testing.T) {
	f := newSquadFixture(t)
	ctx := f.ctx
	owner := f.login(t, "owner", "owner")
	addProject(t, f, owner)
	task := createTask(t, f, owner, store.TaskInput{Title: "只属于演示团队"})

	// A member of another workspace sees neither the task nor the workspace itself.
	var otherTenant, otherUser string
	if e := f.db.QueryRow(ctx, `INSERT INTO tenants (name) VALUES ('别的工作区') RETURNING id`).Scan(&otherTenant); e != nil {
		t.Fatal(e)
	}
	if e := f.db.QueryRow(ctx, `INSERT INTO users (source, subject, display_name) VALUES ('test', 'other', 'other') RETURNING id`).Scan(&otherUser); e != nil {
		t.Fatal(e)
	}
	if _, e := f.db.Exec(ctx, `INSERT INTO memberships (tenant_id, user_id, role) VALUES ($1, $2, 'owner')`, otherTenant, otherUser); e != nil {
		t.Fatal(e)
	}
	if _, e := f.store.Tasks(ctx, f.tenant, otherUser); !errors.Is(e, store.ErrNotFound) {
		t.Fatalf("foreign list = %v", e)
	}
	if _, e := f.store.Task(ctx, f.tenant, otherUser, task.ID); !errors.Is(e, store.ErrNotFound) {
		t.Fatalf("foreign get = %v", e)
	}
	if _, e := f.store.Task(ctx, otherTenant, otherUser, task.ID); !errors.Is(e, store.ErrNotFound) {
		t.Fatalf("task of another workspace = %v", e)
	}
	if _, e := f.store.CreateTask(ctx, otherTenant, otherUser, store.TaskInput{Title: "别人的项目", ProjectID: taskProject}); !errors.Is(e, store.ErrInvalidProject) {
		t.Fatalf("project of another workspace = %v", e)
	}
	if _, e := f.store.Tasks(ctx, otherTenant, otherUser); e != nil {
		t.Fatalf("own workspace list = %v", e)
	}
}
