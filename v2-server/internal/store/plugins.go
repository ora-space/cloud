package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/wanglongan587/cloud/v2-server/internal/pluginmarket"
)

// Plugin marketplace errors. The HTTP layer maps these stable outcomes to Cloud error codes.
var (
	ErrInvalidPluginID          = errors.New("invalid plugin id")
	ErrPluginNotFound           = errors.New("plugin not found")
	ErrPluginVersionUnavailable = errors.New("plugin version unavailable")
	ErrPluginNotInstalled       = errors.New("plugin not installed")
	ErrPluginKindNotInstallable = errors.New("plugin kind not installable")
	ErrPluginRuntimeManaged     = errors.New("plugin runtime managed")
	ErrIdempotencyConflict      = errors.New("idempotency key reused")
	ErrPluginWorkflowInvalid    = errors.New("workflow plugin document invalid")
	ErrPluginPackageCredentials = errors.New("plugin package credentials unavailable")
)

// PluginCatalogEntry mirrors Cloud's PluginCatalogEntry. Logo, release targets and pack members
// remain generic JSON because the temporary server only serves their stored catalog shape.
type PluginCatalogEntry struct {
	ID                      string    `json:"id"`
	SourceNamespace         string    `json:"sourceNamespace"`
	Identifier              string    `json:"identifier"`
	Title                   string    `json:"title"`
	Kind                    string    `json:"kind"`
	Version                 string    `json:"version"`
	Description             string    `json:"description"`
	Homepage                *string   `json:"homepage"`
	License                 *string   `json:"license"`
	Logo                    any       `json:"logo"`
	URL                     *string   `json:"url"`
	SHA256                  *string   `json:"sha256"`
	Targets                 any       `json:"targets"`
	PackMembers             any       `json:"packMembers"`
	Readme                  *string   `json:"readme"`
	MarketplaceVisible      bool      `json:"marketplaceVisible"`
	SourceURL               string    `json:"sourceUrl"`
	IndexedAt               time.Time `json:"indexedAt"`
	AgentConfiguration      any       `json:"agentConfiguration,omitempty"`
	AgentConfigurationIssue *string   `json:"agentConfigurationIssue,omitempty"`
}

// PluginInstallInput is the request Cloud's POST plugin route accepts.
type PluginInstallInput struct {
	Identifier    string
	PluginVersion string
	Version       int64
}

// PluginRemoveInput is the request Cloud's DELETE plugin route accepts.
type PluginRemoveInput struct {
	Identifier string
	Version    int64
}

// PluginPackageSource is the frozen package location for a workflow plugin. Credentials are
// decrypted only on the owner-only install route, just before the package is installed.
type PluginPackageSource struct {
	Entry       PluginCatalogEntry
	Locator     string
	Digest      string
	Retrieval   pluginmarket.Retrieval
	Credentials pluginmarket.Credentials
}

// ReplacePluginCatalog atomically replaces the marketplace snapshot after a successful scan.
// Readers never observe a partially synced catalog because the delete and inserts share one
// transaction; a failure keeps the previous snapshot intact.
func (s *Store) ReplacePluginCatalog(ctx context.Context, entries []pluginmarket.Entry, indexedAt time.Time) error {
	if len(entries) == 0 {
		return errors.New("refusing to replace plugin catalog with an empty snapshot")
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, e := tx.Exec(ctx, `DELETE FROM v2_plugin_catalog`); e != nil {
			return fmt.Errorf("clear plugin catalog: %w", e)
		}
		for _, source := range entries {
			entry := catalogEntry(source, indexedAt)
			raw, e := json.Marshal(entry)
			if e != nil {
				return fmt.Errorf("encode catalog entry %s: %w", entry.ID, e)
			}
			if _, e := tx.Exec(ctx, `
				INSERT INTO v2_plugin_catalog (source_namespace, identifier, kind, version, data, synced_at)
				VALUES ($1, $2, $3, $4, $5, $6)`,
				entry.SourceNamespace, entry.Identifier, entry.Kind, entry.Version, raw, indexedAt); e != nil {
				return fmt.Errorf("insert catalog entry %s: %w", entry.ID, e)
			}
		}
		return nil
	})
}

func catalogEntry(source pluginmarket.Entry, indexedAt time.Time) PluginCatalogEntry {
	return PluginCatalogEntry{
		ID: source.Namespace + "/" + source.Identifier, SourceNamespace: source.Namespace,
		Identifier: source.Identifier, Title: source.Title, Kind: source.Kind,
		Version: source.Version, Description: source.Description,
		Homepage: optional(source.Homepage), License: optional(source.License),
		Logo: source.Logo, URL: optional(source.URL), SHA256: optional(source.SHA256),
		Targets: targets(source.Targets), PackMembers: optionalStrings(source.PackMembers),
		Readme: optional(source.Readme), MarketplaceVisible: source.MarketplaceVisible,
		SourceURL: source.SourceURL, IndexedAt: indexedAt,
	}
}

func optional(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func targets(values []pluginmarket.ReleaseTarget) any {
	if len(values) == 0 {
		return nil
	}
	return values
}

func optionalStrings(values []string) any {
	if len(values) == 0 {
		return nil
	}
	return values
}

// PluginCatalog returns the visible marketplace snapshot for a member of the caller's space.
func (s *Store) PluginCatalog(ctx context.Context, tenant, space string) ([]PluginCatalogEntry, time.Time, error) {
	if e := requireSpace(ctx, s.pool, tenant, space); e != nil {
		return nil, time.Time{}, e
	}
	rows, e := s.pool.Query(ctx, `
		SELECT data, synced_at
		FROM v2_plugin_catalog
		WHERE (data->>'marketplaceVisible')::boolean AND (NOT $2 OR source_namespace <> 'official')
		UNION ALL
		SELECT item, synced_at
		FROM v2_market_sources CROSS JOIN LATERAL jsonb_array_elements(snapshot) item
		WHERE space_id = $1 AND enabled AND (NOT $2 OR namespace <> 'official')
			AND COALESCE((item->>'marketplaceVisible')::boolean, false)
		ORDER BY 2 DESC`, space, s.officialDisabled)
	if e != nil {
		return nil, time.Time{}, fmt.Errorf("list plugin catalog: %w", e)
	}
	defer rows.Close()
	items := []PluginCatalogEntry{}
	var syncedAt time.Time
	for rows.Next() {
		var raw []byte
		var stamp time.Time
		if e := rows.Scan(&raw, &stamp); e != nil {
			return nil, time.Time{}, fmt.Errorf("scan plugin catalog: %w", e)
		}
		var entry PluginCatalogEntry
		if e := json.Unmarshal(raw, &entry); e != nil {
			return nil, time.Time{}, fmt.Errorf("decode plugin catalog: %w", e)
		}
		items = append(items, entry)
		if stamp.After(syncedAt) {
			syncedAt = stamp
		}
	}
	if e := rows.Err(); e != nil {
		return nil, time.Time{}, fmt.Errorf("iterate plugin catalog: %w", e)
	}
	return items, syncedAt, nil
}

// SpacePlugins returns every live market selection in a space. Removed rows remain in the
// response so the UI can distinguish "never selected" from "explicitly removed".
func (s *Store) SpacePlugins(ctx context.Context, tenant, space string) ([]SpacePlugin, error) {
	if e := requireSpace(ctx, s.pool, tenant, space); e != nil {
		return nil, e
	}
	return querySpacePlugins(ctx, s.pool, space, false)
}

// PluginPackageSource returns the frozen download location for a marketplace entry. It resolves
// package credentials, so only the owner-only install route may call it.
func (s *Store) PluginPackageSource(ctx context.Context, tenant, space, id string) (PluginPackageSource, error) {
	if e := requireSpace(ctx, s.pool, tenant, space); e != nil {
		return PluginPackageSource{}, e
	}
	namespace, identifier, ok := pluginIdentity(id)
	if !ok {
		return PluginPackageSource{}, ErrInvalidPluginID
	}
	kind, _, raw, e := pluginCatalogRow(ctx, s.pool, space, namespace, identifier, s.officialDisabled)
	if e != nil {
		return PluginPackageSource{}, e
	}
	var entry PluginCatalogEntry
	if e := json.Unmarshal(raw, &entry); e != nil {
		return PluginPackageSource{}, fmt.Errorf("decode catalog entry: %w", e)
	}
	source := PluginPackageSource{Entry: entry}
	if kind != "workflow" {
		return source, nil
	}
	if entry.URL == nil || entry.SHA256 == nil || *entry.URL == "" || *entry.SHA256 == "" {
		return PluginPackageSource{}, ErrPluginVersionUnavailable
	}
	source.Locator, source.Digest = *entry.URL, *entry.SHA256
	if namespace == "official" {
		return source, nil
	}
	var retrievalJSON, credentials []byte
	e = s.pool.QueryRow(ctx, `
		SELECT retrieval, credentials FROM v2_market_sources
		WHERE space_id = $1 AND namespace = $2 AND enabled`, space, namespace).
		Scan(&retrievalJSON, &credentials)
	if errors.Is(e, pgx.ErrNoRows) {
		return PluginPackageSource{}, ErrPluginNotFound
	}
	if e != nil {
		return PluginPackageSource{}, fmt.Errorf("read market source: %w", e)
	}
	if e := json.Unmarshal(retrievalJSON, &source.Retrieval); e != nil {
		return PluginPackageSource{}, fmt.Errorf("decode market retrieval: %w", e)
	}
	if len(credentials) > 0 {
		binding := strings.Join([]string{space, namespace, source.Retrieval.Endpoint, source.Retrieval.Bucket}, "/")
		source.Credentials, e = pluginmarket.OpenCredentials(credentials, binding)
		if e != nil {
			return PluginPackageSource{}, ErrPluginPackageCredentials
		}
	}
	return source, nil
}

// InstallSpacePlugin records an owner's selection and terminal install result. v2 has no generic
// plugin executor, so the simplified backend settles the selection immediately instead of
// claiming that package bytes were downloaded or executed. Hook selections are demo-only: they
// are settled as installed but never download or run a Hook package. Skill selections are the space
// selection itself: a project Node may still receive the package, but the selection is usable as
// soon as it is written, so a Space without a bound project is never left waiting for one.
func (s *Store) InstallSpacePlugin(ctx context.Context, tenant, space, caller, path, key string, in PluginInstallInput) (SpacePlugin, error) {
	return s.installSpacePlugin(ctx, tenant, space, caller, path, key, in, nil, nil)
}

// selectionSettlesWithoutNode reports whether a selected plugin is usable from its record alone.
// Agents are installed by the runtime, Hooks only record the demo choice, and Skills are the Space
// selection: waiting for every bound project to confirm would keep them pending forever in a Space
// with no project, and the Skills page and workflow Agent picker read that settled state.
func selectionSettlesWithoutNode(kind string) bool {
	return kind == "agent" || kind == "hook" || kind == "skill"
}

// InstallWorkflowPlugin imports the documents from a verified workflow package and records the
// marketplace selection in the same transaction. The imported workflows remain user data after
// removal; the import table makes repeated installs idempotent and lets an upgrade refresh an
// untouched import without replacing a workflow the user has edited.
func (s *Store) InstallWorkflowPlugin(
	ctx context.Context,
	tenant, space, caller, path, key string,
	in PluginInstallInput,
	documents []pluginmarket.WorkflowDocument,
	validate func(string) error,
) (SpacePlugin, error) {
	if len(documents) == 0 || validate == nil {
		return SpacePlugin{}, ErrPluginKindNotInstallable
	}
	return s.installSpacePlugin(ctx, tenant, space, caller, path, key, in, documents, validate)
}

func (s *Store) installSpacePlugin(
	ctx context.Context,
	tenant, space, caller, path, key string,
	in PluginInstallInput,
	workflowDocuments []pluginmarket.WorkflowDocument,
	validate func(string) error,
) (SpacePlugin, error) {
	var out SpacePlugin
	e := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if e := requireSpace(ctx, tx, tenant, space); e != nil {
			return e
		}
		if replay, ok, e := replayPluginMutation(ctx, tx, tenant, caller, "POST", path, key, pluginInstallHash(in)); e != nil || ok {
			out = replay
			return e
		}
		namespace, identifier, ok := pluginIdentity(in.Identifier)
		if !ok {
			return ErrInvalidPluginID
		}
		// Serialize the first selection as well as later updates (#835).
		if _, e := tx.Exec(ctx, `select pg_advisory_xact_lock(hashtextextended($1,1))`, space+"/"+in.Identifier); e != nil {
			return e
		}
		kind, version, raw, e := pluginCatalogRow(ctx, tx, space, namespace, identifier, s.officialDisabled)
		if e != nil {
			return e
		}
		// Only Skill and workflow plugins have a server-backed install in v2. Agents, Hooks and any
		// other kind have no package to deliver or run, so a new selection would only pretend to
		// install them; the page shows those kinds read-only instead. A preinstalled Agent is still
		// handled below so its runtime-managed guard stays the recognizable error.
		if kind != "skill" && kind != "workflow" && kind != "agent" {
			return ErrPluginKindNotInstallable
		}
		if kind == "workflow" && (len(workflowDocuments) == 0 || validate == nil) {
			return ErrPluginKindNotInstallable
		}
		desired := in.PluginVersion
		if desired == "" {
			desired = version
		}
		if desired != version {
			return ErrPluginVersionUnavailable
		}
		var entry PluginCatalogEntry
		if e := json.Unmarshal(raw, &entry); e != nil {
			return fmt.Errorf("decode catalog entry: %w", e)
		}
		old, oldErr := querySpacePlugin(ctx, tx, space, namespace, identifier, true)
		switch {
		case oldErr == nil:
			// Agents never get a server-backed install here: a preinstalled Agent is managed by the
			// runtime, and an existing market Agent selection (requested_by_user_id set) is only
			// browsed, so an update is rejected instead of quietly refreshing it.
			if old.Kind == "agent" {
				if old.RequestedByUserID == nil {
					return ErrPluginRuntimeManaged
				}
				return ErrPluginKindNotInstallable
			}
			if in.Version != old.Version {
				return ErrConflict
			}
			if kind != "workflow" && old.DesiredState == "installed" && old.ObservedState == "installed" && old.DesiredVersion == desired {
				out = old
				return recordPluginMutation(ctx, tx, tenant, caller, "POST", path, key, pluginInstallHash(in), out)
			}
			observedState, observedVersion := "pending", any(nil)
			if selectionSettlesWithoutNode(kind) {
				observedState, observedVersion = "installed", desired
			}
			if _, e := tx.Exec(ctx, `
				UPDATE v2_space_agents
				SET name = $4, kind = $5, desired_state = 'installed', desired_version = $6,
					observed_state = $7, observed_version = $8, install_error = NULL,
					requested_by_user_id = $9, status = 'active', version = version + 1, updated_at = now()
				WHERE space_id = $1 AND tenant_id = $2 AND source_namespace = $3 AND identifier = $10`,
				space, tenant, namespace, entry.Title, kind, desired, observedState, observedVersion, caller, identifier); e != nil {
				return fmt.Errorf("update plugin selection: %w", e)
			}
		case errors.Is(oldErr, ErrNotFound):
			// A new Agent selection would only pretend to install a runnable Agent; preinstalled
			// Agents always exist already, so they never reach this branch.
			if kind != "skill" && kind != "workflow" {
				return ErrPluginKindNotInstallable
			}
			if in.Version != 0 {
				return ErrConflict
			}
			observedState, observedVersion := "pending", any(nil)
			if selectionSettlesWithoutNode(kind) {
				observedState, observedVersion = "installed", desired
			}
			if _, e := tx.Exec(ctx, `
				INSERT INTO v2_space_agents
					(tenant_id, space_id, name, status, source_namespace, identifier, kind,
					 desired_state, desired_version, observed_state, observed_version, requested_by_user_id)
				VALUES ($1, $2, $3, 'active', $4, $5, $6, 'installed', $7, $8, $9, $10)`,
				tenant, space, entry.Title, namespace, identifier, kind, desired, observedState, observedVersion, caller); e != nil {
				return fmt.Errorf("insert plugin selection: %w", e)
			}
		default:
			return oldErr
		}
		out, e = querySpacePlugin(ctx, tx, space, namespace, identifier, false)
		if e != nil {
			return e
		}
		if kind == "skill" {
			if e := freezeSkill(ctx, tx, out, entry); e != nil {
				return e
			}
		}
		if kind == "workflow" {
			if e := importWorkflowDocuments(ctx, tx, out, workflowDocuments, validate); e != nil {
				return e
			}
			if _, e := tx.Exec(ctx, `
				UPDATE v2_space_agents
				SET observed_state = 'installed', observed_version = $4, install_error = NULL, updated_at = now()
				WHERE id = $1 AND tenant_id = $2 AND space_id = $3`,
				out.ID, tenant, space, desired); e != nil {
				return fmt.Errorf("settle workflow install: %w", e)
			}
			out, e = querySpacePlugin(ctx, tx, space, namespace, identifier, false)
			if e != nil {
				return e
			}
		}
		return recordPluginMutation(ctx, tx, tenant, caller, "POST", path, key, pluginInstallHash(in), out)
	})
	return out, e
}

func pluginCatalogRow(ctx context.Context, q dbTX, space, namespace, identifier string, officialDisabled bool) (kind, version string, raw []byte, e error) {
	if officialDisabled && namespace == "official" {
		return "", "", nil, ErrPluginNotFound
	}
	e = q.QueryRow(ctx, `
		SELECT kind, version, data FROM v2_plugin_catalog
		WHERE source_namespace = $1 AND identifier = $2
		UNION ALL
		SELECT item->>'kind', item->>'version', item
		FROM v2_market_sources CROSS JOIN LATERAL jsonb_array_elements(snapshot) item
		WHERE space_id = $3 AND enabled AND namespace = $1 AND item->>'identifier' = $2`,
		namespace, identifier, space).Scan(&kind, &version, &raw)
	if errors.Is(e, pgx.ErrNoRows) {
		return "", "", nil, ErrPluginNotFound
	}
	if e != nil {
		return "", "", nil, fmt.Errorf("read catalog entry: %w", e)
	}
	return kind, version, raw, nil
}

func importWorkflowDocuments(
	ctx context.Context,
	tx pgx.Tx,
	selection SpacePlugin,
	documents []pluginmarket.WorkflowDocument,
	validate func(string) error,
) error {
	// Serialize name selection in one tenant so two Spaces installing the same package cannot
	// choose the same fallback name between the lookup and insert.
	if _, e := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 2))`,
		"workflow-plugin-import:"+selection.TenantID); e != nil {
		return fmt.Errorf("lock workflow plugin imports: %w", e)
	}
	for _, document := range documents {
		if e := validate(document.Graph); e != nil {
			return fmt.Errorf("%w: %s: %w", ErrPluginWorkflowInvalid, document.Path, e)
		}
		var importedWorkflow, importedSnapshot string
		e := tx.QueryRow(ctx, `
			SELECT i.workflow_id::text, i.snapshot_id::text
			FROM v2_workflow_plugin_imports i
			JOIN v2_workflows w ON w.id = i.workflow_id AND w.deleted_at IS NULL
			WHERE i.selection_id = $1 AND i.document_path = $2`,
			selection.ID, document.Path).Scan(&importedWorkflow, &importedSnapshot)
		switch {
		case e == nil:
			if e := refreshWorkflowPluginImport(ctx, tx, selection, importedWorkflow, importedSnapshot, document); e != nil {
				return e
			}
			continue
		case !errors.Is(e, pgx.ErrNoRows):
			return fmt.Errorf("read workflow import: %w", e)
		}
		if _, e := tx.Exec(ctx, `
			DELETE FROM v2_workflow_plugin_imports
			WHERE selection_id = $1 AND document_path = $2`, selection.ID, document.Path); e != nil {
			return fmt.Errorf("clear stale workflow import: %w", e)
		}
		name, e := uniqueWorkflowPluginName(ctx, tx, selection.TenantID, document.Name)
		if e != nil {
			return e
		}
		var workflowID string
		if e := tx.QueryRow(ctx, `
			INSERT INTO v2_workflows (tenant_id, name) VALUES ($1, $2)
			RETURNING id::text`, selection.TenantID, name).Scan(&workflowID); e != nil {
			return fmt.Errorf("create workflow: %w", e)
		}
		if _, e := tx.Exec(ctx, `
			INSERT INTO v2_workflow_snapshots (tenant_id, workflow_id, version, graph, updated_at)
			VALUES ($1, $2, $3, $4, now())`,
			selection.TenantID, workflowID, WorkflowDraftVersion, document.Graph); e != nil {
			return fmt.Errorf("create workflow draft: %w", e)
		}
		var snapshotID string
		if e := tx.QueryRow(ctx, `
			INSERT INTO v2_workflow_snapshots (tenant_id, workflow_id, version, graph)
			VALUES ($1, $2, 'v1', $3)
			RETURNING id::text`, selection.TenantID, workflowID, document.Graph).Scan(&snapshotID); e != nil {
			return fmt.Errorf("publish workflow: %w", e)
		}
		if _, e := tx.Exec(ctx, `
			UPDATE v2_workflows SET published_snapshot_id = $3, updated_at = now()
			WHERE tenant_id = $1 AND id = $2`, selection.TenantID, workflowID, snapshotID); e != nil {
			return fmt.Errorf("activate workflow: %w", e)
		}
		if _, e := tx.Exec(ctx, `
			INSERT INTO v2_workflow_plugin_imports (selection_id, document_path, workflow_id, snapshot_id)
			VALUES ($1, $2, $3, $4)`,
			selection.ID, document.Path, workflowID, snapshotID); e != nil {
			return fmt.Errorf("record workflow import: %w", e)
		}
	}
	return nil
}

// refreshWorkflowPluginImport applies an upgraded package document to an imported workflow only
// while that workflow is still the untouched copy the previous package imported. The draft and
// published pointer must both still be the import; otherwise the user has saved or published an
// edit, and replacing it would discard user data. An update freezes a new published version so
// runs already frozen on the previous version keep their graph.
func refreshWorkflowPluginImport(
	ctx context.Context,
	tx pgx.Tx,
	selection SpacePlugin,
	workflowID, snapshotID string,
	document pluginmarket.WorkflowDocument,
) error {
	var publishedID, draftGraph, importedGraph string
	e := tx.QueryRow(ctx, `
		SELECT COALESCE(w.published_snapshot_id::text, ''), d.graph, s.graph
		FROM v2_workflows w
		JOIN v2_workflow_snapshots d
			ON d.workflow_id = w.id AND d.version = $3 AND d.deleted_at IS NULL
		JOIN v2_workflow_snapshots s
			ON s.workflow_id = w.id AND s.id = $4 AND s.deleted_at IS NULL
		WHERE w.tenant_id = $1 AND w.id = $2 AND w.deleted_at IS NULL
		FOR UPDATE OF w`, selection.TenantID, workflowID, WorkflowDraftVersion, snapshotID).
		Scan(&publishedID, &draftGraph, &importedGraph)
	if errors.Is(e, pgx.ErrNoRows) {
		return nil
	}
	if e != nil {
		return fmt.Errorf("read imported workflow: %w", e)
	}
	if publishedID != snapshotID || draftGraph != importedGraph || document.Graph == importedGraph {
		return nil
	}
	if _, e := tx.Exec(ctx, `
		UPDATE v2_workflow_snapshots SET graph = $3, updated_at = now()
		WHERE tenant_id = $1 AND workflow_id = $2 AND version = $4 AND deleted_at IS NULL`,
		selection.TenantID, workflowID, document.Graph, WorkflowDraftVersion); e != nil {
		return fmt.Errorf("update imported workflow draft: %w", e)
	}
	version, e := nextWorkflowPluginVersion(ctx, tx, selection.TenantID, workflowID)
	if e != nil {
		return e
	}
	var publishedSnapshot string
	if e := tx.QueryRow(ctx, `
		INSERT INTO v2_workflow_snapshots (tenant_id, workflow_id, version, graph)
		VALUES ($1, $2, $3, $4)
		RETURNING id::text`,
		selection.TenantID, workflowID, version, document.Graph).Scan(&publishedSnapshot); e != nil {
		return fmt.Errorf("publish imported workflow upgrade: %w", e)
	}
	if _, e := tx.Exec(ctx, `
		UPDATE v2_workflows SET published_snapshot_id = $3, updated_at = now()
		WHERE tenant_id = $1 AND id = $2`,
		selection.TenantID, workflowID, publishedSnapshot); e != nil {
		return fmt.Errorf("activate imported workflow upgrade: %w", e)
	}
	if _, e := tx.Exec(ctx, `
		UPDATE v2_workflow_plugin_imports SET snapshot_id = $3
		WHERE selection_id = $1 AND document_path = $2`,
		selection.ID, document.Path, publishedSnapshot); e != nil {
		return fmt.Errorf("record workflow upgrade: %w", e)
	}
	return nil
}

// nextWorkflowPluginVersion returns the first unused sequential published version (v2, v3, ...).
func nextWorkflowPluginVersion(ctx context.Context, tx pgx.Tx, tenant, workflowID string) (string, error) {
	for number := 2; ; number++ {
		version := fmt.Sprintf("v%d", number)
		var exists bool
		if e := tx.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM v2_workflow_snapshots
				WHERE tenant_id = $1 AND workflow_id = $2 AND version = $3 AND deleted_at IS NULL)`,
			tenant, workflowID, version).Scan(&exists); e != nil {
			return "", fmt.Errorf("check imported workflow version: %w", e)
		}
		if !exists {
			return version, nil
		}
	}
}

// uniqueWorkflowPluginName keeps the imported definition independent from an existing workflow
// with the same name. It preserves the document name when free; conflicting imports become
// "（插件）", then "（插件 2）", and so on.
func uniqueWorkflowPluginName(ctx context.Context, tx pgx.Tx, tenant, base string) (string, error) {
	base = strings.TrimSpace(base)
	if base == "" {
		base = "工作流"
	}
	for attempt := 0; ; attempt++ {
		candidate := base
		if attempt > 0 {
			suffix := "（插件）"
			if attempt > 1 {
				suffix = fmt.Sprintf("（插件 %d）", attempt)
			}
			maxBaseRunes := 200 - utf8.RuneCountInString(suffix)
			runes := []rune(base)
			if len(runes) > maxBaseRunes {
				runes = runes[:maxBaseRunes]
			}
			candidate = string(runes) + suffix
		}
		var exists bool
		if e := tx.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM v2_workflows
				WHERE tenant_id = $1 AND lower(name) = lower($2) AND deleted_at IS NULL)`,
			tenant, candidate).Scan(&exists); e != nil {
			return "", fmt.Errorf("check workflow name: %w", e)
		}
		if !exists {
			return candidate, nil
		}
	}
}

// RemoveSpacePlugin records an owner's removal and settles the simplified terminal state. A Skill
// selection that no project ever received is deleted; one a project Node already holds is kept as a
// removed selection so the reconciler can tell the Node to remove the package (see DeleteSettledSkillRemovals).
func (s *Store) RemoveSpacePlugin(ctx context.Context, tenant, space, caller, path, key string, in PluginRemoveInput) (SpacePlugin, error) {
	var out SpacePlugin
	e := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if e := requireSpace(ctx, tx, tenant, space); e != nil {
			return e
		}
		if replay, ok, e := replayPluginMutation(ctx, tx, tenant, caller, "DELETE", path, key, pluginRemoveHash(in)); e != nil || ok {
			out = replay
			return e
		}
		namespace, identifier, ok := pluginIdentity(in.Identifier)
		if !ok {
			return ErrInvalidPluginID
		}
		old, e := querySpacePlugin(ctx, tx, space, namespace, identifier, true)
		if e != nil {
			if errors.Is(e, ErrNotFound) {
				return ErrPluginNotInstalled
			}
			return e
		}
		if old.Version != in.Version {
			return ErrConflict
		}
		if old.Kind == "agent" && old.RequestedByUserID == nil {
			return ErrPluginRuntimeManaged
		}
		if old.Kind == "skill" {
			// A Skill selection is the whole install, but a project Node that already received the
			// package must be told to remove it: keep the row as a removed selection so SkillJobs
			// can raise a removal job and drop the market-skills.json link. When no project ever
			// confirmed delivery there is nothing to notify, so the row is deleted immediately and
			// the Skills page and workflow Agent picker drop it as they did before.
			var outstanding bool
			if e := tx.QueryRow(ctx, `
				SELECT EXISTS(
					SELECT 1 FROM v2_skill_results r
					WHERE r.selection_id = $1
						AND (r.installed_version IS NOT NULL OR r.state IN ('applying','failed'))
				)`, old.ID).Scan(&outstanding); e != nil {
				return fmt.Errorf("check skill delivery: %w", e)
			}
			if !outstanding {
				if _, e := tx.Exec(ctx, `DELETE FROM v2_space_agents WHERE id = $1`, old.ID); e != nil {
					return fmt.Errorf("remove skill selection: %w", e)
				}
				out = old
				out.DesiredState = "removed"
				out.ObservedState = "removed"
				out.ObservedVersion = nil
				out.InstallError = nil
				out.RequestedByUserID = &caller
				out.Version++
				out.UpdatedAt = time.Now()
				return recordPluginMutation(ctx, tx, tenant, caller, "DELETE", path, key, pluginRemoveHash(in), out)
			}
			// A project Node still holds the package: fall through to the generic removal state and
			// let the skill reconciler remove it, then the settled row is cleaned up.
		}
		if _, e := tx.Exec(ctx, `
			WITH removal_state AS (
				SELECT a.id, EXISTS(
					SELECT 1 FROM v2_skill_results r
					WHERE r.selection_id = a.id
						AND (r.installed_version IS NOT NULL OR r.state IN ('applying','failed'))
				) AS outstanding
				FROM v2_space_agents a
				WHERE a.space_id = $1 AND a.tenant_id = $2 AND a.source_namespace = $3 AND a.identifier = $5
			)
			UPDATE v2_space_agents a
			SET desired_state = 'removed',
				observed_state = CASE WHEN removal_state.outstanding THEN 'removing' ELSE 'removed' END,
				observed_version = CASE WHEN removal_state.outstanding THEN a.observed_version ELSE NULL END,
				install_error = NULL, requested_by_user_id = $4, version = version + 1, updated_at = now()
			FROM removal_state
			WHERE a.id = removal_state.id`,
			space, tenant, namespace, caller, identifier); e != nil {
			return fmt.Errorf("remove plugin selection: %w", e)
		}
		out, e = querySpacePlugin(ctx, tx, space, namespace, identifier, false)
		if e != nil {
			return e
		}
		return recordPluginMutation(ctx, tx, tenant, caller, "DELETE", path, key, pluginRemoveHash(in), out)
	})
	return out, e
}

// requireSpace checks that the space is a live space of the tenant. A space elsewhere answers
// ErrForbidden, as it always has, so a caller cannot tell another tenant's space from a missing one.
func requireSpace(ctx context.Context, q dbTX, tenant, space string) error {
	var owned bool
	if e := q.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM v2_spaces
			WHERE id = $1 AND tenant_id = $2 AND archived_at IS NULL)`, space, tenant).Scan(&owned); e != nil {
		return fmt.Errorf("look up space: %w", e)
	}
	if !owned {
		return ErrForbidden
	}
	return nil
}

func querySpacePlugins(ctx context.Context, q dbTX, space string, runtimeAgentsOnly bool) ([]SpacePlugin, error) {
	query := `
		SELECT id::text, space_id::text, tenant_id::text, source_namespace, identifier, kind,
			desired_state, desired_version, observed_state, observed_version,
			requested_by_user_id::text, install_error, version, created_at, updated_at
		FROM v2_space_agents
		WHERE space_id = $1 AND status = 'active'`
	if runtimeAgentsOnly {
		query += ` AND kind = 'agent' AND observed_state = 'installed' AND requested_by_user_id IS NULL`
	}
	query += ` ORDER BY source_namespace, identifier`
	rows, e := q.Query(ctx, query, space)
	if e != nil {
		return nil, fmt.Errorf("list space plugins: %w", e)
	}
	defer rows.Close()
	items := []SpacePlugin{}
	for rows.Next() {
		item, e := scanSpacePlugin(rows)
		if e != nil {
			return nil, e
		}
		items = append(items, item)
	}
	if e := rows.Err(); e != nil {
		return nil, fmt.Errorf("iterate space plugins: %w", e)
	}
	return items, nil
}

func querySpacePlugin(ctx context.Context, q dbTX, space, namespace, identifier string, lock bool) (SpacePlugin, error) {
	query := `
		SELECT id::text, space_id::text, tenant_id::text, source_namespace, identifier, kind,
			desired_state, desired_version, observed_state, observed_version,
			requested_by_user_id::text, install_error, version, created_at, updated_at
		FROM v2_space_agents
		WHERE space_id = $1 AND source_namespace = $2 AND identifier = $3`
	if lock {
		query += ` FOR UPDATE`
	}
	item, e := scanSpacePlugin(q.QueryRow(ctx, query, space, namespace, identifier))
	if errors.Is(e, pgx.ErrNoRows) {
		return SpacePlugin{}, ErrNotFound
	}
	if e != nil {
		return SpacePlugin{}, fmt.Errorf("read space plugin: %w", e)
	}
	return item, nil
}

func scanSpacePlugin(row pgx.Row) (SpacePlugin, error) {
	var item SpacePlugin
	if e := row.Scan(&item.ID, &item.SpaceID, &item.TenantID, &item.SourceNamespace,
		&item.Identifier, &item.Kind, &item.DesiredState, &item.DesiredVersion,
		&item.ObservedState, &item.ObservedVersion, &item.RequestedByUserID,
		&item.InstallError, &item.Version, &item.CreatedAt, &item.UpdatedAt); e != nil {
		return SpacePlugin{}, e
	}
	return item, nil
}

func pluginIdentity(id string) (namespace, identifier string, ok bool) {
	parts := strings.Split(id, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func pluginInstallHash(in PluginInstallInput) []byte {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%d", in.Identifier, in.PluginVersion, in.Version)))
	return sum[:]
}

func pluginRemoveHash(in PluginRemoveInput) []byte {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d", in.Identifier, in.Version)))
	return sum[:]
}

func replayPluginMutation(ctx context.Context, tx pgx.Tx, tenant, user, method, path, key string, hash []byte) (SpacePlugin, bool, error) {
	if _, e := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
		strings.Join([]string{tenant, user, method, path, key}, "|")); e != nil {
		return SpacePlugin{}, false, fmt.Errorf("lock plugin idempotency key: %w", e)
	}
	var storedHash, response []byte
	e := tx.QueryRow(ctx, `
		SELECT request_hash, response FROM v2_plugin_mutations
		WHERE tenant_id = $1 AND user_id = $2 AND method = $3 AND path = $4 AND idempotency_key = $5`,
		tenant, user, method, path, key).Scan(&storedHash, &response)
	if errors.Is(e, pgx.ErrNoRows) {
		return SpacePlugin{}, false, nil
	}
	if e != nil {
		return SpacePlugin{}, false, fmt.Errorf("read plugin idempotency record: %w", e)
	}
	if !bytes.Equal(storedHash, hash) {
		return SpacePlugin{}, false, ErrIdempotencyConflict
	}
	var stored struct {
		Resource SpacePlugin `json:"resource"`
	}
	if e := json.Unmarshal(response, &stored); e != nil {
		return SpacePlugin{}, false, fmt.Errorf("decode plugin idempotency response: %w", e)
	}
	return stored.Resource, true, nil
}

func recordPluginMutation(ctx context.Context, tx pgx.Tx, tenant, user, method, path, key string, hash []byte, resource SpacePlugin) error {
	response, e := json.Marshal(map[string]any{"resource": resource})
	if e != nil {
		return fmt.Errorf("encode plugin idempotency response: %w", e)
	}
	if _, e := tx.Exec(ctx, `
		INSERT INTO v2_plugin_mutations
			(tenant_id, user_id, method, path, idempotency_key, request_hash, response)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		tenant, user, method, path, key, hash, response); e != nil {
		return fmt.Errorf("record plugin idempotency response: %w", e)
	}
	return nil
}
