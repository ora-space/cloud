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
type Store struct {
	Pool *sql.DB

	// Collaboration ports (consuming-side seams; see collaboration.go). Each is nil by default
	// ("Unavailable"); dev/demo/integration wire the in-memory fixtures, production real adapters.
	Directory  CollaborationDirectory
	Context    ContextBuilder
	Dispatcher ExecutionDispatcher
	Forms      FormDescriptorProvider
	Assist     InputAssistProvider

	// Events broadcasts committed collaboration-space invalidation notices to live
	// SSE subscribers. Space association is optional (projects.space_id is nullable):
	// a space-scoped project gates visibility to active space members (the
	// resource-sharing boundary), while an unscoped project keeps owner-based
	// authorization.
	Events *SpaceHub

	// Signals carries at-most-once work hints to the lease-holding Controller's Watch stream;
	// clone requests stay durable in PostgreSQL whether or not a hint is delivered.
	Signals *ControlHub
}

// NewStore obtains the injected SQL pool without creating or migrating schema.
func NewStore(db *gorm.DB) (*Store, error) {
	if db == nil {
		return nil, fmt.Errorf("database is required")
	}
	pool, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("get database pool: %w", err)
	}
	return &Store{Pool: pool, Events: NewSpaceHub(), Signals: NewControlHub()}, nil
}

type transaction struct {
	tx  *sql.Tx
	ctx context.Context
	// collaboration ports shadowed from the Store so transaction-scoped helpers can use them.
	directory      CollaborationDirectory
	contextBuilder ContextBuilder
	forms          FormDescriptorProvider
	assist         InputAssistProvider
	// queued names operations this transaction made claimable; they are published only after commit.
	queued []string
}

func (t *transaction) exec(q string, args ...any) {
	if _, e := t.tx.ExecContext(t.ctx, q, args...); e != nil {
		panic(databaseFailure{e})
	}
}

// execRows runs a statement and returns the number of rows affected, for compare-and-set guards that
// must distinguish "claimed it" from "someone else already did".
func (t *transaction) execRows(q string, args ...any) int64 {
	res, e := t.tx.ExecContext(t.ctx, q, args...)
	if e != nil {
		panic(databaseFailure{e})
	}
	n, e := res.RowsAffected()
	if e != nil {
		panic(databaseFailure{e})
	}
	return n
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
	t := &transaction{tx: tx, ctx: ctx, directory: s.Directory, contextBuilder: s.Context, forms: s.Forms, assist: s.Assist}
	t.exec("SELECT pg_advisory_xact_lock(67420911)")
	out = fn(t)
	if err = tx.Commit(); err == nil {
		s.signalOperations(t.queued)
	}
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

// Bootstrap atomically provisions a tenant, its initial administrator, and the
// tenant's default collaboration space from a deployment command.
func (s *Store) Bootstrap(ctx context.Context, name, source, subject, display string) (Object, error) {
	return s.transact(ctx, func(t *transaction) Object {
		require(name != "" && len(name) <= 200, 400, "invalid_name")
		u := identity(t, source, subject, display)
		tenant, space := provisionTenant(t, u.S("id"), name, "Default", "default")
		return Object{"tenantId": tenant.S("id"), "userId": u.S("id"), "spaceId": space.S("id")}
	})
}

// EnsureMember resolves or provisions a user identity and guarantees an active membership in the
// given tenant, returning the user object. It exists only for the local development edge server
// (cmd/ora-web), which signs a user token for an arbitrary login subject and must attach that user
// to its bootstrap tenant before they can read the board; it is never a public HTTP path.
func (s *Store) EnsureMember(ctx context.Context, tid, source, subject, display string) (Object, error) {
	return s.transact(ctx, func(t *transaction) Object {
		require(validID(tid), 404, "not_found")
		u := identity(t, source, subject, display)
		uid := u.S("id")
		if t.one("SELECT user_id FROM tenant_memberships WHERE tenant_id=$1 AND user_id=$2", tid, uid) == nil {
			t.exec("INSERT INTO tenant_memberships(tenant_id,user_id,role,status) VALUES($1,$2,'member','active')", tid, uid)
		} else {
			t.exec("UPDATE tenant_memberships SET status='active' WHERE tenant_id=$1 AND user_id=$2 AND status<>'active'", tid, uid)
		}
		return u
	})
}

// validEmail is a deliberately lightweight registration check: a single '@' with
// non-empty local and domain parts and a dotted domain. It is not a full RFC 5322
// validator — the system has no mailbox delivery to be strict about.
func validEmail(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > 512 {
		return false
	}
	local, domain, ok := strings.Cut(s, "@")
	if !ok || local == "" || domain == "" || strings.Contains(domain, "@") {
		return false
	}
	dot := strings.LastIndexByte(domain, '.')
	return dot > 0 && dot < len(domain)-1
}

// normalizeEmail folds an address to its canonical form: trimmed and lowercase,
// so case-variant duplicates ("Alice@Example.com" vs "alice@example.com") collide
// on the single (source,subject) identity row.
func normalizeEmail(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// RegisterIdentity strictly provisions a new user identity and its active
// membership in the given tenant. Unlike EnsureMember it never reuses an existing
// identity: a duplicate (source,subject) — after email normalization — is a
// conflict (409 user_already_exists), not a silent login. It exists only for the
// local development edge server's registration flow (cmd/ora-web); it is never a
// public HTTP path. The created user holds only its own tenant membership; no
// runtime workspace, project ownership, or collaboration-space membership is
// granted by registration.
func (s *Store) RegisterIdentity(ctx context.Context, tid, source, subject, name string) (Object, error) {
	return s.transact(ctx, func(t *transaction) Object {
		require(validID(tid), 400, "invalid_input")
		require(source != "" && len(source) <= 128, 400, "invalid_input")
		subject = normalizeEmail(subject)
		require(validEmail(subject), 400, "invalid_email")
		name = strings.TrimSpace(name)
		require(name != "" && len(name) <= 200, 400, "name_required")
		// The advisory lock keeps this check + insert atomic, so a concurrent
		// duplicate cannot slip past the pre-check; the PK(source,subject) remains
		// the final integrity guard.
		require(t.one("SELECT user_id FROM user_identities WHERE source=$1 AND subject=$2", source, subject) == nil, 409, "user_already_exists")
		id := newID()
		t.exec("INSERT INTO users(id,display_name,status) VALUES($1,$2,'active')", id, name)
		t.exec("INSERT INTO user_identities(user_id,source,subject) VALUES($1,$2,$3)", id, source, subject)
		t.exec("INSERT INTO tenant_memberships(tenant_id,user_id,role,status) VALUES($1,$2,'member','active')", tid, id)
		return t.one("SELECT * FROM users WHERE id=$1", id)
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

// project loads a live project in the tenant and applies project access:
// a space-scoped project (space_id set) is reachable by any active member of
// that workspace (the resource-sharing boundary), while an unscoped (legacy)
// project keeps owner-only access. Non-members stay hidden (404, no existence
// leak), identically to the previous owner-filtered lookup.
func project(t *transaction, tid, uid, pid string) Object {
	require(validID(pid), 404, "not_found")
	p := t.one("SELECT * FROM projects WHERE id=$1 AND tenant_id=$2 AND deleted_at IS NULL", pid, tid)
	require(p != nil, 404, "not_found")
	if sid := p.S("spaceId"); sid != "" {
		require(workspaceRole(t, sid, uid) != "", 404, "not_found")
	} else if p.S("ownerUserId") != uid {
		reject(404, "not_found")
	}
	return p
}

// workspace loads a live runtime workspace in the tenant. Non-admin access
// inherits the parent project's access: a runtime workspace of a space-scoped
// project is reachable by any active member of that workspace, while a runtime
// workspace of an unscoped (legacy) project keeps owner-only access. The admin
// form (administrative-stop) requires tenant administration.
func workspace(t *transaction, tid, uid, wid string, admin bool) Object {
	require(validID(wid), 404, "not_found")
	if admin {
		w := t.one("SELECT * FROM workspaces WHERE id=$1 AND tenant_id=$2 AND deleted_at IS NULL", wid, tid)
		require(w != nil, 404, "not_found")
		return w
	}
	w := t.one("SELECT * FROM workspaces WHERE id=$1 AND tenant_id=$2 AND deleted_at IS NULL", wid, tid)
	require(w != nil, 404, "not_found")
	proj := t.one("SELECT space_id FROM projects WHERE id=$1 AND tenant_id=$2", w.S("projectId"), tid)
	if proj == nil || proj.S("spaceId") == "" {
		require(w.S("ownerUserId") == uid, 404, "not_found")
	} else {
		require(workspaceRole(t, proj.S("spaceId"), uid) != "", 404, "not_found")
	}
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
