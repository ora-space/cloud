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

// CreateProject retains a prepared project after verifying current tenant membership.
// Its ID must be a server-generated random identity, never a caller-selected ID.
func (s *Store) CreateProject(ctx context.Context, scope execution.Scope, project Project) (Project, error) {
	if err := s.CheckExecutionScope(ctx, scope); err != nil {
		return Project{}, err
	}
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
		if _, e := requireSpaceAccess(ctx, s.pool, scope.TenantID, project.SpaceID, scope.UserID); e != nil {
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

// GetProject returns a shared project only to current members of its tenant.
func (s *Store) GetProject(ctx context.Context, scope execution.Scope, id string) (Project, error) {
	if err := s.CheckExecutionScope(ctx, scope); err != nil {
		return Project{}, err
	}
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
	if err := s.CheckExecutionScope(ctx, scope); err != nil {
		return nil, err
	}
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
