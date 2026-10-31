package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// WorkflowDraftVersion is the reserved snapshot version naming the one mutable document every
// workflow owns. A published version may never carry this spelling.
const WorkflowDraftVersion = "draft"

var (
	// ErrWorkflowNameConflict reports another live workflow with the same name.
	ErrWorkflowNameConflict = errors.New("workflow name conflict")
	// ErrWorkflowVersionExists reports another live snapshot using the requested version.
	ErrWorkflowVersionExists = errors.New("workflow version exists")
	// ErrWorkflowSnapshotNotFound reports a snapshot that is missing, deleted, or owned elsewhere.
	ErrWorkflowSnapshotNotFound = errors.New("workflow snapshot not found")
	// ErrWorkflowDraftSource reports an operation that cannot use the draft as its source.
	ErrWorkflowDraftSource = errors.New("workflow draft is not a valid source")
	// ErrWorkflowDraftDeletion reports an attempt to delete the draft.
	ErrWorkflowDraftDeletion = errors.New("workflow draft cannot be deleted")
	// ErrWorkflowActiveVersion reports an attempt to delete the published version.
	ErrWorkflowActiveVersion = errors.New("workflow version is active")
	// ErrWorkflowDocumentInvalid wraps a document the workflow domain refused to freeze.
	ErrWorkflowDocumentInvalid = errors.New("workflow document is not executable")
)

// Workflow is the definition head: identity, name, and the published snapshot future runs freeze.
type Workflow struct {
	ID                  string
	TenantID            string
	Namespace           string
	Name                string
	PublishedSnapshotID *string
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

// WorkflowSummary is a library row: the definition without any graph content.
type WorkflowSummary struct {
	ID               string
	Namespace        string
	Name             string
	PublishedVersion *string
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// WorkflowSnapshot is one stored document. The draft is mutable; published versions are frozen and
// keep UpdatedAt unset, as desktop's contract expects.
type WorkflowSnapshot struct {
	ID         string
	TenantID   string
	WorkflowID string
	Version    string
	Graph      string
	CreatedAt  time.Time
	UpdatedAt  *time.Time
}

// WorkflowVersion is one published version without its graph.
type WorkflowVersion struct {
	ID        string
	Version   string
	CreatedAt time.Time
}

const workflowColumns = "id, tenant_id, namespace, name, published_snapshot_id, created_at, updated_at"

const workflowSnapshotColumns = "id, tenant_id, workflow_id, version, graph, created_at, updated_at"

// workflowError maps the constraints the authoring model relies on and keeps unexpected failures
// visible with their operation context.
func workflowError(op string, err error) error {
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "23505" {
		switch pg.ConstraintName {
		case "v2_workflows_live_name":
			return ErrWorkflowNameConflict
		case "v2_workflow_snapshots_live_version", "v2_workflow_snapshots_one_draft":
			return ErrWorkflowVersionExists
		}
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	return nil
}

func scanWorkflow(row pgx.Row) (Workflow, error) {
	var w Workflow
	err := row.Scan(&w.ID, &w.TenantID, &w.Namespace, &w.Name, &w.PublishedSnapshotID, &w.CreatedAt, &w.UpdatedAt)
	return w, workflowError("scan workflow", err)
}

func scanWorkflowSnapshot(row pgx.Row) (WorkflowSnapshot, error) {
	var s WorkflowSnapshot
	err := row.Scan(&s.ID, &s.TenantID, &s.WorkflowID, &s.Version, &s.Graph, &s.CreatedAt, &s.UpdatedAt)
	err = workflowError("scan workflow snapshot", err)
	if errors.Is(err, ErrNotFound) {
		return WorkflowSnapshot{}, ErrWorkflowSnapshotNotFound
	}
	return s, err
}

// Workflows lists the tenant's live definitions newest created first; ties fall back to the id so
// the order stays stable for tests and pagination-free lists.
func (s *Store) Workflows(ctx context.Context, tenant string) ([]WorkflowSummary, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT w.id, w.namespace, w.name, published.version, w.created_at, w.updated_at
		FROM v2_workflows w
		LEFT JOIN v2_workflow_snapshots published
			ON published.id = w.published_snapshot_id AND published.deleted_at IS NULL
		WHERE w.tenant_id = $1 AND w.deleted_at IS NULL
		ORDER BY w.created_at DESC, w.id DESC`, tenant)
	if err != nil {
		return nil, workflowError("list workflows", err)
	}
	summaries, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (WorkflowSummary, error) {
		var w WorkflowSummary
		err := row.Scan(&w.ID, &w.Namespace, &w.Name, &w.PublishedVersion, &w.CreatedAt, &w.UpdatedAt)
		return w, err
	})
	if err != nil {
		return nil, workflowError("list workflows", err)
	}
	return summaries, nil
}

// Workflow reads one live definition in the tenant.
func (s *Store) Workflow(ctx context.Context, tenant, id string) (Workflow, error) {
	return scanWorkflow(s.pool.QueryRow(ctx, "SELECT "+workflowColumns+`
		FROM v2_workflows WHERE tenant_id = $1 AND id = $2 AND deleted_at IS NULL`, tenant, id))
}

// CreateWorkflow stores a definition and its draft in one transaction, so a definition never
// exists without the document the editor saves into.
func (s *Store) CreateWorkflow(ctx context.Context, tenant, name, graph string) (Workflow, WorkflowSnapshot, error) {
	var w Workflow
	var draft WorkflowSnapshot
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		w, err = scanWorkflow(tx.QueryRow(ctx, `
			INSERT INTO v2_workflows (tenant_id, name) VALUES ($1, $2)
			RETURNING `+workflowColumns, tenant, name))
		if err != nil {
			return err
		}
		draft, err = scanWorkflowSnapshot(tx.QueryRow(ctx, `
			INSERT INTO v2_workflow_snapshots (tenant_id, workflow_id, version, graph, updated_at)
			VALUES ($1, $2, $3, $4, now()) RETURNING `+workflowSnapshotColumns,
			tenant, w.ID, WorkflowDraftVersion, graph))
		return err
	})
	return w, draft, err
}

// UpdateWorkflowName replaces the name; the unique index refuses a duplicate live name.
func (s *Store) UpdateWorkflowName(ctx context.Context, tenant, id, name string) (Workflow, error) {
	return scanWorkflow(s.pool.QueryRow(ctx, `
		UPDATE v2_workflows SET name = $3, updated_at = now()
		WHERE tenant_id = $1 AND id = $2 AND deleted_at IS NULL
		RETURNING `+workflowColumns, tenant, id, name))
}

// UpdateWorkflowDraft replaces the draft graph. Saves stay unvalidated so an unfinished canvas can
// be kept; only publish freezes and validates a document. As in desktop, the definition's
// updated_at does not move: the draft's own timestamp is what the editor tracks.
func (s *Store) UpdateWorkflowDraft(ctx context.Context, tenant, id, graph string) (WorkflowSnapshot, error) {
	var draft WorkflowSnapshot
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockWorkflow(ctx, tx, tenant, id); err != nil {
			return err
		}
		var err error
		draft, err = scanWorkflowSnapshot(tx.QueryRow(ctx, `
			UPDATE v2_workflow_snapshots SET graph = $3, updated_at = now()
			WHERE tenant_id = $1 AND workflow_id = $2 AND version = $4 AND deleted_at IS NULL
			RETURNING `+workflowSnapshotColumns, tenant, id, graph, WorkflowDraftVersion))
		return err
	})
	return draft, err
}

// WorkflowDraft reads the mutable snapshot of a live definition.
func (s *Store) WorkflowDraft(ctx context.Context, tenant, id string) (WorkflowSnapshot, error) {
	return scanWorkflowSnapshot(s.pool.QueryRow(ctx, "SELECT "+workflowSnapshotColumns+`
		FROM v2_workflow_snapshots
		WHERE tenant_id = $1 AND workflow_id = $2 AND version = $3 AND deleted_at IS NULL`,
		tenant, id, WorkflowDraftVersion))
}

// WorkflowSnapshot reads one snapshot by identity, scoped to its owning definition.
func (s *Store) WorkflowSnapshot(ctx context.Context, tenant, id, snapshotID string) (WorkflowSnapshot, error) {
	return scanWorkflowSnapshot(s.pool.QueryRow(ctx, "SELECT "+workflowSnapshotColumns+`
		FROM v2_workflow_snapshots
		WHERE tenant_id = $1 AND workflow_id = $2 AND id = $3 AND deleted_at IS NULL`,
		tenant, id, snapshotID))
}

// PublishWorkflow freezes the draft as a new immutable version, makes it the published one, and
// returns the frozen snapshot. Reading the draft, validating it, and inserting the frozen copy
// happen under one row lock, so the version a run will execute is exactly the validated document.
func (s *Store) PublishWorkflow(
	ctx context.Context,
	tenant, id, version string,
	validate func(graph string) error,
) (WorkflowSnapshot, error) {
	var frozen WorkflowSnapshot
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockWorkflow(ctx, tx, tenant, id); err != nil {
			return err
		}
		var graph string
		err := tx.QueryRow(ctx, `
			SELECT graph FROM v2_workflow_snapshots
			WHERE tenant_id = $1 AND workflow_id = $2 AND version = $3 AND deleted_at IS NULL
			FOR UPDATE`, tenant, id, WorkflowDraftVersion).Scan(&graph)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrWorkflowSnapshotNotFound
		}
		if err != nil {
			return workflowError("read draft", err)
		}
		if validate != nil {
			if err := validate(graph); err != nil {
				return fmt.Errorf("%w: %w", ErrWorkflowDocumentInvalid, err)
			}
		}
		frozen, err = scanWorkflowSnapshot(tx.QueryRow(ctx, `
			INSERT INTO v2_workflow_snapshots (tenant_id, workflow_id, version, graph)
			VALUES ($1, $2, $3, $4) RETURNING `+workflowSnapshotColumns,
			tenant, id, version, graph))
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
			UPDATE v2_workflows SET published_snapshot_id = $3, updated_at = $4
			WHERE tenant_id = $1 AND id = $2`, tenant, id, frozen.ID, frozen.CreatedAt)
		return workflowError("activate published version", err)
	})
	return frozen, err
}

// RollbackWorkflow copies a published version's graph into the draft without moving the published
// pointer, so existing runs keep the version they froze.
func (s *Store) RollbackWorkflow(ctx context.Context, tenant, id, snapshotID string) (WorkflowSnapshot, error) {
	return s.syncDraftFromVersion(ctx, tenant, id, snapshotID, false)
}

// ActivateWorkflow makes an existing published version the one future runs freeze and copies its
// graph into the draft, so the editor and the run target describe the same document.
func (s *Store) ActivateWorkflow(ctx context.Context, tenant, id, snapshotID string) (WorkflowSnapshot, error) {
	return s.syncDraftFromVersion(ctx, tenant, id, snapshotID, true)
}

func (s *Store) syncDraftFromVersion(ctx context.Context, tenant, id, snapshotID string, activate bool) (WorkflowSnapshot, error) {
	var draft WorkflowSnapshot
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockWorkflow(ctx, tx, tenant, id); err != nil {
			return err
		}
		var source WorkflowSnapshot
		err := tx.QueryRow(ctx, "SELECT "+workflowSnapshotColumns+`
			FROM v2_workflow_snapshots
			WHERE tenant_id = $1 AND workflow_id = $2 AND id = $3 AND deleted_at IS NULL
			FOR UPDATE`, tenant, id, snapshotID).
			Scan(&source.ID, &source.TenantID, &source.WorkflowID, &source.Version, &source.Graph, &source.CreatedAt, &source.UpdatedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrWorkflowSnapshotNotFound
		}
		if err != nil {
			return workflowError("read source version", err)
		}
		if source.Version == WorkflowDraftVersion {
			return ErrWorkflowDraftSource
		}
		draft, err = scanWorkflowSnapshot(tx.QueryRow(ctx, `
			UPDATE v2_workflow_snapshots SET graph = $3, updated_at = now()
			WHERE tenant_id = $1 AND workflow_id = $2 AND version = $4 AND deleted_at IS NULL
			RETURNING `+workflowSnapshotColumns, tenant, id, source.Graph, WorkflowDraftVersion))
		if err != nil {
			return err
		}
		if activate {
			if _, err := tx.Exec(ctx, `
				UPDATE v2_workflows SET published_snapshot_id = $3, updated_at = now()
				WHERE tenant_id = $1 AND id = $2`, tenant, id, source.ID); err != nil {
				return workflowError("activate version", err)
			}
		}
		return nil
	})
	return draft, err
}

// ListWorkflowVersions lists published versions newest first. The draft never appears in history.
func (s *Store) ListWorkflowVersions(ctx context.Context, tenant, id string) ([]WorkflowVersion, error) {
	if _, err := s.Workflow(ctx, tenant, id); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, version, created_at FROM v2_workflow_snapshots
		WHERE tenant_id = $1 AND workflow_id = $2 AND version <> $3 AND deleted_at IS NULL
		ORDER BY created_at DESC, id DESC`, tenant, id, WorkflowDraftVersion)
	if err != nil {
		return nil, workflowError("list workflow versions", err)
	}
	versions, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (WorkflowVersion, error) {
		var v WorkflowVersion
		err := row.Scan(&v.ID, &v.Version, &v.CreatedAt)
		return v, err
	})
	if err != nil {
		return nil, workflowError("list workflow versions", err)
	}
	return versions, nil
}

// WorkflowVersion reads one snapshot by its version string; "draft" resolves to the draft, as in
// desktop's contract.
func (s *Store) WorkflowVersion(ctx context.Context, tenant, id, version string) (WorkflowSnapshot, error) {
	if _, err := s.Workflow(ctx, tenant, id); err != nil {
		return WorkflowSnapshot{}, err
	}
	return scanWorkflowSnapshot(s.pool.QueryRow(ctx, "SELECT "+workflowSnapshotColumns+`
		FROM v2_workflow_snapshots
		WHERE tenant_id = $1 AND workflow_id = $2 AND version = $3 AND deleted_at IS NULL`,
		tenant, id, version))
}

// DeleteWorkflowSnapshot soft-deletes one published version. The draft, the published version, and
// a version a live run froze are refused; the spelling becomes reusable afterwards.
func (s *Store) DeleteWorkflowSnapshot(ctx context.Context, tenant, id, version string) (WorkflowVersion, error) {
	var deleted WorkflowVersion
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if version == WorkflowDraftVersion {
			return ErrWorkflowDraftDeletion
		}
		var published *string
		err := tx.QueryRow(ctx, `
			SELECT published_snapshot_id FROM v2_workflows
			WHERE tenant_id = $1 AND id = $2 AND deleted_at IS NULL FOR UPDATE`, tenant, id).Scan(&published)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return workflowError("lock workflow", err)
		}
		err = tx.QueryRow(ctx, `
			SELECT id, created_at FROM v2_workflow_snapshots
			WHERE tenant_id = $1 AND workflow_id = $2 AND version = $3 AND deleted_at IS NULL
			FOR UPDATE`, tenant, id, version).Scan(&deleted.ID, &deleted.CreatedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrWorkflowSnapshotNotFound
		}
		if err != nil {
			return workflowError("read version", err)
		}
		if published != nil && *published == deleted.ID {
			return ErrWorkflowActiveVersion
		}
		if _, err := tx.Exec(ctx, `
			UPDATE v2_workflow_snapshots SET deleted_at = now()
			WHERE tenant_id = $1 AND workflow_id = $2 AND id = $3`, tenant, id, deleted.ID); err != nil {
			return workflowError("delete version", err)
		}
		deleted.Version = version
		return nil
	})
	return deleted, err
}

// DeleteWorkflow soft-deletes a definition with every snapshot it owns. Nothing is hard deleted, so
// history a run or a snapshot identity refers to keeps resolving until runs start pinning it.
func (s *Store) DeleteWorkflow(ctx context.Context, tenant, id string) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockWorkflow(ctx, tx, tenant, id); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE v2_workflow_snapshots SET deleted_at = now()
			WHERE tenant_id = $1 AND workflow_id = $2 AND deleted_at IS NULL`, tenant, id); err != nil {
			return workflowError("delete workflow snapshots", err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE v2_workflows SET deleted_at = now(), updated_at = now()
			WHERE tenant_id = $1 AND id = $2`, tenant, id); err != nil {
			return workflowError("delete workflow", err)
		}
		return nil
	})
}

// lockWorkflow serializes authoring writes on one definition without holding the lock outside the
// transaction.
func lockWorkflow(ctx context.Context, tx pgx.Tx, tenant, id string) error {
	var exists bool
	err := tx.QueryRow(ctx, `
		SELECT true FROM v2_workflows
		WHERE tenant_id = $1 AND id = $2 AND deleted_at IS NULL FOR UPDATE`, tenant, id).Scan(&exists)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return workflowError("lock workflow", err)
}
