package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// DemoSpaceName and DemoSpaceSlug identify the one collaboration space every demo tenant gets.
const (
	DemoSpaceName = "演示空间"
	DemoSpaceSlug = "demo"
)

// DemoAgent is a real Agent plugin this demo installs in that space. Each artifact is one
// scripts/prepare-node.sh downloads and verifies by SHA-256, and the same one the chat, workflow
// and schedule runtimes load, so the space lists installed plugins rather than made-up Agents.
type DemoAgent struct {
	Name, Namespace, Identifier, Version string
}

// DemoAgents lists the demo space's plugins; the space lists them by name.
var DemoAgents = []DemoAgent{
	{Name: "OpenCode", Namespace: "official", Identifier: "ora-space.opencode", Version: "0.6.11"},
	{Name: "Claude Code", Namespace: "official", Identifier: "ora-space.claude", Version: "0.4.5"},
}

// Space mirrors the contract's SpaceListItem. v2 has no space members or settings beyond the
// tenant's membership, so Role repeats the caller's workspace role.
type Space struct {
	ID          string     `json:"id"`
	TenantID    string     `json:"tenantId"`
	Name        string     `json:"name"`
	Description string     `json:"description"`
	Slug        string     `json:"slug"`
	Role        string     `json:"role"`
	Version     int64      `json:"version"`
	CreatedBy   string     `json:"createdBy"`
	CreatedAt   time.Time  `json:"createdAt"`
	UpdatedAt   time.Time  `json:"updatedAt"`
	ArchivedAt  *time.Time `json:"archivedAt"`
}

// SpacePlugin mirrors the contract's SpacePlugin for a package selected in a space. The table
// predates the general marketplace and keeps its historical name; Kind now distinguishes agents
// from market packages, while aggregate runtime counts stay absent in this single-process backend.
type SpacePlugin struct {
	ID                string    `json:"id"`
	SpaceID           string    `json:"spaceId"`
	TenantID          string    `json:"tenantId"`
	SourceNamespace   string    `json:"sourceNamespace"`
	Identifier        string    `json:"identifier"`
	Kind              string    `json:"kind"`
	DesiredState      string    `json:"desiredState"`
	DesiredVersion    string    `json:"desiredVersion"`
	ObservedState     string    `json:"observedState"`
	ObservedVersion   *string   `json:"observedVersion"`
	RequestedByUserID *string   `json:"requestedByUserId"`
	InstallError      *string   `json:"installError"`
	Version           int64     `json:"version"`
	CreatedAt         time.Time `json:"createdAt"`
	UpdatedAt         time.Time `json:"updatedAt"`
}

// TenantSpaces returns the caller's workspace spaces. The membership check runs first so a
// non-member cannot probe which spaces a workspace holds.
func (s *Store) TenantSpaces(ctx context.Context, tenant, caller string) ([]Space, error) {
	role, e := tenantRole(ctx, s.pool, tenant, caller)
	if e != nil {
		return nil, e
	}
	rows, e := s.pool.Query(ctx, `
		SELECT id, name, description, slug, version, created_by, created_at, updated_at, archived_at
		FROM v2_spaces
		WHERE tenant_id = $1 AND archived_at IS NULL
		ORDER BY created_at, id`, tenant)
	if e != nil {
		return nil, fmt.Errorf("list tenant spaces: %w", e)
	}
	defer rows.Close()
	items := make([]Space, 0)
	for rows.Next() {
		var sp Space
		if e := rows.Scan(&sp.ID, &sp.Name, &sp.Description, &sp.Slug, &sp.Version, &sp.CreatedBy,
			&sp.CreatedAt, &sp.UpdatedAt, &sp.ArchivedAt); e != nil {
			return nil, fmt.Errorf("scan tenant space: %w", e)
		}
		sp.TenantID, sp.Role = tenant, role
		items = append(items, sp)
	}
	if e := rows.Err(); e != nil {
		return nil, fmt.Errorf("iterate tenant spaces: %w", e)
	}
	return items, nil
}

// SpaceAgentPlugins returns the system-preinstalled executable Agents in one of the caller's
// spaces. Marketplace Agent selections are deliberately excluded. A space outside the caller's
// workspace answers ErrNotFound, which the HTTP layer turns into 403 so an outsider cannot tell a
// missing space from a forbidden one.
func (s *Store) SpaceAgentPlugins(ctx context.Context, tenant, space, caller string) ([]SpacePlugin, error) {
	if _, e := tenantRole(ctx, s.pool, tenant, caller); e != nil {
		return nil, e
	}
	var owned bool
	if e := s.pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM v2_spaces
			WHERE id = $1 AND tenant_id = $2 AND archived_at IS NULL)`, space, tenant).Scan(&owned); e != nil {
		return nil, fmt.Errorf("look up space: %w", e)
	}
	if !owned {
		return nil, ErrNotFound
	}
	return querySpacePlugins(ctx, s.pool, space, true)
}

// ensureDemoSpace gives a demo tenant its space and the installed agent plugins the runtimes use.
// It runs inside the login transaction and is idempotent, so repeated logins keep one space and
// one row per plugin, and a database seeded before a plugin existed gains its row. A conflicting
// insert keeps the existing row, which is what a real install would leave behind.
func ensureDemoSpace(ctx context.Context, tx pgx.Tx, tenant, user string) error {
	if _, e := tx.Exec(ctx, `
		INSERT INTO v2_spaces (tenant_id, name, slug, description, created_by)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (tenant_id, slug) DO NOTHING`,
		tenant, DemoSpaceName, DemoSpaceSlug, "演示工作区的协作空间。", user); e != nil {
		return fmt.Errorf("ensure demo space: %w", e)
	}
	var space string
	if e := tx.QueryRow(ctx, `
		SELECT id FROM v2_spaces WHERE tenant_id = $1 AND slug = $2`, tenant, DemoSpaceSlug).Scan(&space); e != nil {
		return fmt.Errorf("read demo space: %w", e)
	}
	for _, a := range DemoAgents {
		if _, e := tx.Exec(ctx, `
			INSERT INTO v2_space_agents (tenant_id, space_id, name, source_namespace, identifier,
				desired_version, observed_version)
			VALUES ($1, $2, $3, $4, $5, $6, $6)
			ON CONFLICT (space_id, source_namespace, identifier) DO NOTHING`,
			tenant, space, a.Name, a.Namespace, a.Identifier, a.Version); e != nil {
			return fmt.Errorf("register demo agent %s: %w", a.Identifier, e)
		}
	}
	return nil
}
