package integration

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/wanglongan587/cloud/internal/core"
)

// Both UI forms submit JSON text, while API callers may already supply native values. Admission
// must persist the same execution input and refuse bad values before claiming an interaction.
func TestWorkflowStructuredInputAcrossEntries(t *testing.T) {
	for _, entry := range []string{"direct", "manual-issue", "confirm"} {
		t.Run(entry, func(t *testing.T) {
			f := setup(t)
			wid := createWorkflow(t, f, "Structured input", "structured-wf", []any{
				core.Object{"name": "items", "valueType": "array[string]", "required": true},
				core.Object{"name": "settings", "valueType": "object"},
				core.Object{"name": "text", "valueType": "string"},
			})
			snapshot := publishWorkflow(t, f, wid, "structured-publish")
			pid := runProject(t, f, "structured-project")
			iid := f.call("POST", f.path("/issues"), core.Object{"title": "Structured start", "projectRef": pid}, "structured-issue", 200).O("resource").S("id")
			interaction := workflowComment(t, f, wid, iid, "structured-comment", "run")
			activities := f.scalar("SELECT count(*) FROM issue_activities WHERE issue_id=$1", iid)
			submit := func(values core.Object, key string, status int) core.Object {
				t.Helper()
				switch entry {
				case "confirm":
					values["repository"], values["branch"], values["snapshotId"] = "ora-space/cloud", "main", snapshot.S("id")
					return f.call("POST", confirmPath(f, iid, interaction.S("id")), core.Object{"values": values}, key, status)
				case "manual-issue":
					return f.call("POST", f.path("/issues/"+iid+"/runs"), core.Object{"executorType": "workflow", "executorId": wid, "input": values}, key, status)
				default:
					return f.call("POST", f.path("/workflows/"+wid+"/runs"), core.Object{"snapshotId": snapshot.S("id"), "projectId": pid, "input": values}, key, status)
				}
			}
			for i, invalid := range []any{`[`, `[] []`, `{}`, `null`, `"[]"`, `[1]`, core.Object{"a": 1}} {
				refusal := submit(core.Object{"items": invalid}, fmt.Sprintf("structured-invalid-%d", i), 400)
				if refusal.S("code") != "invalid_field_value" || refusal.O("params").S("field") != "items" {
					t.Fatalf("invalid input %v: %v", invalid, refusal)
				}
				assertUnclaimedWorkflowInvocation(t, f, iid, interaction.S("id"), activities)
			}
			values := core.Object{"items": `["甲","乙"]`, "settings": `{"enabled":true}`, "text": `["keep text"]`}
			run := submit(values, "structured-valid", 200).O("resource")
			rid := run.S("id")
			if entry != "direct" {
				rid = run.S("workflowInvocationRef")
			}
			persisted := f.call("GET", f.path("/workflows/"+wid+"/runs/"+rid), nil, "", 200).O("input")
			if !reflect.DeepEqual(persisted["items"], []any{"甲", "乙"}) || !persisted.O("settings").B("enabled") || persisted.S("text") != `["keep text"]` {
				t.Fatalf("execution input was not normalized: %v", persisted)
			}
			// Native API clients remain compatible through the direct-run boundary.
			if entry == "direct" {
				run = submit(core.Object{"items": []any{"甲", "乙"}, "settings": core.Object{"enabled": true}}, "structured-native", 200).O("resource")
				native := f.call("GET", f.path("/workflows/"+wid+"/runs/"+run.S("id")), nil, "", 200).O("input")
				if !reflect.DeepEqual(native["items"], persisted["items"]) || !reflect.DeepEqual(native["settings"], persisted["settings"]) {
					t.Fatalf("native and text input differ: native=%v text=%v", native, persisted)
				}
			}
		})
	}
}
