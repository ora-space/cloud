// Package store keeps v2-server's state in PostgreSQL.
package store

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound reports a missing or expired row.
var ErrNotFound = errors.New("not found")

// Store is the v2-server database.
type Store struct {
	pool             *pgxpool.Pool
	mcpChanged       func(tenant, plugin string)
	officialDisabled bool
	migrated         []AppliedMigration
}

// SetMCPChangeObserver registers a post-commit wakeup before the store is shared with workers.
func (s *Store) SetMCPChangeObserver(observer func(tenant, plugin string)) { s.mcpChanged = observer }

// DisableOfficialMarket configures catalog visibility before the store is shared with workers.
// It retains durable snapshots for a later re-enable, while preventing new installs from them.
func (s *Store) DisableOfficialMarket(disabled bool) { s.officialDisabled = disabled }

// OfficialMarketEnabled reports deployment configuration independently of saved catalog entries.
func (s *Store) OfficialMarketEnabled() bool { return !s.officialDisabled }

// Open connects to url, creating its database when missing, and applies pending migrations.
func Open(ctx context.Context, url string) (*Store, error) {
	config, e := pgxpool.ParseConfig(url)
	if e != nil {
		return nil, fmt.Errorf("parse database url: %w", e)
	}
	if e := ensureDatabase(ctx, config.ConnConfig); e != nil {
		return nil, e
	}
	return OpenConfig(ctx, config)
}

// OpenConfig connects with a parsed config and applies pending migrations to the connection's
// current schema; tests use it to pick a schema. It refuses a database whose migration history
// does not match this binary, and one created before versioned migrations existed.
func OpenConfig(ctx context.Context, config *pgxpool.Config) (*Store, error) {
	migrations, e := embeddedMigrations()
	if e != nil {
		return nil, e
	}
	return openWith(ctx, config, migrations)
}

// openWith is OpenConfig with an explicit migration set, so tests can exercise invalid histories.
func openWith(ctx context.Context, config *pgxpool.Config, migrations fs.FS) (*Store, error) {
	pool, e := pgxpool.NewWithConfig(ctx, config)
	if e != nil {
		return nil, fmt.Errorf("connect: %w", e)
	}
	applied, e := migrate(ctx, pool, migrations)
	if e != nil {
		pool.Close()
		return nil, fmt.Errorf("migrate database: %w", e)
	}
	return &Store{pool: pool, migrated: applied}, nil
}

// AppliedMigrations lists the migrations this Open applied, oldest first; it is empty when the
// database was already current.
func (s *Store) AppliedMigrations() []AppliedMigration { return s.migrated }

// ensureDatabase creates the target database through the server's default database, so a fresh
// local PostgreSQL works without manual setup.
func ensureDatabase(ctx context.Context, target *pgx.ConnConfig) error {
	admin := target.Copy()
	admin.Database = "postgres"
	conn, e := pgx.ConnectConfig(ctx, admin)
	if e != nil {
		// No access to the maintenance database: assume the target exists and let Open report otherwise.
		return nil
	}
	defer conn.Close(ctx)
	var exists bool
	if e := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`, target.Database).Scan(&exists); e != nil {
		return fmt.Errorf("look up database: %w", e)
	}
	if exists {
		return nil
	}
	if _, e := conn.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{target.Database}.Sanitize()); e != nil {
		return fmt.Errorf("create database %s: %w", target.Database, e)
	}
	return nil
}

// Close releases the pool.
func (s *Store) Close() { s.pool.Close() }

// Ping checks the database is reachable.
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// User mirrors the contract's User. The contract forbids extra fields, so the caller's workspace
// role travels with GET /api/v1/me/tenants instead.
type User struct {
	ID          string     `json:"id"`
	DisplayName string     `json:"displayName"`
	Status      string     `json:"status"`
	Version     int64      `json:"version"`
	CreatedAt   time.Time  `json:"createdAt"`
	DeletedAt   *time.Time `json:"deletedAt"`
}

// Tenant mirrors the contract's Tenant, with the caller's role.
type Tenant struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
	Role   string `json:"role"`
}

// DemoTenant is the tenant every development login joins.
const DemoTenant = "演示团队"

// Logout ends the web session; ending an unknown one is not an error. It never ends a desktop
// token, which only desktop logout revokes (A6 D6).
func (s *Store) Logout(ctx context.Context, token string) error {
	if _, e := s.pool.Exec(ctx, `DELETE FROM sessions WHERE token_hash = $1 AND kind = 'web'`, hash(token)); e != nil {
		return fmt.Errorf("delete session: %w", e)
	}
	return nil
}

// SessionUser returns the active user behind a live web session, or ErrNotFound. Disabling or
// deleting the account ends every session at the next request. A desktop token is not a web
// session: presented as a cookie it is unknown.
func (s *Store) SessionUser(ctx context.Context, token string) (User, error) {
	var u User
	e := s.pool.QueryRow(ctx, `
		SELECT u.id, u.display_name, u.status, u.version, u.created_at, u.deleted_at
		FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = $1 AND s.kind = 'web' AND s.expires_at > now()
			AND u.status = 'active' AND u.deleted_at IS NULL`, hash(token)).
		Scan(&u.ID, &u.DisplayName, &u.Status, &u.Version, &u.CreatedAt, &u.DeletedAt)
	if errors.Is(e, pgx.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if e != nil {
		return User{}, fmt.Errorf("look up session: %w", e)
	}
	return u, nil
}

// Tenants lists the user's tenants by name.
func (s *Store) Tenants(ctx context.Context, userID string) ([]Tenant, error) {
	rows, e := s.pool.Query(ctx, `
		SELECT t.id, t.name, t.status, m.role
		FROM memberships m JOIN tenants t ON t.id = m.tenant_id
		WHERE m.user_id = $1 ORDER BY t.name, t.id`, userID)
	if e != nil {
		return nil, fmt.Errorf("list tenants: %w", e)
	}
	tenants, e := pgx.CollectRows(rows, pgx.RowToStructByPos[Tenant])
	if e != nil {
		return nil, fmt.Errorf("list tenants: %w", e)
	}
	return tenants, nil
}

func hash(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}
