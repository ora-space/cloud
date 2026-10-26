package core

// enterPluginStep freezes the plugin set the Node execution must carry. An empty set means the step
// has nothing to install and the caller advances past it. Calling it again does not recompute the set.
func enterPluginStep(t *transaction, o Object) bool {
	o = t.one("SELECT * FROM operations WHERE id=$1", o.S("id"))
	require(o != nil, 404, "not_found")
	req := o.O("request")
	if _, frozen := req["plugins"]; frozen {
		return len(objectsOf(req["plugins"])) == 0
	}
	plugins, meta := pluginSnapshot(t, o)
	if plugins == nil {
		plugins = []Object{}
	}
	if meta == nil {
		meta = []Object{}
	}
	req["plugins"] = plugins
	req["pluginMeta"] = meta
	t.exec("UPDATE operations SET request=$2,version=version+1,updated_at=now() WHERE id=$1", o.S("id"), jsonText(req))
	state := "installing"
	if o.S("kind") == "remove_plugin" {
		state = "removing"
	}
	for _, item := range plugins {
		revision := pluginRevision(meta, item.S("pluginId"))
		ensurePluginInstance(t, o.S("workspaceId"), item.S("pluginId"), revision)
		pluginInstanceWriteback(t, o, item.S("pluginId"), state, "", nil, revision)
	}
	return len(plugins) == 0
}

func pluginSnapshot(t *transaction, o Object) (plugins, meta []Object) {
	switch o.S("kind") {
	case "install_plugin":
		release := o.O("request").O("release")
		item := pluginPayload(release)
		if item.S("pluginId") == "" {
			item["pluginId"] = o.O("request").S("pluginId")
		}
		return []Object{item}, []Object{{"pluginId": item.S("pluginId"), "desiredRevision": o.O("request").N("desiredRevision")}}
	case "remove_plugin":
		id, version := o.O("request").S("pluginId"), o.O("request").S("version")
		return []Object{{"pluginId": id, "version": version}}, []Object{{"pluginId": id, "desiredRevision": o.O("request").N("desiredRevision")}}
	default:
		rows := t.list(`SELECT sp.desired_revision,(sp.source_namespace||'/'||sp.identifier) AS plugin_id,sp.selected_release
			FROM space_plugins sp JOIN projects p ON p.space_id=sp.space_id
			WHERE p.id=$1 AND sp.desired_state='installed' ORDER BY sp.source_namespace,sp.identifier`, o.S("projectId"))
		for _, row := range rows {
			release := row.O("selectedRelease")
			if release.S("id") == "" {
				release["id"] = row.S("pluginId")
			}
			item := pluginPayload(release)
			plugins = append(plugins, item)
			meta = append(meta, Object{"pluginId": item.S("pluginId"), "desiredRevision": row.N("desiredRevision")})
		}
		plugins, meta = frozenAgentPluginSnapshot(t, o, plugins, meta)
		return frozenWorkflowRunPluginSnapshot(t, o, plugins, meta)
	}
}

// frozenWorkflowRunPluginSnapshot puts the Agent CLI package a WorkflowRun froze at creation in place of
// the Space's current selection for that plugin (D3: an in-flight upgrade must not change what an
// already-created run executes). The release is frozen on the run row in the same transaction that
// froze plugin_version, so a run whose Space moved on between creation and the Workspace's plugin step
// installs the version its own session will require instead of one the Node rejects as
// `agent_plugin_unavailable`. Every other plugin keeps the Space's current selection: the run freezes
// only the Agent CLI identity its session dispatches.
//
// It replaces, never appends. A plugin the Space no longer selects is not installed for a run created
// before the removal: reinstalling it would undo an administrator's decision for a run that has not
// started yet, and the start gate already reports that bounded failure as `agent_plugin_unavailable`.
// A run with no frozen release (one created before the release was frozen, or whose selection named
// another version) keeps the plain Space snapshot for the same reason it has no honest package to use.
func frozenWorkflowRunPluginSnapshot(t *transaction, operation Object, plugins, meta []Object) (frozenPlugins, frozenMeta []Object) {
	frozen := t.one(`SELECT r.plugin_release FROM workflow_runs r JOIN workspaces w ON w.workflow_run_id=r.id
		WHERE w.id=$1`, operation.S("workspaceId"))
	if frozen == nil || frozen["pluginRelease"] == nil {
		return plugins, meta
	}
	release := frozen.O("pluginRelease")
	if release.S("version") == "" {
		return plugins, meta
	}
	payload := pluginPayload(release)
	if payload.S("pluginId") == "" {
		payload["pluginId"] = opencodeAgentCLI
	}
	for i, plugin := range plugins {
		if plugin.S("pluginId") == payload.S("pluginId") {
			plugins[i] = payload
			return plugins, meta
		}
	}
	return plugins, meta
}

func pluginPayload(release Object) Object {
	item := Object{"pluginId": release.S("id"), "version": release.S("version")}
	if release.S("url") != "" {
		item["universal"] = Object{"url": release.S("url"), "sha256": release.S("sha256")}
	}
	if targets := objectsOf(release["targets"]); len(targets) > 0 {
		item["targets"] = targets
	}
	// The frozen declaration association travels with the plan so the Node can prove the package's
	// in-archive declaration bytes against it. Only the identity fields travel: the static copy
	// and its path are Cloud-side evidence the Node re-derives from the actual package.
	// An undeclared release carries no key at all: the Node reads a present association as a
	// declaration to prove, so an empty one would fail every undeclared install.
	if association, declared := PluginAgentConfiguration(release); declared {
		item["agentConfiguration"] = Object{
			"schemaVersion":     association.N("schemaVersion"),
			"declarationSha256": association.S("declarationSha256"),
		}
	}
	return item
}

// PluginAgentConfiguration returns the Agent declaration association a frozen release or plugin
// plan item carries, and whether it carries one at all. Only an object value counts: the catalog
// stores an undeclared listing's association as SQL NULL, which a frozen selection keeps as a JSON
// null, and older selections lack the key. Object.O cannot make this distinction because it
// returns an empty object for an absent field.
func PluginAgentConfiguration(item Object) (Object, bool) {
	switch v := item["agentConfiguration"].(type) {
	case map[string]any:
		return Object(v), true
	case Object:
		return v, true
	}
	return nil, false
}

func pluginRevision(meta []Object, pluginID string) int64 {
	for _, item := range meta {
		if item.S("pluginId") == pluginID {
			return item.N("desiredRevision")
		}
	}
	return 0
}

func ensurePluginInstance(t *transaction, workspaceID, pluginID string, revision int64) {
	namespace, identifier, ok := pluginIdentity(pluginID)
	if !ok {
		return
	}
	w := t.one("SELECT * FROM workspaces WHERE id=$1", workspaceID)
	if w == nil {
		return
	}
	t.exec(`INSERT INTO workspace_plugin_instances(workspace_id,tenant_id,owner_user_id,project_id,source_namespace,identifier,observed_state,desired_revision)
		VALUES($1,$2,$3,$4,$5,$6,'pending',$7)
		ON CONFLICT(workspace_id,source_namespace,identifier) DO UPDATE SET desired_revision=EXCLUDED.desired_revision,updated_at=now()`,
		workspaceID, w.S("tenantId"), w.S("ownerUserId"), w.S("projectId"), namespace, identifier, revision)
}

// pluginInputOf is the execution input fixed when the operation entered the plugin step.
// It is empty when the step has no plugins, in which case no execution is dispatched.
func pluginInputOf(o Object) Object {
	req := o.O("request")
	plugins := objectsOf(req["plugins"])
	if _, frozen := req["plugins"]; !frozen || len(plugins) == 0 {
		return nil
	}
	kind := "install_plugins"
	if o.S("kind") == "remove_plugin" {
		kind = "remove_plugins"
	}
	return Object{"kind": kind, "plugins": plugins}
}

// pluginStepSettled reports whether every snapshotted plugin has an item result. Item failures still
// settle the step; an execution that could not run at all does not.
func pluginStepSettled(t *transaction, o Object) bool {
	if len(pluginInputOf(o)) == 0 {
		return true
	}
	e := t.one("SELECT * FROM node_executions WHERE operation_id=$1 AND kind IN ('install_plugins','remove_plugins') ORDER BY created_at DESC,execution_id DESC LIMIT 1", o.S("id"))
	if e == nil || e.O("result").S("outcome") != "plugins_result" {
		return false
	}
	got := map[string]bool{}
	for _, item := range objectsOf(e.O("result")["items"]) {
		got[item.S("pluginId")] = true
	}
	for _, item := range objectsOf(o.O("request")["plugins"]) {
		if !got[item.S("pluginId")] {
			return false
		}
	}
	return true
}

func applyPluginResults(t *transaction, execution, result Object) {
	if result.S("outcome") != "plugins_result" {
		return
	}
	o := t.one("SELECT * FROM operations WHERE id=$1", execution.S("operationId"))
	require(o != nil, 404, "not_found")
	planned := map[string]Object{}
	for _, item := range objectsOf(o.O("request")["plugins"]) {
		planned[item.S("pluginId")] = item
	}
	meta := objectsOf(o.O("request")["pluginMeta"])
	items := objectsOf(result["items"])
	require(len(items) == len(planned), 400, "invalid_plugin_evidence")
	seen := make(map[string]bool, len(items))
	for _, item := range items {
		want, ok := planned[item.S("pluginId")]
		require(ok && !seen[item.S("pluginId")], 400, "invalid_plugin_evidence")
		seen[item.S("pluginId")] = true
		switch item.S("outcome") {
		case "installed":
			require(execution.S("kind") == "install_plugins", 400, "invalid_plugin_evidence")
			require(item.S("version") == want.S("version") && item.S("version") != "", 400, "invalid_plugin_evidence")
		case "removed":
			require(execution.S("kind") == "remove_plugins", 400, "invalid_plugin_evidence")
		case "failed":
			require(pluginFailureCodes[item.S("reason")], 400, "invalid_plugin_evidence")
		default:
			reject(400, "invalid_plugin_evidence")
		}
	}
	for _, item := range items {
		revision := pluginRevision(meta, item.S("pluginId"))
		switch item.S("outcome") {
		case "installed":
			pluginInstanceWriteback(t, o, item.S("pluginId"), "installed", item.S("version"), nil, revision)
		case "removed":
			pluginInstanceWriteback(t, o, item.S("pluginId"), "removed", "", nil, revision)
		default:
			reason := item.S("reason")
			pluginInstanceWriteback(t, o, item.S("pluginId"), "failed", "", &reason, revision)
		}
	}
}

var pluginFailureCodes = map[string]bool{
	"download_failed": true, "checksum_mismatch": true, "no_matching_target": true,
	"invalid_package": true, "install_failed": true, "plugin_in_use": true,
}

func objectsOf(v any) []Object {
	switch list := v.(type) {
	case []Object:
		return list
	case []any:
		out := make([]Object, 0, len(list))
		for _, item := range list {
			switch m := item.(type) {
			case map[string]any:
				out = append(out, Object(m))
			case Object:
				out = append(out, m)
			}
		}
		return out
	default:
		return nil
	}
}

func openAfterPlugins(kind string) bool {
	return kind == "create_project" || kind == "create_workspace" || kind == "start" || kind == "restart"
}

// finishPluginGate freezes the plugin set. An empty set opens admission immediately; otherwise the
// operation stops on the plugin step until the Node execution has a result for every plugin.
func finishPluginGate(t *transaction, o Object, wid string) string {
	if enterPluginStep(t, o) {
		openWorkspace(t, o, wid)
		return "done"
	}
	return "plugin"
}
