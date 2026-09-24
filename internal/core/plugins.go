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
	return Object{"items": t.list(`SELECT space_id,tenant_id,source_namespace,identifier,desired_state,desired_version,observed_state,observed_version,install_error,version,created_at,updated_at,(source_namespace||'/'||identifier) AS id FROM space_plugins WHERE space_id=$1 ORDER BY source_namespace,identifier`, spaceID)}
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
	return t.list(`SELECT w.* FROM workspaces w JOIN projects p ON p.id=w.project_id WHERE p.space_id=$1 AND w.deleted_at IS NULL ORDER BY w.id`, spaceID)
}

// installSpacePlugin records the member's install intent and fans it out:
// one workspace_plugin_instances row per live runtime workspace plus one
// install_plugin operation per target workspace. The one_project_operation
// constraint serializes projects: a project with an in-flight operation makes
// the whole install 409, and one install never creates two operations for the
// same project (a second live workspace of a busy project is covered by the
// next install, which re-fans-out pending instances).
func installSpacePlugin(t *transaction, r *PublicRequest, uid string) Object {
	spaceMember(t, r.SpaceID, uid)
	namespace, identifier, ok := pluginIdentity(r.Body.S("identifier"))
	require(ok, 400, "invalid_plugin_id")
	entry := t.pluginCatalogEntry(namespace + "/" + identifier)
	require(entry != nil, 404, "plugin_not_found")
	// Packs are orchestration entries; v1 does not expand them (plan Q4).
	require(entry.S("kind") != "pack", 400, "plugin_kind_not_installable")
	desiredVersion := r.Body.S("pluginVersion")
	if desiredVersion == "" {
		desiredVersion = entry.S("version")
	}
	// Versions are pinned at install time; the catalog only lists its current
	// version, so anything else cannot be installed from it (decision D2).
	require(desiredVersion == entry.S("version"), 400, "plugin_version_unavailable")

	// The already-pinned case short-circuits before any fan-out admission:
	// re-installing the same version is an idempotent success that must not
	// depend on the projects being idle.
	old := t.spacePluginRow(r.SpaceID, namespace, identifier)
	if old != nil && old.S("desiredState") == "installed" && old.S("desiredVersion") == desiredVersion {
		return Object{"resource": old}
	}

	workspaces := livePluginWorkspaces(t, r.SpaceID)
	// Fail before any write when a target project is busy; the unique index
	// one_project_operation remains the final integrity guard.
	for _, w := range workspaces {
		idleProject(t, w.S("projectId"))
	}

	if old == nil {
		t.exec(`INSERT INTO space_plugins(space_id,tenant_id,source_namespace,identifier,desired_state,desired_version,observed_state) VALUES($1,$2,$3,$4,'installed',$5,'pending')`,
			r.SpaceID, r.TenantID, namespace, identifier, desiredVersion)
	} else {
		t.exec(`UPDATE space_plugins SET desired_state='installed',desired_version=$4,observed_state='pending',observed_version=NULL,install_error=NULL,version=version+1,updated_at=now() WHERE space_id=$1 AND source_namespace=$2 AND identifier=$3`,
			r.SpaceID, namespace, identifier, desiredVersion)
	}

	// Fan-out: reset every live workspace's instance to pending and create the
	// install operation where the project admits one. A space without live
	// runtime workspaces has nothing to execute: the aggregate is already
	// terminal (plan aggregation rule), so the row converges immediately.
	projectOps := map[string]bool{}
	hash := requestHash(r.Method, r.Path, r.Body)
	for _, w := range workspaces {
		t.exec(`INSERT INTO workspace_plugin_instances(workspace_id,tenant_id,owner_user_id,project_id,source_namespace,identifier,observed_state) VALUES($1,$2,$3,$4,$5,$6,'pending')
			ON CONFLICT (workspace_id,source_namespace,identifier) DO UPDATE SET observed_state='pending',observed_version=NULL,install_error=NULL,version=workspace_plugin_instances.version+1,updated_at=now()`,
			w.S("id"), w.S("tenantId"), w.S("ownerUserId"), w.S("projectId"), namespace, identifier)
		if projectOps[w.S("projectId")] {
			continue
		}
		req := Object{"pluginId": namespace + "/" + identifier, "version": desiredVersion}
		newOperation(t, r, uid, w.S("projectId"), w.S("id"), "install_plugin", "plugin", hash, req)
		projectOps[w.S("projectId")] = true
	}
	if len(workspaces) == 0 {
		t.exec(`UPDATE space_plugins SET observed_state='installed',observed_version=$4,install_error=NULL,version=version+1,updated_at=now() WHERE space_id=$1 AND source_namespace=$2 AND identifier=$3`,
			r.SpaceID, namespace, identifier, desiredVersion)
	}
	return Object{"resource": t.spacePluginRow(r.SpaceID, namespace, identifier)}
}

// removeSpacePlugin records the removal intent and fans a remove_plugin
// operation out to the same workspace set an install would target.
func removeSpacePlugin(t *transaction, r *PublicRequest, uid string) Object {
	spaceMember(t, r.SpaceID, uid)
	namespace, identifier, ok := pluginIdentity(r.Body.S("identifier"))
	require(ok, 400, "invalid_plugin_id")
	old := t.spacePluginRow(r.SpaceID, namespace, identifier)
	require(old != nil, 404, "plugin_not_installed")
	version(old, r.Body.N("version"))

	workspaces := livePluginWorkspaces(t, r.SpaceID)
	for _, w := range workspaces {
		idleProject(t, w.S("projectId"))
	}

	t.exec(`UPDATE space_plugins SET desired_state='removed',observed_state='removing',observed_version=NULL,install_error=NULL,version=version+1,updated_at=now() WHERE space_id=$1 AND source_namespace=$2 AND identifier=$3`,
		r.SpaceID, namespace, identifier)
	projectOps := map[string]bool{}
	hash := requestHash(r.Method, r.Path, r.Body)
	for _, w := range workspaces {
		t.exec(`UPDATE workspace_plugin_instances SET observed_state='removing',observed_version=NULL,install_error=NULL,version=version+1,updated_at=now() WHERE workspace_id=$1 AND source_namespace=$2 AND identifier=$3`,
			w.S("id"), namespace, identifier)
		if projectOps[w.S("projectId")] {
			continue
		}
		req := Object{"pluginId": namespace + "/" + identifier, "version": old.S("desiredVersion")}
		newOperation(t, r, uid, w.S("projectId"), w.S("id"), "remove_plugin", "plugin", hash, req)
		projectOps[w.S("projectId")] = true
	}
	if len(workspaces) == 0 {
		// Nothing to remove on the execution plane: converge immediately.
		t.exec(`UPDATE space_plugins SET observed_state='removed',observed_version=NULL,install_error=NULL,version=version+1,updated_at=now() WHERE space_id=$1 AND source_namespace=$2 AND identifier=$3`,
			r.SpaceID, namespace, identifier)
	}
	return Object{"resource": t.spacePluginRow(r.SpaceID, namespace, identifier)}
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

// pluginInstanceWriteback updates one fan-out instance from an effect outcome
// and recomputes the space-level aggregate in the same transaction, returning
// the space the SSE invalidation must target. The space is derived through the
// instance's project, never from caller input.
func pluginInstanceWriteback(t *transaction, op Object, state, version string, installError *string, spaceEvents *[]SpaceEvent) {
	namespace, identifier, ok := pluginIdentity(op.O("request").S("pluginId"))
	if !ok {
		return
	}
	row := t.one(`SELECT wi.workspace_id,p.space_id FROM workspace_plugin_instances wi JOIN workspaces ws ON ws.id=wi.workspace_id JOIN projects p ON p.id=ws.project_id WHERE wi.workspace_id=$1 AND wi.source_namespace=$2 AND wi.identifier=$3`,
		op.S("workspaceId"), namespace, identifier)
	if row == nil || row.S("spaceId") == "" {
		return
	}
	var err any
	if installError != nil {
		err = *installError
	}
	t.exec(`UPDATE workspace_plugin_instances SET observed_state=$4,observed_version=$5,install_error=$6,version=version+1,updated_at=now() WHERE workspace_id=$1 AND source_namespace=$2 AND identifier=$3`,
		op.S("workspaceId"), namespace, identifier, state, nullable(version), err)
	states := t.list("SELECT observed_state FROM workspace_plugin_instances WHERE source_namespace=$1 AND identifier=$2 AND workspace_id IN (SELECT w.id FROM workspaces w JOIN projects p ON p.id=w.project_id WHERE p.space_id=$3 AND w.deleted_at IS NULL)", namespace, identifier, row.S("spaceId"))
	aggregate := "installed"
	if desired := t.one("SELECT desired_state FROM space_plugins WHERE space_id=$1 AND source_namespace=$2 AND identifier=$3", row.S("spaceId"), namespace, identifier); desired != nil {
		flat := make([]string, 0, len(states))
		for _, s := range states {
			flat = append(flat, s.S("observedState"))
		}
		aggregate = pluginAggregate(desired.S("desiredState"), flat)
	}
	t.exec(`UPDATE space_plugins SET observed_state=$4,observed_version=$5,install_error=$6,version=version+1,updated_at=now() WHERE space_id=$1 AND source_namespace=$2 AND identifier=$3`,
		row.S("spaceId"), namespace, identifier, aggregate, nullable(version), err)
	*spaceEvents = append(*spaceEvents, SpaceEvent{Type: "space.plugins_updated", SpaceID: row.S("spaceId")})
}
