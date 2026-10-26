package integration

import (
	"testing"

	"github.com/wanglongan587/cloud/internal/core"
)

// Issue invocations freeze the confirming actor, not the Issue/workflow author. The default fixture
// simulator deliberately remains installed: neither entry may use it to skip executor freezing or
// fabricate a completed WorkflowRun.
func TestWorkflowInvocationFreezesVerifiedIdentity(t *testing.T) {
	for _, entry := range []string{"confirm", "manual"} {
		t.Run(entry, func(t *testing.T) {
			f := setup(t)
			if f.store.Simulator == nil {
				t.Fatal("this scenario requires the direct-run simulator to remain installed")
			}
			pid := runProject(t, f, "inv-identity-project")
			actor, actorID := f.addUser(t, "inv-actor", "Invocation actor")
			author := f.user
			f.user = actor
			connection := seedPersonalModel(t, f)
			f.call("PUT", "/api/v1/me/executor-slot-mappings/strong", core.Object{
				"agentCli": "official/ora-space.opencode", "connectionId": connection.S("id"),
				"modelId": fixtureModelID, "version": 0,
			}, "", 200)
			f.user = author
			graph := agentRunGraph("strong", false, []any{core.Object{"name": "strong", "description": "Review"}})
			wid := f.call("POST", f.path("/workflows"), core.Object{
				"name": "Identity workflow", "graph": graph,
			}, "inv-identity-wf", 200).O("resource").S("id")
			publishWorkflow(t, f, wid, "inv-identity-publish")
			_, err := f.store.Pool.Exec(`INSERT INTO space_plugins(space_id,tenant_id,source_namespace,identifier,desired_state,desired_version,observed_state)
				VALUES($1,$2,'official','ora-space.opencode','installed','1.7.0','pending')`, f.defaultSpaceID(), f.tid)
			must(t, err)
			iid := f.call("POST", f.path("/issues"), core.Object{
				"title": "Another member starts this", "projectRef": pid,
			}, "inv-identity-issue", 200).O("resource").S("id")
			var run core.Object
			if entry == "confirm" {
				it := workflowComment(t, f, wid, iid, "inv-identity-comment", "review")
				run = callAs(t, f, actor, "POST", confirmPath(f, iid, it.S("id")), "inv-identity-confirm", core.Object{
					"values": core.Object{"repository": "ora-space/cloud", "branch": "main"},
				}, 200).O("resource")
			} else {
				run = callAs(t, f, actor, "POST", f.path("/issues/"+iid+"/runs"), "inv-identity-manual", core.Object{
					"executorType": "workflow", "executorId": wid, "input": core.Object{},
				}, 200).O("resource")
			}
			rid := run.S("workflowInvocationRef")
			if rid == "" || run.S("status") != "queued" {
				t.Fatalf("invocation did not return a queued linked IssueRun: %v", run)
			}
			assertFrozen := func() {
				t.Helper()
				var creator, project, plugin, status string
				must(t, f.store.Pool.QueryRow(`SELECT created_by,project_id,plugin_version,status FROM workflow_runs WHERE id=$1`, rid).
					Scan(&creator, &project, &plugin, &status))
				if creator != actorID || creator == f.uid || project != pid || plugin != "1.7.0" || status != "pending" {
					t.Fatalf("frozen identity creator=%q project=%q plugin=%q status=%q; want verified actor, Issue project, 1.7.0, pending", creator, project, plugin, status)
				}
				if n := f.scalar("SELECT count(*) FROM run_model_bindings WHERE workflow_run_id=$1", rid); n != 1 {
					t.Fatalf("invocation froze %d executor bindings, want exactly one", n)
				}
				var slot, userID, connectionID, credentialID, name, protocol, baseURL, authMode, modelID string
				var connectionVersion int64
				must(t, f.store.Pool.QueryRow(`SELECT slot_name,user_id,connection_id,connection_version,credential_id,connection_name,protocol,base_url,auth_mode,model->>'id'
					FROM run_model_bindings WHERE workflow_run_id=$1`, rid).
					Scan(&slot, &userID, &connectionID, &connectionVersion, &credentialID, &name, &protocol, &baseURL, &authMode, &modelID))
				var expectedCredential string
				must(t, f.store.Pool.QueryRow("SELECT credential_id FROM personal_model_connections WHERE id=$1", connection.S("id")).Scan(&expectedCredential))
				if slot != "strong" || userID != actorID || connectionID != connection.S("id") || connectionVersion != connection.N("version") ||
					credentialID != expectedCredential || name != "Personal" || protocol != "openai-completions" ||
					baseURL != "https://models.example.invalid/v1" || authMode != "bearer" || modelID != fixtureModelID {
					t.Fatalf("frozen executor differs from actor's admitted configuration: slot=%q user=%q connection=%q version=%d name=%q protocol=%q baseURL=%q authMode=%q model=%q", slot, userID, connectionID, connectionVersion, name, protocol, baseURL, authMode, modelID)
				}
				if n := f.scalar(`SELECT count(*) FROM workflow_runs WHERE id=$1 AND started_at IS NULL AND finished_at IS NULL AND node_states='{}'::jsonb`, rid); n != 1 {
					t.Fatal("Issue invocation fabricated a simulator execution trace")
				}
			}
			assertFrozen()
			// Later mutable configuration is not the invocation's execution identity.
			_, err = f.store.Pool.Exec("UPDATE space_plugins SET desired_version='2.0.0' WHERE space_id=$1 AND identifier='ora-space.opencode'", f.defaultSpaceID())
			must(t, err)
			_, err = f.store.Pool.Exec("UPDATE personal_model_connections SET name='Changed after creation',version=version+1 WHERE id=$1", connection.S("id"))
			must(t, err)
			assertFrozen()
		})
	}
}

func TestWorkflowInvocationWithoutIssueProjectRollsBack(t *testing.T) {
	for _, entry := range []string{"confirm", "manual"} {
		t.Run(entry, func(t *testing.T) {
			f := setup(t)
			wid := createWorkflow(t, f, "No Project workflow", "inv-no-project-wf", nil)
			publishWorkflow(t, f, wid, "inv-no-project-publish")
			iid := f.call("POST", f.path("/issues"), core.Object{"title": "No Project"}, "inv-no-project-issue", 200).O("resource").S("id")
			it := workflowComment(t, f, wid, iid, "inv-no-project-comment", "run")
			activities := f.scalar("SELECT count(*) FROM issue_activities WHERE issue_id=$1", iid)
			var refusal core.Object
			if entry == "confirm" {
				refusal = f.call("POST", confirmPath(f, iid, it.S("id")), core.Object{
					"values": core.Object{"repository": "ora-space/cloud", "branch": "main"},
				}, "inv-no-project-confirm", 409)
			} else {
				status, body := manualWorkflowRun(t, f, iid, wid, "inv-no-project-manual", core.Object{})
				if status != 409 {
					t.Fatalf("manual missing-project status=%d, want 409: %v", status, body)
				}
				refusal = body
			}
			if refusal.S("code") != "workflow_run_project_required" {
				t.Fatalf("missing-project code=%q, want workflow_run_project_required", refusal.S("code"))
			}
			assertUnclaimedWorkflowInvocation(t, f, iid, it.S("id"), activities)
		})
	}
}
