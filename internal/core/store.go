// Package core implements the authoritative cloud aggregate and control contracts.
package core

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Object is a JSON resource; database column names are converted at the persistence boundary.
type Object map[string]any

// S returns the string field at key, or the empty string when the field is absent or not a string.
func (o Object) S(k string) string { v, _ := o[k].(string); return v }

// N returns the integer field at key, or zero when the field is absent or not an exact integer.
func (o Object) N(k string) int64 {
	switch v := o[k].(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case int:
		return int64(v)
	case json.Number:
		n, _ := v.Int64()
		return n
	}
	return 0
}

// B returns the boolean field at key, or false when the field is absent or not a boolean.
func (o Object) B(k string) bool { v, _ := o[k].(bool); return v }

// O returns the object field at key, or an empty object when the field is absent or not an object.
func (o Object) O(k string) Object {
	switch v := o[k].(type) {
	case map[string]any:
		return Object(v)
	case Object:
		return v
	}
	return Object{}
}

// Fault is the stable error contract. Internal database detail never reaches clients.
type Fault struct {
	Code   string `json:"code"`
	Params Object `json:"params"`
	Status int    `json:"-"`
}

func (e *Fault) Error() string       { return e.Code }
func reject(status int, code string) { panic(&Fault{Code: code, Status: status, Params: Object{}}) }
func require(ok bool, status int, code string) {
	if !ok {
		reject(status, code)
	}
}

type databaseFailure struct{ err error }

// Store is injected; there is no global database handle.
type Store struct{ Pool *sql.DB }

// NewStore obtains the injected SQL pool without creating or migrating schema.
func NewStore(db *gorm.DB) (*Store, error) {
	if db == nil {
		return nil, fmt.Errorf("database is required")
	}
	pool, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("get database pool: %w", err)
	}
	return &Store{Pool: pool}, nil
}

type transaction struct {
	tx  *sql.Tx
	ctx context.Context
}

func (t *transaction) exec(q string, args ...any) {
	if _, e := t.tx.ExecContext(t.ctx, q, args...); e != nil {
		panic(databaseFailure{e})
	}
}

func (t *transaction) list(q string, args ...any) []Object {
	// q is assembled only from package-owned SQL fragments; all external values are bound.
	rows, e := t.tx.QueryContext(t.ctx, "SELECT row_to_json(resource) FROM ("+q+") resource", args...) // #nosec G202 -- fixed SQL fragments, parameterized values.
	if e != nil {
		panic(databaseFailure{e})
	}
	defer rows.Close()
	out := []Object{}
	for rows.Next() {
		var b []byte
		if e = rows.Scan(&b); e != nil {
			panic(databaseFailure{e})
		}
		var raw Object
		if e = json.Unmarshal(b, &raw); e != nil {
			panic(databaseFailure{e})
		}
		o := Object{}
		for k, v := range raw {
			o[camel(k)] = v
		}
		out = append(out, o)
	}
	if e = rows.Err(); e != nil {
		panic(databaseFailure{e})
	}
	return out
}

func (t *transaction) one(q string, args ...any) Object {
	a := t.list(q, args...)
	if len(a) == 0 {
		return nil
	}
	return a[0]
}

func camel(s string) string {
	p := strings.Split(s, "_")
	for i := 1; i < len(p); i++ {
		if p[i] != "" {
			p[i] = strings.ToUpper(p[i][:1]) + p[i][1:]
		}
	}
	return strings.Join(p, "")
}

func jsonText(v any) string {
	b, e := json.Marshal(v)
	if e != nil {
		panic(databaseFailure{e})
	}
	return string(b)
}
func newID() string         { return uuid.NewString() }
func validID(s string) bool { _, e := uuid.Parse(s); return e == nil }

// transact serializes mutations within the single-cluster phase-one control plane.
// The lock is transaction scoped, never spans HTTP or external work. It deliberately
// trades write throughput for a simple, auditable lock order; reads use the same boundary.
func (s *Store) transact(ctx context.Context, fn func(*transaction) Object) (out Object, err error) {
	tx, e := s.Pool.BeginTx(ctx, nil)
	if e != nil {
		return nil, e
	}
	defer func() { _ = tx.Rollback() }()
	defer func() {
		if r := recover(); r != nil {
			switch v := r.(type) {
			case *Fault:
				err = v
			case databaseFailure:
				err = v.err
			default:
				panic(r)
			}
		}
	}()
	t := &transaction{tx: tx, ctx: ctx}
	t.exec("SELECT pg_advisory_xact_lock(67420911)")
	out = fn(t)
	err = tx.Commit()
	return out, err
}

//go:embed migrations/*.sql
var migrations embed.FS

// CheckSchema rejects a missing or changed migration without mutating production schema.
func (s *Store) CheckSchema(ctx context.Context) error {
	actual, err := schemaMigrations(ctx, s.Pool)
	if err != nil {
		return err
	}

	entries, e := migrations.ReadDir("migrations")
	if e != nil {
		return fmt.Errorf("read embedded migrations: %w", e)
	}
	for _, entry := range entries {
		b, e := migrations.ReadFile("migrations/" + entry.Name())
		if e != nil {
			return fmt.Errorf("read embedded migration %s: %w", entry.Name(), e)
		}
		sum := sha256.Sum256(b)
		checksum, ok := actual[entry.Name()]
		if !ok {
			return fmt.Errorf("migration %s is missing; run cloudctl migrate", entry.Name())
		}
		if checksum != hex.EncodeToString(sum[:]) {
			return fmt.Errorf("migration checksum mismatch: %s", entry.Name())
		}
		delete(actual, entry.Name())
	}
	for version := range actual {
		return fmt.Errorf("database contains migration unknown to this binary: %s", version)
	}
	return nil
}

func schemaMigrations(ctx context.Context, pool *sql.DB) (actual map[string]string, err error) {
	rows, err := pool.QueryContext(ctx, "SELECT version,checksum FROM schema_migrations ORDER BY version")
	if err != nil {
		return nil, fmt.Errorf("read schema migrations; run cloudctl migrate: %w", err)
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	actual = map[string]string{}
	for rows.Next() {
		var version, checksum string
		if err = rows.Scan(&version, &checksum); err != nil {
			return nil, fmt.Errorf("read schema migration: %w", err)
		}
		actual[version] = checksum
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("read schema migrations: %w", err)
	}
	return actual, nil
}

// Migrate applies explicit ordered SQL migrations under a database advisory lock.
func (s *Store) Migrate(ctx context.Context) error {
	_, e := s.transact(ctx, func(t *transaction) Object {
		t.exec("CREATE TABLE IF NOT EXISTS schema_migrations(version text PRIMARY KEY, checksum text NOT NULL, applied_at timestamptz NOT NULL DEFAULT now())")
		entries, err := migrations.ReadDir("migrations")
		if err != nil {
			panic(databaseFailure{err})
		}
		for _, entry := range entries {
			b, err := migrations.ReadFile("migrations/" + entry.Name())
			if err != nil {
				panic(databaseFailure{err})
			}
			sum := sha256.Sum256(b)
			hash := hex.EncodeToString(sum[:])
			old := t.one("SELECT checksum FROM schema_migrations WHERE version=$1", entry.Name())
			if old != nil {
				require(old.S("checksum") == hash, 409, "migration_checksum_mismatch")
				continue
			}
			t.exec(string(b))
			t.exec("INSERT INTO schema_migrations(version,checksum) VALUES($1,$2)", entry.Name(), hash)
		}
		return Object{"migrated": true}
	})
	return e
}

func identity(t *transaction, source, subject, name string) Object {
	require(source != "" && len(source) <= 128 && subject != "" && len(subject) <= 512 && len(name) <= 200, 401, "invalid_identity")
	u := t.one("SELECT u.* FROM users u JOIN user_identities i ON i.user_id=u.id WHERE i.source=$1 AND i.subject=$2", source, subject)
	if u == nil {
		id := newID()
		t.exec("INSERT INTO users(id,display_name,status) VALUES($1,$2,'active')", id, name)
		t.exec("INSERT INTO user_identities(user_id,source,subject) VALUES($1,$2,$3)", id, source, subject)
		u = t.one("SELECT * FROM users WHERE id=$1", id)
	}
	require(u.S("status") == "active" && u["deletedAt"] == nil, 403, "user_disabled")
	return u
}

// Bootstrap atomically provisions a tenant and its initial administrator from a deployment command.
func (s *Store) Bootstrap(ctx context.Context, name, source, subject, display string) (Object, error) {
	return s.transact(ctx, func(t *transaction) Object {
		require(name != "" && len(name) <= 200, 400, "invalid_name")
		u := identity(t, source, subject, display)
		id := newID()
		t.exec("INSERT INTO tenants(id,name,status) VALUES($1,$2,'active')", id, name)
		t.exec("INSERT INTO tenant_memberships(tenant_id,user_id,role,status) VALUES($1,$2,'admin','active')", id, u.S("id"))
		return Object{"tenantId": id, "userId": u.S("id")}
	})
}

// ConfigureCredential is deliberately a deployment-only management path, never a public secret API.
func (s *Store) ConfigureCredential(ctx context.Context, tid, owner, ref string) (Object, error) {
	return s.transact(ctx, func(t *transaction) Object {
		membership(t, tid, owner, false)
		require(strings.TrimSpace(ref) != "" && len(ref) <= 1024, 400, "invalid_secret_ref")
		id := newID()
		t.exec("INSERT INTO credential_refs(id,tenant_id,owner_user_id,purpose,secret_ref) VALUES($1,$2,$3,'git',$4)", id, tid, owner, ref)
		return Object{"id": id, "tenantId": tid, "ownerUserId": owner, "purpose": "git"}
	})
}

func membership(t *transaction, tid, uid string, admin bool) Object {
	require(validID(tid) && validID(uid), 404, "not_found")
	m := t.one("SELECT m.* FROM tenant_memberships m JOIN tenants t ON t.id=m.tenant_id JOIN users u ON u.id=m.user_id WHERE m.tenant_id=$1 AND m.user_id=$2 AND m.status='active' AND t.status='active' AND t.deleted_at IS NULL AND u.status='active' AND u.deleted_at IS NULL", tid, uid)
	require(m != nil, 403, "membership_required")
	require(!admin || m.S("role") == "admin", 403, "admin_required")
	return m
}

func project(t *transaction, tid, uid, pid string) Object {
	require(validID(pid), 404, "not_found")
	p := t.one("SELECT * FROM projects WHERE id=$1 AND tenant_id=$2 AND owner_user_id=$3 AND deleted_at IS NULL", pid, tid, uid)
	require(p != nil, 404, "not_found")
	return p
}

func workspace(t *transaction, tid, uid, wid string, admin bool) Object {
	require(validID(wid), 404, "not_found")
	q := "SELECT * FROM workspaces WHERE id=$1 AND tenant_id=$2 AND deleted_at IS NULL"
	args := []any{wid, tid}
	if !admin {
		q += " AND owner_user_id=$3"
		args = append(args, uid)
	}
	w := t.one(q, args...)
	require(w != nil, 404, "not_found")
	return w
}

func version(o Object, v int64) {
	require(v > 0, 428, "version_required")
	require(o.N("version") == v, 409, "version_conflict")
}

func idleProject(t *transaction, pid string) {
	require(t.one("SELECT id FROM operations WHERE project_id=$1 AND state IN ('queued','running','retry_wait','blocked')", pid) == nil, 409, "operation_in_progress")
}

// ErrorCode normalizes failures for HTTP without leaking SQL or credentials.
func ErrorCode(err error) *Fault {
	var f *Fault
	if errors.As(err, &f) {
		return f
	}
	return &Fault{Code: "internal_error", Params: Object{}, Status: 500}
}

func requestHash(method, path string, body Object) string {
	b := method + "\n" + path + "\n" + jsonText(body)
	h := sha256.Sum256([]byte(b))
	return fmt.Sprintf("%x", h)
}
