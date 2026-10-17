// Plugin marketplace state: cloud is the sole authority for the per-space plugin
// selection and the catalog snapshot. The Node execution plane only downloads
// and runs what cloud fans out through the operation/effect channel; it never
// syncs the marketplace itself. See docs/plugins.md for the full contract.
package core

import (
	"context"
	"strings"
	"time"

	"github.com/wanglongan587/cloud/internal/pluginmarket"
)

// SeedPluginSource upserts the configured marketplace source row. The default
// source keeps the 'official' namespace, matching the desktop marketplace
// identity model; the row is deployment-global, never tenant-scoped.
func (s *Store) SeedPluginSource(ctx context.Context, source pluginmarket.Source) error {
	_, err := s.transact(ctx, func(t *transaction) Object {
		t.exec(`INSERT INTO plugin_sources(namespace,url,branch) VALUES($1,$2,$3)
			ON CONFLICT (namespace) DO UPDATE SET url=EXCLUDED.url,branch=EXCLUDED.branch,updated_at=now(),version=plugin_sources.version+1`,
			source.Namespace, source.URL, source.Branch)
		return nil
	})
	return err
}

// ReplacePluginCatalog atomically replaces the source's catalog snapshot and
// stamps its synced_at. Readers never observe a half-synced catalog: the delete
// and inserts commit together. On failure the previous snapshot stays in place,
// which is the "read never leaves the database" guarantee the UI relies on.
func (s *Store) ReplacePluginCatalog(ctx context.Context, source pluginmarket.Source, entries []pluginmarket.Entry, indexedAt time.Time) error {
	_, err := s.transact(ctx, func(t *transaction) Object {
		t.exec("DELETE FROM plugin_catalog_entries WHERE source_namespace=$1", source.Namespace)
		for i := range entries {
			e := &entries[i]
			t.exec(`INSERT INTO plugin_catalog_entries(source_namespace,identifier,title,kind,version,description,homepage,license,logo,url,sha256,targets,pack_members,readme,marketplace_visible,source_url,indexed_at)
				VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)`,
				source.Namespace, e.Identifier, e.Title, e.Kind, e.Version, e.Description,
				nullable(e.Homepage), nullable(e.License), jsonValue(e.Logo), nullable(e.URL), nullable(e.SHA256),
				jsonValue(targetsJSON(e.Targets)), jsonValue(stringsJSON(e.PackMembers)), e.Readme,
				e.MarketplaceVisible, e.SourceURL, indexedAt)
		}
		t.exec("UPDATE plugin_sources SET synced_at=$2,sync_error=NULL,updated_at=now(),version=version+1 WHERE namespace=$1", source.Namespace, indexedAt)
		return nil
	})
	if err == nil && s.Events != nil {
		s.Events.PublishAll(SpaceEvent{Type: "plugins.catalog_updated"})
	}
	return err
}

// RecordPluginSyncError keeps the previous catalog snapshot and records why the
// latest sync failed on the source row; the next tick retries against the same
// state.
func (s *Store) RecordPluginSyncError(ctx context.Context, namespace string, syncErr error) error {
	_, err := s.transact(ctx, func(t *transaction) Object {
		t.exec("UPDATE plugin_sources SET sync_error=$2,updated_at=now(),version=version+1 WHERE namespace=$1", namespace, syncErr.Error())
		return nil
	})
	return err
}

// jsonValue renders v as JSON text for a jsonb column, or SQL NULL when v is
// nil, so optional catalog fields stay null instead of 'null'.
func jsonValue(v any) any {
	if v == nil {
		return nil
	}
	return jsonText(v)
}

func targetsJSON(targets []pluginmarket.ReleaseTarget) any {
	if len(targets) == 0 {
		return nil
	}
	return targets
}

func stringsJSON(values []string) any {
	if len(values) == 0 {
		return nil
	}
	return values
}

// pluginCatalogEntry builds the API shape of one catalog row, including the
// canonical id (namespace/identifier) the install API addresses it by.
func (t *transaction) pluginCatalogEntry(id string) Object {
	parts := strings.SplitN(id, "/", 2)
	if len(parts) != 2 {
		return nil
	}
	return t.one(`SELECT source_namespace,identifier,title,kind,version,description,homepage,license,logo,url,sha256,targets,pack_members,readme,marketplace_visible,source_url,indexed_at,(source_namespace||'/'||identifier) AS id FROM plugin_catalog_entries WHERE source_namespace=$1 AND identifier=$2`, parts[0], parts[1])
}

// pluginCatalog returns the full catalog snapshot (v1 decision D4: a few
// hundred entries; filtering and search happen in the frontend).
func pluginCatalog(t *transaction) Object {
	items := t.list(`SELECT source_namespace,identifier,title,kind,version,description,homepage,license,logo,url,sha256,targets,pack_members,readme,marketplace_visible,source_url,indexed_at,(source_namespace||'/'||identifier) AS id FROM plugin_catalog_entries WHERE marketplace_visible ORDER BY source_namespace,identifier`)
	source := t.one("SELECT namespace,synced_at,sync_error FROM plugin_sources WHERE enabled ORDER BY synced_at NULLS FIRST,namespace LIMIT 1")
	out := Object{"items": items}
	if source != nil {
		out["syncedAt"] = source["syncedAt"]
	}
	return out
}

// spacePluginRow loads one space_plugins row with the canonical
// namespace/identifier id the API contract exposes; the table itself has no id
// column, the pair IS the identity.
func (t *transaction) spacePluginRow(spaceID, namespace, identifier string) Object {
	return t.one(`SELECT *,(source_namespace||'/'||identifier) AS id FROM space_plugins WHERE space_id=$1 AND source_namespace=$2 AND identifier=$3`, spaceID, namespace, identifier)
}

// spacePluginList returns the space's selected plugins with their aggregate
// observed state. Desired/observed split makes the UI able to show "installing"
// while the fan-out effects are still running.
func spacePluginList(t *transaction, spaceID string) Object {
	rows := t.list(`SELECT space_id,tenant_id,source_namespace,identifier,desired_state,desired_version,observed_state,observed_version,install_error,version,created_at,updated_at,(source_namespace||'/'||identifier) AS id FROM space_plugins WHERE space_id=$1 ORDER BY source_namespace,identifier`, spaceID)
	for i, row := range rows {
		rows[i] = pluginSelection(t, spaceID, row.S("sourceNamespace"), row.S("identifier"))
	}
	return Object{"items": rows}
}

// pluginIdentity splits a canonical plugin id into its namespace and identifier
// segments; the catalog rows are the authority that such an id exists.
func pluginIdentity(id string) (namespace, identifier string, ok bool) {
	parts := strings.SplitN(id, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || strings.Contains(parts[1], "/") {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// livePluginWorkspaces returns the space's live runtime workspaces the
// fan-out targets, ordered deterministically: every live workspace (the Node
// plane may still provision or stop it; admission at effect time gates the
// actual dispatch).
func livePluginWorkspaces(t *transaction, spaceID string) []Object {
	// Run Workspaces install plugins through their own create_workspace step. They are not fan-out
	// targets: a user install must not queue an operation against a Workspace the user cannot see.
	return t.list(`SELECT w.* FROM workspaces w JOIN projects p ON p.id=w.project_id WHERE p.space_id=$1 AND w.deleted_at IS NULL AND w.issue_run_id IS NULL AND w.workflow_run_id IS NULL ORDER BY w.id`, spaceID)
}

// installSpacePlugin records an administrator's pinned selection. Execution waits durably;
// accepting selection is never evidence that a real runtime installed the plugin.
func installSpacePlugin(t *transaction, r *PublicRequest, uid string) Object {
	requireSpaceRole(spaceMember(t, r.SpaceID, uid), "admin")
	namespace, identifier, ok := pluginIdentity(r.Body.S("identifier"))
	require(ok, 400, "invalid_plugin_id")
	entry := t.pluginCatalogEntry(namespace + "/" + identifier)
	require(entry != nil, 404, "plugin_not_found")
	require(entry.S("kind") != "pack", 400, "plugin_kind_not_installable")
	desired := r.Body.S("pluginVersion")
	if desired == "" {
		desired = entry.S("version")
	}
	require(desired == entry.S("version"), 400, "plugin_version_unavailable")
	old := t.spacePluginRow(r.SpaceID, namespace, identifier)
	if old != nil {
		version(old, r.Body.N("version"))
		if old.S("desiredState") == "installed" && old.S("desiredVersion") == desired {
			// A repeated install is still an asset-import trigger: it is the administrator's
			// retry for documents that failed (or for a package download that failed) without
			// touching the already-imported copies.
			requestWorkflowAssetImport(t, r.SpaceID, r.TenantID, namespace, identifier, entry)
			schedulePluginMaintenance(t)
			return Object{"resource": pluginSelection(t, r.SpaceID, namespace, identifier)}
		}
		t.exec("UPDATE space_plugins SET desired_state='installed',desired_version=$4,observed_state='pending',install_error=NULL,requested_by_user_id=$5,selected_release=$6,desired_revision=desired_revision+1,version=version+1,updated_at=clock_timestamp() WHERE space_id=$1 AND source_namespace=$2 AND identifier=$3", r.SpaceID, namespace, identifier, desired, uid, jsonText(entry))
	} else {
		require(r.Body.N("version") == 0, 409, "version_conflict")
		t.exec("INSERT INTO space_plugins(space_id,tenant_id,source_namespace,identifier,desired_state,desired_version,observed_state,requested_by_user_id,selected_release) VALUES($1,$2,$3,$4,'installed',$5,'pending',$6,$7)", r.SpaceID, r.TenantID, namespace, identifier, desired, uid, jsonText(entry))
	}
	requestWorkflowAssetImport(t, r.SpaceID, r.TenantID, namespace, identifier, entry)
	recordPluginTargets(t, r, namespace, identifier)
	schedulePluginMaintenance(t)
	return Object{"resource": pluginSelection(t, r.SpaceID, namespace, identifier)}
}

// removeSpacePlugin uses the same administrator, version and maintenance boundary as install.
func removeSpacePlugin(t *transaction, r *PublicRequest, uid string) Object {
	requireSpaceRole(spaceMember(t, r.SpaceID, uid), "admin")
	namespace, identifier, ok := pluginIdentity(r.Body.S("identifier"))
	require(ok, 400, "invalid_plugin_id")
	old := t.spacePluginRow(r.SpaceID, namespace, identifier)
	require(old != nil, 404, "plugin_not_installed")
	version(old, r.Body.N("version"))
	t.exec("UPDATE space_plugins SET desired_state='removed',observed_state='pending',install_error=NULL,requested_by_user_id=$4,desired_revision=desired_revision+1,version=version+1,updated_at=clock_timestamp() WHERE space_id=$1 AND source_namespace=$2 AND identifier=$3", r.SpaceID, namespace, identifier, uid)
	syncSpaceAgent(t, r.SpaceID, r.TenantID, namespace+"/"+identifier, "removing", "removed")
	recordPluginTargets(t, r, namespace, identifier)
	schedulePluginMaintenance(t)
	return Object{"resource": pluginSelection(t, r.SpaceID, namespace, identifier)}
}

// recordPluginTargets covers only runtimes that existed when the selection was accepted.
// A later-created runtime is not silently added to this maintenance responsibility.
func recordPluginTargets(t *transaction, r *PublicRequest, namespace, identifier string) {
	row := t.spacePluginRow(r.SpaceID, namespace, identifier)
	targets := livePluginWorkspaces(t, r.SpaceID)
	for _, w := range targets {
		t.exec("INSERT INTO workspace_plugin_instances(workspace_id,tenant_id,owner_user_id,project_id,source_namespace,identifier,observed_state,desired_revision) VALUES($1,$2,$3,$4,$5,$6,'pending',$7) ON CONFLICT(workspace_id,source_namespace,identifier) DO UPDATE SET observed_state=CASE WHEN workspace_plugin_instances.maintenance_operation_id IS NULL THEN 'pending' ELSE workspace_plugin_instances.observed_state END,desired_revision=EXCLUDED.desired_revision,install_error=NULL,version=workspace_plugin_instances.version+1,updated_at=clock_timestamp()", w.S("id"), w.S("tenantId"), w.S("ownerUserId"), w.S("projectId"), namespace, identifier, row.N("desiredRevision"))
	}
	if len(targets) == 0 {
		t.exec("UPDATE space_plugins SET observed_state=desired_state,observed_version=CASE WHEN desired_state='installed' THEN desired_version END WHERE space_id=$1 AND source_namespace=$2 AND identifier=$3", r.SpaceID, namespace, identifier)
		syncSpaceAgent(t, r.SpaceID, r.TenantID, namespace+"/"+identifier, row.S("desiredState"), row.S("desiredState"))
	}
}

// pluginAggregate folds the fan-out instance states into the space-level
// observed state: any failure wins; otherwise any non-terminal instance keeps
// the direction's in-progress state; all terminal (or no live targets at all)
// means terminal. The rule is pure so it is unit-testable without PostgreSQL.
func pluginAggregate(desired string, states []string) string {
	progress, terminal := "installing", "installed"
	if desired == "removed" {
		progress, terminal = "removing", "removed"
	}
	if len(states) == 0 {
		return terminal
	}
	for _, state := range states {
		if state == "failed" {
			return "failed"
		}
	}
	for _, state := range states {
		if state != terminal {
			return progress
		}
	}
	return terminal
}

// pluginInstanceWriteback updates one fan-out instance from a Node execution outcome and recomputes
// the space-level aggregate in the same transaction. The space is derived through the instance's
// project, never from caller input. A terminal result that does not match the revision snapshotted
// for this operation becomes pending again, so a newer selection is not overwritten.
func pluginInstanceWriteback(t *transaction, op Object, pluginID, state, version string, installError *string, revision int64) {
	if pluginID == "" {
		pluginID = op.O("request").S("pluginId")
	}
	if revision == 0 {
		revision = op.O("request").N("desiredRevision")
	}
	namespace, identifier, ok := pluginIdentity(pluginID)
	if !ok {
		return
	}
	row := t.one(`SELECT wi.workspace_id,wi.desired_revision,p.space_id,ws.tenant_id FROM workspace_plugin_instances wi JOIN workspaces ws ON ws.id=wi.workspace_id JOIN projects p ON p.id=ws.project_id WHERE wi.workspace_id=$1 AND wi.source_namespace=$2 AND wi.identifier=$3`,
		op.S("workspaceId"), namespace, identifier)
	if row == nil || row.S("spaceId") == "" {
		return
	}
	var err any
	if installError != nil {
		err = *installError
	}
	terminal := state == "installed" || state == "removed" || state == "failed"
	if terminal && row.N("desiredRevision") != revision {
		state = "pending"
		// A late failure settles its original attempt without poisoning the newer selection.
		err = nil
	}
	t.exec(`UPDATE workspace_plugin_instances SET observed_state=$4,observed_version=$5,install_error=$6,version=version+1,updated_at=now() WHERE workspace_id=$1 AND source_namespace=$2 AND identifier=$3`,
		op.S("workspaceId"), namespace, identifier, state, nullable(version), err)
	if terminal {
		t.exec("UPDATE workspace_plugin_instances SET maintenance_operation_id=NULL,pending_reason=NULL WHERE workspace_id=$1 AND source_namespace=$2 AND identifier=$3", op.S("workspaceId"), namespace, identifier)
	}
	// Run Workspace instances are execution facts for that run. They do not change the Space aggregate,
	// so one disposable install failure cannot retire an Agent the Space still has selected.
	states := t.list("SELECT observed_state FROM workspace_plugin_instances wi WHERE source_namespace=$1 AND identifier=$2 AND workspace_id IN (SELECT w.id FROM workspaces w JOIN projects p ON p.id=w.project_id WHERE p.space_id=$3 AND w.deleted_at IS NULL AND w.issue_run_id IS NULL AND w.workflow_run_id IS NULL)", namespace, identifier, row.S("spaceId"))
	aggregate := "installed"
	if desired := t.one("SELECT desired_state,tenant_id FROM space_plugins WHERE space_id=$1 AND source_namespace=$2 AND identifier=$3", row.S("spaceId"), namespace, identifier); desired != nil {
		desiredState := desired.S("desiredState")
		flat := make([]string, 0, len(states))
		for _, s := range states {
			flat = append(flat, s.S("observedState"))
		}
		aggregate = pluginAggregate(desiredState, flat)
		syncSpaceAgent(t, row.S("spaceId"), desired.S("tenantId"), pluginID, aggregate, desiredState)
	}
	t.exec(`UPDATE space_plugins SET observed_state=$4,observed_version=$5,install_error=$6,version=version+1,updated_at=now() WHERE space_id=$1 AND source_namespace=$2 AND identifier=$3`,
		row.S("spaceId"), namespace, identifier, aggregate, nullable(version), err)
	t.events = append(t.events, SpaceEvent{Type: "space.plugins_updated", SpaceID: row.S("spaceId")})
}

// syncSpaceAgent keeps the Space Agent row aligned with an agent-kind plugin's selection. A failed
// instance does not retire the row; only a removed selection does.
func syncSpaceAgent(t *transaction, spaceID, tenantID, pluginID, aggregate, desired string) {
	if desired == "removed" {
		t.exec("UPDATE space_agents SET status='retired',updated_at=now() WHERE space_id=$1 AND plugin_id=$2", spaceID, pluginID)
		return
	}
	if aggregate != "installed" {
		return
	}
	// The selected release is the accepted identity snapshot; a later catalog refresh must not
	// change whether an existing selection represents an Agent.
	namespace, identifier, ok := pluginIdentity(pluginID)
	if !ok {
		return
	}
	entry := t.spacePluginRow(spaceID, namespace, identifier).O("selectedRelease")
	if entry.S("kind") != "agent" {
		return
	}
	title := entry.S("title")
	if title == "" {
		title = pluginID
	}
	t.exec(`INSERT INTO space_agents(id,space_id,tenant_id,plugin_id,display_name,status) VALUES($1,$2,$3,$4,$5,'active')
		ON CONFLICT(space_id,plugin_id) DO UPDATE SET status='active',display_name=EXCLUDED.display_name,updated_at=now()`,
		newID(), spaceID, tenantID, pluginID, title)
}
