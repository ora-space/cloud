// Package store keeps v2-server's state in PostgreSQL.
package store

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed schema.sql
var schema string

// ErrNotFound reports a missing or expired row.
var ErrNotFound = errors.New("not found")

// Store is the v2-server database.
type Store struct{ pool *pgxpool.Pool }

// Open connects to url, creating its database when missing, and applies the schema.
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

// OpenConfig connects with a parsed config and applies the schema; tests use it to pick a schema.
func OpenConfig(ctx context.Context, config *pgxpool.Config) (*Store, error) {
	pool, e := pgxpool.NewWithConfig(ctx, config)
	if e != nil {
		return nil, fmt.Errorf("connect: %w", e)
	}
	if _, e := pool.Exec(ctx, schema); e != nil {
		pool.Close()
		return nil, fmt.Errorf("apply schema: %w", e)
	}
	return &Store{pool: pool}, nil
}

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

// Logout ends the session; ending an unknown one is not an error.
func (s *Store) Logout(ctx context.Context, token string) error {
	if _, e := s.pool.Exec(ctx, `DELETE FROM sessions WHERE token_hash = $1`, hash(token)); e != nil {
		return fmt.Errorf("delete session: %w", e)
	}
	return nil
}

// SessionUser returns the active user behind a live session, or ErrNotFound.
func (s *Store) SessionUser(ctx context.Context, token string) (User, error) {
	var u User
	e := s.pool.QueryRow(ctx, `
		SELECT u.id, u.display_name, u.status, u.version, u.created_at, u.deleted_at
		FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = $1 AND s.expires_at > now() AND u.deleted_at IS NULL`, hash(token)).
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
