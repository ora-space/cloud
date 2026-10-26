package integration

import (
	"testing"

	"github.com/wanglongan587/cloud/internal/core"
	"github.com/wanglongan587/cloud/internal/workflowasset"
)

// reservedVariables is the author declaration the publish gate refuses: a Start variable under the
// name the platform owns in the `@` form.
func reservedVariables() []any {
	return []any{
		core.Object{"name": "snapshotId", "displayName": "Release version", "fieldType": "text-input", "valueType": "string"},
	}
}

// TestWorkflowReservedFieldIsRefusedOnPublishAndImport covers the two freezing entries
// (20261021-snapshot-id-is-a-reserved-field.md D1): publishing and workflow asset import share the
// graph gate, so the same declaration is refused with the same code and the same location, and
// neither leaves a snapshot behind. Saving the draft, and a `launchFields` withdrawal that mentions
// the name without declaring it, are not refusals.
func TestWorkflowReservedFieldIsRefusedOnPublishAndImport(t *testing.T) {
	f := setup(t)

	wid := createWorkflow(t, f, "Reserved Start", "reserved-start", reservedVariables())
	refused := f.call("POST", f.path("/workflows/"+wid+"/publish"), core.Object{}, "reserved-start-pub", 400)
	if got := problems(t, refused); len(got) != 1 || got[0].S("code") != "reserved_field_name" || got[0].S("nodeId") != "start-1" || got[0].S("field") != "snapshotId" {
		t.Fatalf("the Start declaration was not refused by name: %v", got)
	}

	globalGraph := startInputGraph(nil)
	globalGraph["globalVariables"] = []any{core.Object{"name": "snapshotId", "valueType": "string", "value": "v1"}}
	global := f.call("POST", f.path("/workflows"), core.Object{
		"name": "Reserved Global", "graph": globalGraph,
	}, "reserved-global-save", 200).O("resource")
	globalRefused := f.call("POST", f.path("/workflows/"+global.S("id")+"/publish"), core.Object{}, "reserved-global-pub", 400)
	if got := problems(t, globalRefused); len(got) != 1 || got[0].S("code") != "reserved_field_name" || got[0].S("name") != "snapshotId" {
		t.Fatalf("the global declaration was not refused by name: %v", got)
	}

	// Withdrawing the platform's selector is a request about the platform's own field, not a claim on
	// the name, so it publishes like any graph that declares nothing.
	withdrawn := createLaunchWorkflow(t, f, "Withdrawn", "reserved-withdrawn", nil, []any{
		core.Object{"key": "snapshotId", "enabled": false},
	})
	f.call("POST", f.path("/workflows/"+withdrawn+"/publish"), core.Object{}, "reserved-withdrawn-pub", 200)

	// The import entry freezes documents through the same gate. A package with one good document and
	// the two declarations above reports each failed path on its own and still imports the good one.
	repo := workflowMarketplace(t, f.root)
	document := func(name, node, extra string) string {
		if extra != "" {
			extra = "," + extra
		}
		return `{"name":"` + name + `",` + node + extra + `}`
	}
	pkg := workflowPackage(t, map[string]string{
		"start.json": document("Reserved Start Kit",
			`"nodes":[{"id":"start","data":{"kind":"start","inputVariables":[{"name":"snapshotId","valueType":"string"}]}}],"edges":[]`, ""),
		"global.json": document("Reserved Global Kit",
			`"nodes":[{"id":"start","data":{"kind":"start"}}],"edges":[]`,
			`"globalVariables":[{"name":"snapshotId","valueType":"string","value":"v1"}]`),
		"fine.json": executableDocument("Fine Kit", ""),
	})
	packages := newWorkflowPackages(t, map[string][]byte{"/packages/reserved-1.0.0.orax": pkg})
	publishWorkflowRelease(t, repo, "reserved-kit", "1.0.0", packages.server.URL+"/packages/reserved-1.0.0.orax", pkg)
	f.syncMarketplace(t, repo)
	f.store.WorkflowAssets = workflowasset.NewFetcher(nil, packages.server.Client().Transport)
	sid := f.defaultSpaceID()
	f.installWorkflowPlugin(t, sid, "official/reserved-kit", "")
	must(t, f.importWorkflowAssets(t))

	documents := assetDocuments(f.assetImport(t, sid, "official/reserved-kit"))
	if got := documents["assets/workflows/fine.json"].S("outcome"); got != "imported" {
		t.Fatalf("a legal document was not imported: %v", documents["assets/workflows/fine.json"])
	}
	for path, want := range map[string]core.Object{
		"assets/workflows/start.json":  {"code": "reserved_field_name", "nodeId": "start", "field": "snapshotId"},
		"assets/workflows/global.json": {"code": "reserved_field_name", "name": "snapshotId"},
	} {
		imported := documents[path]
		if imported.S("outcome") != "failed" || imported.S("failureCode") != "document_not_executable_graph" {
			t.Fatalf("%s = %v, want document_not_executable_graph", path, imported)
		}
		raw, _ := imported.O("failureDetail")["problems"].([]any)
		if len(raw) != 1 {
			t.Fatalf("%s problems = %v, want the one reserved declaration", path, raw)
		}
		problem, _ := raw[0].(map[string]any)
		if got := core.Object(problem); got.S("code") != want.S("code") || got.S("nodeId") != want.S("nodeId") || got.S("name") != want.S("name") || got.S("field") != want.S("field") {
			t.Fatalf("%s problem = %v, want %v", path, got, want)
		}
	}

	// Neither entry froze a refused declaration, and the two successful paths each froze exactly one.
	if n := f.scalar("SELECT count(*) FROM workflow_snapshots WHERE workflow_id=$1", wid); n != 0 {
		t.Fatalf("a refused publish left %d snapshots", n)
	}
	if n := f.scalar("SELECT count(*) FROM workflow_snapshots WHERE workflow_id=$1", global.S("id")); n != 0 {
		t.Fatalf("a refused global left %d snapshots", n)
	}
	if n := f.scalar("SELECT count(*) FROM workflow_snapshots WHERE workflow_id IN (SELECT id FROM workflows WHERE name IN ('Reserved Start Kit','Reserved Global Kit'))"); n != 0 {
		t.Fatalf("refused imported documents left %d snapshots", n)
	}
	// Count per workflow rather than across the schema: a later fixture in this schema must not make
	// this assertion fail for a reason that has nothing to do with the gate.
	if n := f.scalar("SELECT count(*) FROM workflow_snapshots WHERE workflow_id=$1", withdrawn); n != 1 {
		t.Fatalf("the withdrawn-selector workflow froze %d versions, want 1", n)
	}
	if n := f.scalar("SELECT count(*) FROM workflow_snapshots ws JOIN workflows w ON w.id=ws.workflow_id WHERE w.name='Fine Kit'"); n != 1 {
		t.Fatalf("the imported workflow froze %d versions, want 1", n)
	}
}

// TestWorkflowReservedFieldWithholdsStockVersionsAndRefusesThem covers the read side of the reserved
// field (20261021-snapshot-id-is-a-reserved-field.md D2/D3): a version published before the gate that
// declares `snapshotId` is never offered by the selector, the selector explains why, and a confirm
// that resolves to it — by cursor or by falling back to the newest version — is refused instead of
// executing something the user did not choose or silently substituting another version.
func TestWorkflowReservedFieldWithholdsStockVersionsAndRefusesThem(t *testing.T) {
	f := setup(t)
	// The live draft leaves `snapshotId` to the platform, which is what makes a submitted value under
	// that key a version cursor rather than an author variable.
	wid := createWorkflow(t, f, "Stock Versions", "reserved-stock", notesVariables())
	selectable := publishWorkflow(t, f, wid, "reserved-stock-pub-1")
	// The stock version is newer, so it is what "no cursor" resolves to.
	stock := seedLegacySnapshot(t, f, wid, 2, startInputGraph(append(reservedVariables(), notesVariables()...)))

	project := f.create("reserved-stock-project").O("resource")
	iss := f.call("POST", f.path("/issues"), core.Object{
		"title": "Run a stock version", "projectRef": project.S("id"),
	}, "reserved-stock-issue", 200).O("resource")

	fields := formFields(t, f.call("GET", f.path("/collaboration/forms/"+wid+"?issueId="+iss.S("id")), nil, "", 200))
	version := fields["snapshotId"]
	if version.S("type") != "select" || version["defaultValue"] != selectable.S("id") {
		t.Fatalf("the selector did not open on the newest selectable version: %v", version)
	}
	options, _ := version["options"].([]any)
	if len(options) != 1 || core.Object(options[0].(map[string]any)).S("value") != selectable.S("id") {
		t.Fatalf("the withheld version was offered: %v", options)
	}
	if version.S("description") == "" {
		t.Fatalf("the selector did not explain the withheld version: %v", version)
	}

	// Without an issue the snapshot form is the workflow's own schema, so it still projects the author's
	// declaration. Scoped to an issue it is the `@` form, where the key is always the platform's: the
	// declaration is not rendered and the selector is, even though the version being described is the
	// withheld one.
	bare := formFields(t, f.call("GET", f.path("/workflows/"+wid+"/snapshots/"+stock.S("id")+"/form"), nil, "", 200))
	if got := bare["snapshotId"]; got.S("type") != "text" || got.S("label") != "Release version" {
		t.Fatalf("the version's own schema lost the author's declaration: %v", got)
	}
	own := formFields(t, f.call("GET", f.path("/workflows/"+wid+"/snapshots/"+stock.S("id")+"/form?issueId="+iss.S("id")), nil, "", 200))
	if got := own["snapshotId"]; got.S("type") != "select" {
		t.Fatalf("the issue-scoped version form projected the author's declaration: %v", got)
	}

	it := workflowComment(t, f, wid, iss.S("id"), "reserved-stock-1", "run it")
	base := core.Object{"repository": project.S("repositoryUrl"), "branch": "main"}
	refusal := func(key string, values core.Object) core.Object {
		t.Helper()
		refused := f.call("POST", confirmPath(f, iss.S("id"), it.S("id")), core.Object{"values": values}, key, 409)
		if refused.S("code") != "workflow_version_reserved_field" || refused.O("params").S("snapshotId") != stock.S("id") || refused.O("params").S("field") != "snapshotId" {
			t.Fatalf("refusal = %v, want workflow_version_reserved_field for %s", refused, stock.S("id"))
		}
		return refused
	}
	// No cursor: the newest published version is the stock one, and it is refused rather than executed.
	refusal("reserved-stock-c1", core.Object{"repository": base.S("repository"), "branch": "main"})
	// An explicit cursor to it is the same refusal, not a silent fallback to another version.
	refusal("reserved-stock-c2", core.Object{"repository": base.S("repository"), "branch": "main", "snapshotId": stock.S("id")})
	if n := f.scalar("SELECT count(*) FROM issue_runs WHERE issue_id=$1", iss.S("id")); n != 0 {
		t.Fatalf("a refused confirm created %d runs", n)
	}
	if n := f.scalar("SELECT count(*) FROM issue_interactions WHERE id=$1 AND run_id IS NOT NULL", it.S("id")); n != 0 {
		t.Fatal("a refused confirm claimed the interaction")
	}

	// The selectable version is the one the form offers, and choosing it runs exactly it.
	created := f.call("POST", confirmPath(f, iss.S("id"), it.S("id")), core.Object{"values": core.Object{
		"repository": base.S("repository"), "branch": "main", "snapshotId": selectable.S("id"),
	}}, "reserved-stock-c3", 200).O("resource")
	run := f.call("GET", f.path("/issues/"+iss.S("id")+"/runs/"+created.S("id")), nil, "", 200)
	if got := run.O("input").S("workflowSnapshotId"); got != selectable.S("id") {
		t.Fatalf("run snapshot = %q, want the chosen selectable version %q", got, selectable.S("id"))
	}
}

// TestWorkflowReservedFieldStaysThePlatformsWhenTheDraftDeclaresIt pins the A′ half of D3: the `@`
// form and the confirm never hand the key back to a live draft that still declares it. The declaration
// is not rendered, a submitted value under the key is the version cursor, and a resolved version whose
// own graph declares the key is refused — whether the submission named it or fell back to the newest
// version — so the version the user sees selected is always the version that would run.
func TestWorkflowReservedFieldStaysThePlatformsWhenTheDraftDeclaresIt(t *testing.T) {
	f := setup(t)
	// The live draft is the legacy shape the publish gate no longer creates, saved directly. Both
	// versions are seeded for the same reason: one leaves the key alone and stays selectable, the newer
	// one claims it and is withheld.
	wid := createWorkflow(t, f, "Draft Declares It", "reserved-owned", append(reservedVariables(), notesVariables()...))
	selectable := seedLegacySnapshot(t, f, wid, 1, startInputGraph(notesVariables()))
	stock := seedLegacySnapshot(t, f, wid, 2, startInputGraph(append(reservedVariables(), notesVariables()...)))

	project := f.create("reserved-owned-project").O("resource")
	iss := f.call("POST", f.path("/issues"), core.Object{
		"title": "Own the key", "projectRef": project.S("id"),
	}, "reserved-owned-issue", 200).O("resource")

	// The form does not show the author's control: the key is a select over the selectable versions,
	// and it opens on the newest one — not on the stock version that would be refused.
	fields := formFields(t, f.call("GET", f.path("/collaboration/forms/"+wid+"?issueId="+iss.S("id")), nil, "", 200))
	version := fields["snapshotId"]
	if version.S("type") != "select" || version.S("label") != "运行版本" {
		t.Fatalf("the live draft's declaration was projected instead of the platform's cursor: %v", version)
	}
	options, _ := version["options"].([]any)
	if len(options) != 1 || core.Object(options[0].(map[string]any)).S("value") != selectable.S("id") {
		t.Fatalf("the form offered the withheld version or lost the selectable one: %v", options)
	}
	if version["defaultValue"] != selectable.S("id") {
		t.Fatalf("the form opened on %v, want the newest selectable version %s", version["defaultValue"], selectable.S("id"))
	}

	it := workflowComment(t, f, wid, iss.S("id"), "reserved-owned-1", "run it")
	base := core.Object{"repository": project.S("repositoryUrl"), "branch": "main"}
	refusal := func(key string, values core.Object) {
		t.Helper()
		refused := f.call("POST", confirmPath(f, iss.S("id"), it.S("id")), core.Object{"values": values}, key, 409)
		if refused.S("code") != "workflow_version_reserved_field" || refused.O("params").S("snapshotId") != stock.S("id") {
			t.Fatalf("refusal = %v, want workflow_version_reserved_field for %s", refused, stock.S("id"))
		}
	}
	// No cursor: the newest version is the stock one, and the live draft's declaration does not change
	// that the key belongs to the platform.
	refusal("reserved-owned-c1", base)
	// A cursor to the stock version is the same refusal, not a value the workflow receives instead.
	refusal("reserved-owned-c2", core.Object{"repository": base.S("repository"), "branch": "main", "snapshotId": stock.S("id")})

	// The selectable version is the one the form offered, and choosing it runs exactly it.
	created := f.call("POST", confirmPath(f, iss.S("id"), it.S("id")), core.Object{"values": core.Object{
		"repository": base.S("repository"), "branch": "main", "snapshotId": selectable.S("id"),
	}}, "reserved-owned-c3", 200).O("resource")
	run := f.call("GET", f.path("/issues/"+iss.S("id")+"/runs/"+created.S("id")), nil, "", 200)
	if got := run.O("input").S("workflowSnapshotId"); got != selectable.S("id") {
		t.Fatalf("run snapshot = %q, want the chosen selectable version %q", got, selectable.S("id"))
	}
}

// TestWorkflowReservedFieldSelectorCoversEveryDeclarationShape pins the SQL half of D2 against every
// shape the Go predicate recognizes: a global declaration withholds a version like a Start input does,
// a name that only looks like the key is an ordinary variable and stays selectable (both sides match
// exactly), and when every version is withheld the form carries no selector at all — while a confirm
// with no cursor still resolves to that version and is refused by the reserved-field rule.
func TestWorkflowReservedFieldSelectorCoversEveryDeclarationShape(t *testing.T) {
	f := setup(t)
	wid := createWorkflow(t, f, "Selector Shapes", "reserved-shapes", notesVariables())
	selectable := publishWorkflow(t, f, wid, "reserved-shapes-pub-1")
	globalDeclared := startInputGraph(nil)
	globalDeclared["globalVariables"] = []any{core.Object{"name": "snapshotId", "valueType": "string", "value": "v1"}}
	seedLegacySnapshot(t, f, wid, 2, globalDeclared)
	padded := seedLegacySnapshot(t, f, wid, 3, startInputGraph([]any{
		core.Object{"name": " snapshotId ", "displayName": "Padded", "fieldType": "text-input", "valueType": "string"},
	}))

	project := f.create("reserved-shapes-project").O("resource")
	iss := f.call("POST", f.path("/issues"), core.Object{
		"title": "Selector shapes", "projectRef": project.S("id"),
	}, "reserved-shapes-issue", 200).O("resource")

	version := formFields(t, f.call("GET", f.path("/collaboration/forms/"+wid+"?issueId="+iss.S("id")), nil, "", 200))["snapshotId"]
	if version.S("type") != "select" || version["defaultValue"] != padded.S("id") {
		t.Fatalf("the selector did not open on the newest selectable version: %v", version)
	}
	offered := map[string]bool{}
	for _, raw := range version["options"].([]any) {
		offered[core.Object(raw.(map[string]any)).S("value")] = true
	}
	if !offered[selectable.S("id")] || !offered[padded.S("id")] || len(offered) != 2 {
		t.Fatalf("the selector offered %v, want the published version and the padded-name version", offered)
	}
	if version.S("description") == "" {
		t.Fatalf("the selector did not explain the globally withheld version: %v", version)
	}

	// Every version withheld: the form has no selector, and a confirm without a cursor still resolves
	// to the withheld version and is refused rather than executed or rerouted.
	withheldID := createWorkflow(t, f, "Everything Withheld", "reserved-all-withheld", notesVariables())
	only := seedLegacySnapshot(t, f, withheldID, 1, startInputGraph(append(reservedVariables(), notesVariables()...)))
	withheldIssue := f.call("POST", f.path("/issues"), core.Object{
		"title": "Everything withheld", "projectRef": project.S("id"),
	}, "reserved-all-withheld-issue", 200).O("resource")
	withheldFields := formFields(t, f.call("GET", f.path("/collaboration/forms/"+withheldID+"?issueId="+withheldIssue.S("id")), nil, "", 200))
	if selector, ok := withheldFields["snapshotId"]; ok {
		t.Fatalf("a workflow with no selectable version still projected a selector: %v", selector)
	}
	withheldIt := workflowComment(t, f, withheldID, withheldIssue.S("id"), "reserved-all-withheld-1", "run it")
	refused := f.call("POST", confirmPath(f, withheldIssue.S("id"), withheldIt.S("id")), core.Object{"values": core.Object{
		"repository": project.S("repositoryUrl"), "branch": "main",
	}}, "reserved-all-withheld-c1", 409)
	if refused.S("code") != "workflow_version_reserved_field" || refused.O("params").S("snapshotId") != only.S("id") {
		t.Fatalf("refusal = %v, want workflow_version_reserved_field for %s", refused, only.S("id"))
	}
}

// TestWorkflowReservedFieldRefusalOutranksTheExecutorSlotGate covers D3's ordering. The stock version
// here declares the key and references an executor slot the initiator has not configured; the answer
// must still be the version refusal, because no slot mapping can make a version that may never run
// run. A control workflow with the same graph minus the declaration proves the slot gate is really
// unsatisfied in this environment, so the ordering — not a fixture where slots resolve — decided it.
func TestWorkflowReservedFieldRefusalOutranksTheExecutorSlotGate(t *testing.T) {
	f := setup(t)
	wid := createWorkflow(t, f, "Reserved Before Slots", "reserved-before-slots", notesVariables())
	graph := agentRunGraph("", false, nil)
	start := graph["nodes"].([]any)[0].(core.Object)
	start.O("data")["inputVariables"] = reservedVariables()
	stock := seedLegacySnapshot(t, f, wid, 1, graph)

	project := f.create("reserved-before-slots-project").O("resource")
	iss := f.call("POST", f.path("/issues"), core.Object{
		"title": "Reserved beats slots", "projectRef": project.S("id"),
	}, "reserved-before-slots-issue", 200).O("resource")
	it := workflowComment(t, f, wid, iss.S("id"), "reserved-before-slots-1", "run it")
	base := core.Object{"repository": project.S("repositoryUrl"), "branch": "main"}
	refused := f.call("POST", confirmPath(f, iss.S("id"), it.S("id")), core.Object{"values": base}, "reserved-before-slots-c1", 409)
	if refused.S("code") != "workflow_version_reserved_field" || refused.O("params").S("snapshotId") != stock.S("id") {
		t.Fatalf("refusal = %v, want workflow_version_reserved_field for %s", refused, stock.S("id"))
	}

	control := createWorkflow(t, f, "Slots Only", "slots-only", notesVariables())
	seedLegacySnapshot(t, f, control, 1, agentRunGraph("", false, nil))
	controlIssue := f.call("POST", f.path("/issues"), core.Object{
		"title": "Slots only", "projectRef": project.S("id"),
	}, "slots-only-issue", 200).O("resource")
	controlIt := workflowComment(t, f, control, controlIssue.S("id"), "slots-only-1", "run it")
	slots := f.call("POST", confirmPath(f, controlIssue.S("id"), controlIt.S("id")), core.Object{"values": base}, "slots-only-c1", 409)
	if slots.S("code") != "workflow_executor_unconfigured" {
		t.Fatalf("control refusal = %v, want workflow_executor_unconfigured", slots)
	}
}

// TestWorkflowReservedFieldAssistUsesTheSelectorDefault covers the assist side of D2. Assist answers
// for the form the user is filling, and that form opens on the newest *selectable* version; the newest
// published version can be a withheld stock one with a different field set, so resolving it instead
// would refuse exactly the fields the visible form offers. When every version is withheld the form has
// no selector and is the live draft, which assist has to follow as well.
func TestWorkflowReservedFieldAssistUsesTheSelectorDefault(t *testing.T) {
	f := setup(t)
	// The selectable version is the form's default and declares `notes`. The newer withheld version
	// declares the reserved key and no `notes`, so the two schemas disagree about a key the user sees.
	wid := createWorkflow(t, f, "Assist Default", "reserved-assist", notesVariables())
	selectable := publishWorkflow(t, f, wid, "reserved-assist-pub-1")
	seedLegacySnapshot(t, f, wid, 2, startInputGraph(reservedVariables()))

	project := f.create("reserved-assist-project").O("resource")
	iss := f.call("POST", f.path("/issues"), core.Object{
		"title": "Fill the shown form", "projectRef": project.S("id"),
	}, "reserved-assist-issue", 200).O("resource")
	fields := formFields(t, f.call("GET", f.path("/collaboration/forms/"+wid+"?issueId="+iss.S("id")), nil, "", 200))
	if got := fields["snapshotId"]["defaultValue"]; got != selectable.S("id") {
		t.Fatalf("the form did not open on the selectable version: %v", got)
	}
	if _, ok := fields["notes"]; !ok {
		t.Fatalf("the form did not render the selectable version's field: %v", fields)
	}
	// This submission is exactly what that form shows. The newer version does not declare `notes`,
	// so judging the submission against it would make a legal form value an unknown-key 400.
	f.call("POST", assistPath(f, iss.S("id")), workflowAssistBody(wid, core.Object{
		"repository": project.S("repositoryUrl"), "branch": "main", "notes": "release notes",
	}), "reserved-assist-a1", 200)

	// Every published version withheld: the form has no selector and is the live draft, which still
	// declares `notes`. Assist has to answer for that draft, not for a version the user cannot choose.
	allWid := createWorkflow(t, f, "Assist All Withheld", "reserved-assist-all", notesVariables())
	seedLegacySnapshot(t, f, allWid, 1, startInputGraph(reservedVariables()))
	allIss := f.call("POST", f.path("/issues"), core.Object{
		"title": "Fill the draft form", "projectRef": project.S("id"),
	}, "reserved-assist-all-issue", 200).O("resource")
	allFields := formFields(t, f.call("GET", f.path("/collaboration/forms/"+allWid+"?issueId="+allIss.S("id")), nil, "", 200))
	if selector, ok := allFields["snapshotId"]; ok {
		t.Fatalf("a wholly withheld workflow still projected a selector: %v", selector)
	}
	if _, ok := allFields["notes"]; !ok {
		t.Fatalf("the draft form did not render its own field: %v", allFields)
	}
	f.call("POST", assistPath(f, allIss.S("id")), workflowAssistBody(allWid, core.Object{
		"repository": project.S("repositoryUrl"), "branch": "main", "notes": "draft notes",
	}), "reserved-assist-all-a1", 200)
}
