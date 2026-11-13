package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
)

// Project is a tenant-shared, prepared repository snapshot. MirrorPath is backend-only and
// must not be copied into public HTTP responses. Preparing the mirror belongs to the runtime.
type Project struct {
	SpaceID       string
	ID            string
	OwnerUserID   string
	Name          string
	RepositoryURL string
	Branch        string
	Commit        string
	MirrorPath    string
	SnapshotRef   string
	CreatedAt     time.Time
}

// CreateProject retains a prepared project in the caller's tenant; tenant access is checked by the caller.
// Its ID must be a server-generated random identity, never a caller-selected ID.
func (s *Store) CreateProject(ctx context.Context, scope execution.Scope, project Project) (Project, error) {
	if project.ID == "" || project.Name == "" || project.RepositoryURL == "" || project.Commit == "" || project.MirrorPath == "" {
		return Project{}, execution.ErrInvalidRequest
	}
	if project.CreatedAt.IsZero() {
		if err := s.pool.QueryRow(ctx, `SELECT now()`).Scan(&project.CreatedAt); err != nil {
			return Project{}, err
		}
	}
	project.CreatedAt = project.CreatedAt.UTC()
	// The authenticated creator is authoritative even when a caller supplied another owner.
	project.OwnerUserID = scope.UserID
	if project.SpaceID != "" {
		if e := requireSpace(ctx, s.pool, scope.TenantID, project.SpaceID); e != nil {
			return Project{}, execution.ErrInvalidRequest
		}
	} else if e := s.pool.QueryRow(ctx, `select id from v2_spaces where tenant_id=$1 and slug=$2 and archived_at is null`, scope.TenantID, DemoSpaceSlug).Scan(&project.SpaceID); e != nil {
		return Project{}, executionError(e)
	}
	raw, err := json.Marshal(project)
	if err != nil {
		return Project{}, err
	}
	_, err = s.pool.Exec(ctx, `INSERT INTO v2_projects(id,tenant_id,data,plugin_space_id) VALUES($1,$2,$3,$4)`, project.ID, scope.TenantID, raw, project.SpaceID)
	return project, executionError(err)
}

// GetProject returns a project of the tenant; tenant access is checked by the caller.
func (s *Store) GetProject(ctx context.Context, scope execution.Scope, id string) (Project, error) {
	var raw []byte
	if err := s.pool.QueryRow(ctx, `SELECT data FROM v2_projects WHERE id=$1 AND tenant_id=$2::uuid`, id, scope.TenantID).Scan(&raw); err != nil {
		return Project{}, executionError(err)
	}
	var project Project
	err := json.Unmarshal(raw, &project)
	return project, err
}

// ListProjects lists the tenant's prepared projects in stable name/identity order.
func (s *Store) ListProjects(ctx context.Context, scope execution.Scope) ([]Project, error) {
	rows, err := s.pool.Query(ctx, `SELECT data FROM v2_projects WHERE tenant_id=$1::uuid ORDER BY data->>'Name',id`, scope.TenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	projects := []Project{}
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var project Project
		if err := json.Unmarshal(raw, &project); err != nil {
			return nil, err
		}
		projects = append(projects, project)
	}
	return projects, rows.Err()
}

// RecentProjects lists up to limit projects across tenants, most recently active first: a
// project's activity is its latest chat conversation, or its creation when it has none. It is
// backend-only (it picks which Nodes to start ahead of use) and never reaches an HTTP response.
func (s *Store) RecentProjects(ctx context.Context, limit int) ([]Project, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT p.data FROM v2_projects p
		LEFT JOIN LATERAL (SELECT max(c.created_at) AS at FROM v2_chat_conversations c
			WHERE c.tenant_id=p.tenant_id AND c.project_id=p.id) c ON true
		ORDER BY greatest(c.at, (p.data->>'CreatedAt')::timestamptz) DESC NULLS LAST, p.id
		LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	projects := []Project{}
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var project Project
		if err := json.Unmarshal(raw, &project); err != nil {
			return nil, err
		}
		projects = append(projects, project)
	}
	return projects, rows.Err()
}
