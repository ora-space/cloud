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
	ErrIdempotencyConflict      = errors.New("idempotency key reused")
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
func (s *Store) PluginCatalog(ctx context.Context, tenant, space, caller string) ([]PluginCatalogEntry, time.Time, error) {
	if _, e := requireSpaceAccess(ctx, s.pool, tenant, space, caller); e != nil {
		return nil, time.Time{}, e
	}
	rows, e := s.pool.Query(ctx, `
		SELECT data, synced_at
		FROM v2_plugin_catalog
		WHERE (data->>'marketplaceVisible')::boolean AND data->>'kind' <> 'agent'
		UNION ALL
		SELECT item, synced_at
		FROM v2_market_sources CROSS JOIN LATERAL jsonb_array_elements(snapshot) item
		WHERE space_id = $1 AND enabled
			AND COALESCE((item->>'marketplaceVisible')::boolean, false)
			AND item->>'kind' <> 'agent'
		ORDER BY 2 DESC`, space)
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

// SpacePlugins returns every live market selection in a space. Removed non-Agent rows remain in
// the response so the UI can distinguish "never selected" from "explicitly removed".
func (s *Store) SpacePlugins(ctx context.Context, tenant, space, caller string) ([]SpacePlugin, error) {
	if _, e := requireSpaceAccess(ctx, s.pool, tenant, space, caller); e != nil {
		return nil, e
	}
	return querySpacePlugins(ctx, s.pool, space, false)
}

// InstallSpacePlugin records an owner's selection and terminal install result. v2 has no generic
// plugin executor, so the simplified backend settles the selection immediately instead of
// claiming that package bytes were downloaded or executed.
func (s *Store) InstallSpacePlugin(ctx context.Context, tenant, space, caller, path, key string, in PluginInstallInput) (SpacePlugin, error) {
	var out SpacePlugin
	e := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		role, e := requireSpaceAccess(ctx, tx, tenant, space, caller)
		if e != nil {
			return e
		}
		if !isAdministrator(role) {
			return ErrForbidden
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
		var kind, version string
		var raw []byte
		e = tx.QueryRow(ctx, `
			SELECT kind, version, data FROM v2_plugin_catalog
			WHERE source_namespace = $1 AND identifier = $2 UNION ALL SELECT item->>'kind',item->>'version',item FROM v2_market_sources CROSS JOIN LATERAL jsonb_array_elements(snapshot) item WHERE space_id=$3 AND enabled AND namespace=$1 AND item->>'identifier'=$2`, namespace, identifier, space).Scan(&kind, &version, &raw)
		if errors.Is(e, pgx.ErrNoRows) {
			return ErrPluginNotFound
		}
		if e != nil {
			return fmt.Errorf("read catalog entry: %w", e)
		}
		if kind != "skill" {
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
			if in.Version != old.Version {
				return ErrConflict
			}
			if old.DesiredState == "installed" && old.ObservedState == "installed" && old.DesiredVersion == desired {
				out = old
				return recordPluginMutation(ctx, tx, tenant, caller, "POST", path, key, pluginInstallHash(in), out)
			}
			if _, e := tx.Exec(ctx, `
				UPDATE v2_space_agents
				SET name = $4, kind = $5, desired_state = 'installed', desired_version = $6,
					observed_state = 'pending', install_error = NULL,
					requested_by_user_id = $7, status = 'active', version = version + 1, updated_at = now()
				WHERE space_id = $1 AND tenant_id = $2 AND source_namespace = $3 AND identifier = $8`,
				space, tenant, namespace, entry.Title, kind, desired, caller, identifier); e != nil {
				return fmt.Errorf("update plugin selection: %w", e)
			}
		case errors.Is(oldErr, ErrNotFound):
			if in.Version != 0 {
				return ErrConflict
			}
			if _, e := tx.Exec(ctx, `
				INSERT INTO v2_space_agents
					(tenant_id, space_id, name, status, source_namespace, identifier, kind,
					 desired_state, desired_version, observed_state, observed_version, requested_by_user_id)
				VALUES ($1, $2, $3, 'active', $4, $5, $6, 'installed', $7, 'pending', NULL, $8)`,
				tenant, space, entry.Title, namespace, identifier, kind, desired, caller); e != nil {
				return fmt.Errorf("insert plugin selection: %w", e)
			}
		default:
			return oldErr
		}
		out, e = querySpacePlugin(ctx, tx, space, namespace, identifier, false)
		if e != nil {
			return e
		}
		if e := freezeSkill(ctx, tx, out, entry); e != nil {
			return e
		}
		return recordPluginMutation(ctx, tx, tenant, caller, "POST", path, key, pluginInstallHash(in), out)
	})
	return out, e
}

// RemoveSpacePlugin records an owner's removal and settles the simplified terminal state.
func (s *Store) RemoveSpacePlugin(ctx context.Context, tenant, space, caller, path, key string, in PluginRemoveInput) (SpacePlugin, error) {
	var out SpacePlugin
	e := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		role, e := requireSpaceAccess(ctx, tx, tenant, space, caller)
		if e != nil {
			return e
		}
		if !isAdministrator(role) {
			return ErrForbidden
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
		if old.Kind == "agent" {
			return ErrPluginKindNotInstallable
		}
		if _, e := tx.Exec(ctx, `
			UPDATE v2_space_agents
			SET desired_state = 'removed', observed_state = CASE WHEN EXISTS(SELECT 1 FROM v2_skill_results r WHERE r.selection_id=v2_space_agents.id AND (r.installed_version IS NOT NULL OR r.state IN ('applying','failed'))) THEN 'removing' ELSE 'removed' END,
				install_error = NULL, requested_by_user_id = $4, version = version + 1, updated_at = now()
			WHERE space_id = $1 AND tenant_id = $2 AND source_namespace = $3 AND identifier = $5`,
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

func requireSpaceAccess(ctx context.Context, q dbTX, tenant, space, caller string) (string, error) {
	role, e := tenantRole(ctx, q, tenant, caller)
	if e != nil {
		return "", ErrForbidden
	}
	var owned bool
	if e := q.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM v2_spaces
			WHERE id = $1 AND tenant_id = $2 AND archived_at IS NULL)`, space, tenant).Scan(&owned); e != nil {
		return "", fmt.Errorf("look up space: %w", e)
	}
	if !owned {
		return "", ErrForbidden
	}
	return role, nil
}

func querySpacePlugins(ctx context.Context, q dbTX, space string, agentsOnly bool) ([]SpacePlugin, error) {
	query := `
		SELECT id::text, space_id::text, tenant_id::text, source_namespace, identifier, kind,
			desired_state, desired_version, observed_state, observed_version,
			requested_by_user_id::text, install_error, version, created_at, updated_at
		FROM v2_space_agents
		WHERE space_id = $1 AND status = 'active'`
	if agentsOnly {
		query += ` AND kind = 'agent' AND observed_state = 'installed'`
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
