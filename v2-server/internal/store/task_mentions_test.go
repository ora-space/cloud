package store_test

import (
	"testing"

	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/store/storetest"
)

func TestPendingMentions(t *testing.T) {
	f := newSquadFixture(t)
	ctx := f.ctx
	owner := f.login(t, "owner", "owner")
	alice := f.login(t, "alice", "member")
	addProject(t, f, owner)
	task := createTask(t, f, owner, store.TaskInput{Title: "修复登录页"})
	other := createTask(t, f, owner, store.TaskInput{Title: "写周报"})
	comment := func(taskID, author, key string, mentioned ...string) {
		t.Helper()
		in := store.NewTaskComment{TenantID: f.tenant, TaskID: taskID, AuthorID: author, CreateKey: key, Body: "看一下"}
		for _, id := range mentioned {
			in.Mentions = append(in.Mentions, store.TaskMention{Type: "user", ID: id, DisplayName: "x"})
		}
		if _, e := f.store.CreateTaskComment(ctx, in); e != nil {
			t.Fatal(e)
		}
	}
	pending := func(user string) []store.PendingMention {
		t.Helper()
		got, e := f.store.PendingMentions(ctx, f.tenant, user)
		if e != nil {
			t.Fatal(e)
		}
		return got
	}

	// Mentioning oneself waits on nobody; mentioning alice waits on her, once per task, newest first.
	comment(task.ID, owner, "c1", owner)
	if got := pending(owner); len(got) != 0 {
		t.Fatalf("self mention = %+v", got)
	}
	comment(task.ID, owner, "c2", alice)
	comment(task.ID, owner, "c3", alice)
	comment(other.ID, owner, "c4", alice)
	got := pending(alice)
	if len(got) != 2 {
		t.Fatalf("pending = %+v", got)
	}
	for _, m := range got {
		if m.AuthorID != owner || m.AuthorName != "owner" {
			t.Fatalf("mention = %+v", m)
		}
	}

	// Her reply answers the task's mentions; a later mention waits again.
	comment(task.ID, alice, "r1")
	if got := pending(alice); len(got) != 1 || got[0].TaskID != other.ID {
		t.Fatalf("after reply = %+v", got)
	}
	comment(task.ID, owner, "c5", alice)
	if got := pending(alice); len(got) != 2 {
		t.Fatalf("after a new mention = %+v", got)
	}

	// A finished task waits on nobody.
	if _, e := f.store.SetTaskStatus(ctx, storetest.Access(t, f.store, f.tenant, owner), other.ID, store.TaskCancelled, other.Version); e != nil {
		t.Fatal(e)
	}
	if got := pending(alice); len(got) != 1 || got[0].TaskID != task.ID {
		t.Fatalf("after cancel = %+v", got)
	}
	if got, e := f.store.PendingMentions(ctx, f.tenant, "00000000-0000-4000-8000-0000000000ff"); e != nil || len(got) != 0 {
		t.Fatalf("a stranger's mentions = %+v %v", got, e)
	}
}

func TestMySquadTasks(t *testing.T) {
	f := newSquadFixture(t)
	ctx := f.ctx
	owner := f.login(t, "owner", "owner")
	alice := f.login(t, "alice", "member")
	bob := f.login(t, "bob", "member")
	addProject(t, f, owner)
	squad, e := f.store.CreateSquad(ctx, f.tenant, owner, store.SquadInput{Name: "Web 前端组"})
	if e != nil {
		t.Fatal(e)
	}
	squad, e = f.store.AddSquadMember(ctx, storetest.Access(t, f.store, f.tenant, owner), squad.ID, store.SquadMemberInput{Type: "human", UserID: alice, Version: squad.Version})
	if e != nil {
		t.Fatal(e)
	}
	task := createTask(t, f, owner, store.TaskInput{Title: "改版登录页", Assignee: &store.TaskAssigneeRef{Type: store.AssigneeSquad, ID: squad.ID}})
	mine := func(user string) []store.MySquadTask {
		t.Helper()
		got, e := f.store.MySquadTasks(ctx, f.tenant, user)
		if e != nil {
			t.Fatal(e)
		}
		return got
	}

	// A member sees it waiting; whoever created it does not wait on themself; an outsider has none.
	if got := mine(alice); len(got) != 1 || got[0].TaskID != task.ID || got[0].SquadName != "Web 前端组" || !got[0].Waiting {
		t.Fatalf("member = %+v", got)
	}
	if got := mine(owner); len(got) != 1 || got[0].Waiting {
		t.Fatalf("creator = %+v", got)
	}
	if got := mine(bob); len(got) != 0 {
		t.Fatalf("outsider = %+v", got)
	}

	if ok, e := f.store.InSquad(ctx, f.tenant, squad.ID, alice); e != nil || !ok {
		t.Fatalf("member in squad = %v %v", ok, e)
	}
	if ok, e := f.store.InSquad(ctx, f.tenant, squad.ID, bob); e != nil || ok {
		t.Fatalf("outsider in squad = %v %v", ok, e)
	}

	// Work in progress still waits on the member; once it reaches 待验收 only whoever can confirm it
	// acts, so it no longer waits on a member who never took part.
	review := createTask(t, f, owner, store.TaskInput{Title: "补登录页文案", Assignee: &store.TaskAssigneeRef{Type: store.AssigneeSquad, ID: squad.ID}})
	ownerAccess := storetest.Access(t, f.store, f.tenant, owner)
	if review, e = f.store.SetTaskStatus(ctx, ownerAccess, review.ID, store.TaskRunning, review.Version); e != nil {
		t.Fatal(e)
	}
	waits := func(user, id string) bool {
		t.Helper()
		for _, m := range mine(user) {
			if m.TaskID == id {
				return m.Waiting
			}
		}
		t.Fatalf("task %s missing for %s", id, user)
		return false
	}
	if !waits(alice, review.ID) {
		t.Fatal("a running squad task should wait on a member who has not acted")
	}
	if _, e = f.store.SetTaskStatus(ctx, ownerAccess, review.ID, store.TaskReview, review.Version); e != nil {
		t.Fatal(e)
	}
	if waits(alice, review.ID) {
		t.Fatal("a squad task in review must not wait on a member who cannot confirm it")
	}

	// A comment answers it for that member, and the task stays theirs.
	if _, e := f.store.CreateTaskComment(ctx, store.NewTaskComment{TenantID: f.tenant, TaskID: task.ID, AuthorID: alice, CreateKey: "c1", Body: "我来看"}); e != nil {
		t.Fatal(e)
	}
	if waits(alice, task.ID) {
		t.Fatalf("after a comment = %+v", mine(alice))
	}
}
