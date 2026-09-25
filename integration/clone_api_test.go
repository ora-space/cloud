package integration

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/wanglongan587/cloud/internal/core"
)

// control runs one clone control action as the lease-holding simulator controller, through the
// same Store.Control transaction the gRPC ExecutionService uses. The public API test needs a
// dispatch and a result on record; how they arrive over gRPC is covered by control_grpc_test.
func (f *fixture) control(action, submission string, body core.Object) core.Object {
	f.t.Helper()
	body["epoch"] = f.controller.Epoch
	out, e := f.store.Control(context.Background(), &core.ControlRequest{Action: action, SubmissionID: submission, Body: body, Service: &core.Claims{RegisteredClaims: jwt.RegisteredClaims{Subject: f.client.Subject}, Kind: "service", Role: "controller"}})
	must(f.t, e)
	return out
}

func (f *fixture) clone(oid string) core.Object {
	return f.call("GET", f.path("/clones/"+oid), nil, "", 200)
}

func text(v any) string { b, _ := json.Marshal(v); return string(b) }

func TestPublicClonesAcceptOnceAndReflectExecutionFacts(t *testing.T) {
	f := setup(t)
	signals, cancel, ok := f.store.Signals.Subscribe()
	if !ok {
		t.Fatal("control hub refused subscription")
	}
	defer cancel()
	body := core.Object{"requestId": uuid.NewString(), "repository": "https://example.invalid/repo.git", "branch": "main"}

	// Every public mutation needs the idempotency key; acceptance is 202 with a pending view.
	f.call("POST", f.path("/clones"), body, "", 400)
	accepted := f.call("POST", f.path("/clones"), body, "clone-1", 202)
	oid := accepted.S("operationId")
	if _, e := uuid.Parse(oid); e != nil || accepted.S("requestId") != body.S("requestId") || accepted.S("repository") != body.S("repository") || accepted.S("branch") != "main" || accepted.O("state").S("kind") != "pending" || accepted["executionId"] != nil || accepted["nodeId"] != nil {
		t.Fatal("acceptance view", accepted)
	}
	select {
	case signal := <-signals:
		if signal.Kind != core.SignalWorkAvailable || signal.OperationID != oid {
			t.Fatal("work signal", signal)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no work signal after acceptance")
	}

	// The request identity is durable across keys; a changed input under it conflicts, and the
	// idempotency key itself still rejects a changed body. Neither repeat signals new work.
	if again := f.call("POST", f.path("/clones"), body, "clone-2", 202); again.S("operationId") != oid {
		t.Fatal("request identity created another request", again)
	}
	f.call("POST", f.path("/clones"), core.Object{"requestId": body.S("requestId"), "repository": body.S("repository"), "branch": "other"}, "clone-3", 409)
	f.call("POST", f.path("/clones"), core.Object{"requestId": uuid.NewString(), "repository": body.S("repository"), "branch": "main"}, "clone-1", 409)
	select {
	case signal := <-signals:
		t.Fatal("repeat signaled work", signal)
	default:
	}

	// Cloud refuses what no Controller could dispatch, so nothing can sit queued forever.
	for i, bad := range []core.Object{
		{"requestId": uuid.NewString(), "repository": "file:///srv/repo.git", "branch": "main"},
		{"requestId": uuid.NewString(), "repository": "https://user@example.invalid/repo.git", "branch": "main"},
		{"requestId": uuid.NewString(), "repository": "ssh://git:secret@example.invalid/repo.git", "branch": "main"},
		{"requestId": uuid.NewString(), "repository": "https://example.invalid/", "branch": "main"},
		{"requestId": uuid.NewString(), "repository": "https://example.invalid/repo.git?x=1", "branch": "main"},
		{"requestId": uuid.NewString(), "repository": body.S("repository"), "branch": "HEAD"},
		{"requestId": uuid.NewString(), "repository": body.S("repository"), "branch": "refs/heads/main"},
		{"requestId": uuid.NewString(), "repository": body.S("repository"), "branch": "feature..x"},
		{"requestId": uuid.NewString(), "repository": body.S("repository"), "branch": "feature.lock"},
		{"requestId": "", "repository": body.S("repository"), "branch": "main"},
	} {
		f.call("POST", f.path("/clones"), bad, "bad-"+strconv.Itoa(i), 400)
	}
	// The branch reaches Git verbatim: whitespace that validRef would trim, and inner tabs or control
	// characters, are refused at acceptance instead of blocking the Controller's queue head.
	for i, branch := range []string{" main", "main ", "main\n", "ma\tin", "ma\x7fin", " main"} {
		bad := core.Object{"requestId": uuid.NewString(), "repository": body.S("repository"), "branch": branch}
		if fault := f.call("POST", f.path("/clones"), bad, "bad-branch-"+strconv.Itoa(i), 400); fault.S("code") != "invalid_ref" {
			t.Fatalf("branch %q: want invalid_ref, got %v", branch, fault)
		}
	}

	// Reads: the list and the single view agree with the accepted view; unknown ids are absent.
	list := f.call("GET", f.path("/clones"), nil, "", 200)
	if items := list["items"].([]any); len(items) != 1 || core.Object(items[0].(map[string]any)).S("operationId") != oid {
		t.Fatal("listing", list)
	}
	if one := f.clone(oid); text(one) != text(accepted) {
		t.Fatal("single view differs from acceptance", one, accepted)
	}
	f.call("GET", f.path("/clones/"+uuid.NewString()), nil, "", 404)
	f.call("GET", f.path("/clones/not-a-uuid"), nil, "", 404)

	// Another member of the tenant sees nothing: visibility follows the submitting user.
	f.user.Subject = "member"
	member := f.call("GET", "/api/v1/me", nil, "", 200)
	f.user.Subject = "alice"
	f.call("PUT", f.path("/members/"+member.S("id")), core.Object{"role": "member", "status": "active", "version": 0}, "", 200)
	f.user.Subject = "member"
	if others := f.call("GET", f.path("/clones"), nil, "", 200); len(others["items"].([]any)) != 0 {
		t.Fatal("clone visible to another member", others)
	}
	f.call("GET", f.path("/clones/"+oid), nil, "", 404)
	f.user.Subject = "alice"

	// A Controller claims, registers the dispatch and takes over the Node result; the public view
	// reflects each durable fact without exposing the receipt.
	if claimed := f.control("clone_claim", "", core.Object{}); claimed.O("request").S("id") != oid {
		t.Fatal("claim", claimed)
	}
	input := core.Object{"kind": "clone", "repositoryUrl": body.S("repository"), "branch": "main"}
	f.control("clone_dispatch", uuid.NewString(), core.Object{"operationId": oid, "executionId": "exec-1", "nodeId": "node-a", "input": input})
	dispatched := f.clone(oid)
	if dispatched.S("executionId") != "exec-1" || dispatched.S("nodeId") != "node-a" || dispatched.O("state").S("kind") != "pending" {
		t.Fatal("dispatched view", dispatched)
	}
	node := core.Object{"nodeId": "node-a", "nodeIncarnationId": "incarnation-1"}
	f.control("clone_takeover", uuid.NewString(), core.Object{"operationId": oid, "executionId": "exec-1", "sequence": 0, "event": "e30=", "result": core.Object{"node": node, "outcome": "clone_ready", "path": "/srv/checkout", "commit": f.commit}})
	if done := f.clone(oid); text(done.O("state")) != text(core.Object{"kind": "succeeded", "path": "/srv/checkout", "commit": f.commit}) {
		t.Fatal("succeeded view", done)
	}

	// A failed execution surfaces the Controller's reason in the browser vocabulary.
	failing := f.call("POST", f.path("/clones"), core.Object{"requestId": uuid.NewString(), "repository": body.S("repository"), "branch": "missing"}, "clone-4", 202)
	fid := failing.S("operationId")
	f.control("clone_dispatch", uuid.NewString(), core.Object{"operationId": fid, "executionId": "exec-2", "nodeId": "node-a", "input": core.Object{"kind": "clone", "repositoryUrl": body.S("repository"), "branch": "missing"}})
	f.control("clone_takeover", uuid.NewString(), core.Object{"operationId": fid, "executionId": "exec-2", "sequence": 0, "event": "e30=", "result": core.Object{"node": node, "outcome": "clone_failed", "reason": "CLONE_FAILURE_REASON_BRANCH_NOT_FOUND", "retainedPath": "/srv/retained"}})
	if failed := f.clone(fid); text(failed.O("state")) != text(core.Object{"kind": "failed", "reason": "branchNotFound", "retainedPath": "/srv/retained"}) {
		t.Fatal("failed view", failed)
	}
	// An attempt terminated before Git's own verdict surfaces as interrupted, keeping its directory.
	cut := f.call("POST", f.path("/clones"), core.Object{"requestId": uuid.NewString(), "repository": body.S("repository"), "branch": "main"}, "clone-5", 202)
	cid := cut.S("operationId")
	f.control("clone_dispatch", uuid.NewString(), core.Object{"operationId": cid, "executionId": "exec-3", "nodeId": "node-a", "input": input})
	f.control("clone_takeover", uuid.NewString(), core.Object{"operationId": cid, "executionId": "exec-3", "sequence": 0, "event": "e30=", "result": core.Object{"node": node, "outcome": "clone_failed", "reason": "CLONE_FAILURE_REASON_INTERRUPTED", "retainedPath": "/srv/cut"}})
	if interrupted := f.clone(cid); text(interrupted.O("state")) != text(core.Object{"kind": "failed", "reason": "interrupted", "retainedPath": "/srv/cut"}) {
		t.Fatal("interrupted view", interrupted)
	}
	if all := f.call("GET", f.path("/clones?limit=10"), nil, "", 200); len(all["items"].([]any)) != 3 {
		t.Fatal("listing after completion", all)
	}
}
