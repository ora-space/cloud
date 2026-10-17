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
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/wanglongan587/cloud/internal/objectstore"
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

// rejectWith is reject for a fault whose safe parameters are what makes it actionable: the field, node
// or slot the client has to point the user at. Parameters are part of the stable error contract, so
// they carry identifiers and codes the client localizes — never prose, and never anything the caller
// is not already allowed to read.
func rejectWith(status int, code string, params Object) {
	panic(&Fault{Code: code, Status: status, Params: params})
}

func require(ok bool, status int, code string) {
	if !ok {
		reject(status, code)
	}
}

type databaseFailure struct{ err error }

// Store is injected; there is no global database handle.
type Store struct {
	Pool *sql.DB

	// PluginExecution remains for fixtures that still name the old simulation mode. Plugin installs
	// are Node executions and are no longer gated on it.
	PluginExecution PluginExecutionCapability
	// ObjectStore signs upload grants and verifies stored checksums outside SQL transactions.
	// Nil means delivery is explicitly skipped through the transaction-scoped business hook.
	ObjectStore *objectstore.Config
	// WorkflowAssets downloads the pinned package of a workflow plugin for asset import. Nil
	// keeps the import loop idle, so a control-plane-only deployment never leaves the cluster.
	WorkflowAssets WorkflowAssetSource
	// GitServer is where Workspaces clone from and Revisions are delivered to. Nil means clone
	// steps block as git_server_unconfigured and deliveries are skipped.
	GitServer *GitServer
	// OnThreadEvents receives only first-taken-over events and the caller's SQL transaction.
	// Hooks must use that transaction; starting another Store transaction would deadlock.
	OnThreadEvents func(context.Context, *sql.Tx, string, string, []Object) error
	// OnSessionEnded runs inside the transaction that first stores a session's terminal result.
	OnSessionEnded func(context.Context, *sql.Tx, string, string, Object) error
	// OnRunWorkspaceSettled receives ready or failed in the Workspace operation's transaction.
	OnRunWorkspaceSettled func(context.Context, *sql.Tx, string, string) error
	// OnRunWorkspaceDeleted runs after successful data cleanup in the deletion transaction.
	OnRunWorkspaceDeleted func(context.Context, *sql.Tx, string) error
	// OnDeliverySettled receives saved/unchanged with revisionId and Revision metadata, failed with
	// a safe reason, or skipped when storage is unconfigured. It runs after receipt/Revision writes.
	OnDeliverySettled func(context.Context, *sql.Tx, string, string, Object) error

	// Collaboration ports (consuming-side seams; see collaboration.go). NewStore wires the real
	// workflow-backed Directory and Forms, because Cloud owns the workflow document and needs no
	// external backend to serve those two. The remaining three stay nil ("Unavailable") until a
	// deployment wires them: dev/demo/integration add the in-memory fixtures on top, which is why
	// WireDevelopmentFixtures layers the Agent/Team targets in front of Directory instead of
	// replacing it.
	Directory  CollaborationDirectory
	Context    ContextBuilder
	Dispatcher ExecutionDispatcher
	Forms      FormDescriptorProvider
	Assist     InputAssistProvider

	// Simulator is the workflow run executor under Cloud: a deterministic stand-in that
	// walks a frozen snapshot graph and produces node_states/rounds, instead of an engine
	// Cloud does not have. Wired by WireDevelopmentFixtures only; a production Store keeps
	// it nil, leaving runs `pending` — the honest "no real work ran" convention issue_runs
	// follows. A create that bumps a run straight to `succeeded` must not be possible
	// without this port.
	Simulator WorkflowRunSimulator

	// ThreadIdleTimeout is how long a Thread may sit `idle` before Cloud asks the session to end
	// (Thread D4's idle window). It is a deployment configuration value, not a column, and it is
	// judged entirely on the database clock. Zero (the zero-value Store) means the idle path is not
	// configured: the scan is a no-op rather than a zero-length window that would end every Thread
	// the moment it went idle.
	ThreadIdleTimeout time.Duration

	// AgentStartTimeout bounds waiting for a first authoritative Node record. Expiry of registered
	// execution requests termination; confirmation expiry retains an independent residual ledger.
	// Zero-value stores select the approved defaults when each durable window is first frozen.
	AgentStartTimeout time.Duration
	// AgentRestoreTimeout is the ready-based window for a selected prior Revision.
	AgentRestoreTimeout time.Duration
	// AgentEndConfirmTimeout is frozen at the first unresolved startup ending request.
	AgentEndConfirmTimeout time.Duration

	// DeliveryGiveUpAfter and DeliveryUnreachableAfter are IssueRun D5's two first-version give-up
	// limits for a Revision delivery: how long delivery may fail continuously before Cloud abandons
	// it, and how long the run Workspace's Node may be unreachable before Cloud does the same. They
	// are deployment configuration values, not columns, and both are judged entirely on the database
	// clock. Zero (the zero-value Store) means that condition never gives up, so the scan is a no-op
	// rather than a zero-length window that would abandon every delivery the moment it was declared.
	DeliveryGiveUpAfter      time.Duration
	DeliveryUnreachableAfter time.Duration

	// Events broadcasts committed collaboration-space invalidation notices to live
	// SSE subscribers. Every project belongs to its tenant's sole collaboration
	// space; current tenant membership gates visibility and authorization.
	Events *SpaceHub

	// Signals carries at-most-once work hints to the lease-holding Controller's Watch stream;
	// clone requests stay durable in PostgreSQL whether or not a hint is delivered.
	Signals *ControlHub
}

// WorkflowRunSimulator produces the execution trace for one workflow run. Cloud has no
// engine, so only dev/demo deployments provide one; the result must never claim that real
// work ran (see the mock's output wording).
type WorkflowRunSimulator interface {
	SimulateWorkflowRun(graph, input Object) (nodeStates Object, rounds []Object, status string)
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
	return &Store{
		Pool:      pool,
		Events:    NewSpaceHub(),
		Signals:   NewControlHub(),
		Directory: WorkflowDirectory{Pool: pool},
		Forms:     WorkflowFormDescriptors{Pool: pool},
	}, nil
}

type transaction struct {
	tx  *sql.Tx
	ctx context.Context
	// store is the Store that opened this transaction. The B-side business hooks are invoked with
	// the raw handle and need their way back to the owning Store's policy; carrying it here keeps
	// that lookup from becoming another context value.
	store *Store
	// collaboration ports shadowed from the Store so transaction-scoped helpers can use them.
	directory      CollaborationDirectory
	contextBuilder ContextBuilder
	forms          FormDescriptorProvider
	assist         InputAssistProvider
	simulator      WorkflowRunSimulator
	// queued names operations this transaction made claimable; they are published only after commit.
	pluginExecution       PluginExecutionCapability
	objectStore           *objectstore.Config
	onThreadEvents        func(context.Context, *sql.Tx, string, string, []Object) error
	onSessionEnded        func(context.Context, *sql.Tx, string, string, Object) error
	onRunWorkspaceSettled func(context.Context, *sql.Tx, string, string) error
	onRunWorkspaceDeleted func(context.Context, *sql.Tx, string) error
	onDeliverySettled     func(context.Context, *sql.Tx, string, string, Object) error
	queued                []string
	workSignals           []string
	commandSignals        []string
	events                []SpaceEvent
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
		// A null association is not part of the public Workspace document. Run Workspaces carry the id
		// and are filtered out of public lists, so clients never see this column.
		if o["issueRunId"] == nil {
			delete(o, "issueRunId")
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
	t := &transaction{tx: tx, ctx: ctx, store: s, pluginExecution: s.PluginExecution, directory: s.Directory, contextBuilder: s.Context, forms: s.Forms, assist: s.Assist, simulator: s.Simulator, objectStore: s.ObjectStore, onThreadEvents: s.OnThreadEvents, onSessionEnded: s.OnSessionEnded}
	t.onRunWorkspaceSettled, t.onRunWorkspaceDeleted, t.onDeliverySettled = s.OnRunWorkspaceSettled, s.OnRunWorkspaceDeleted, s.OnDeliverySettled
	// Publish this transaction to the B-side hooks, which the seams below invoke with the *sql.Tx
	// alone. Without it a hook could only wrap the handle in a second transaction view, and every
	// enqueue, Thread entry and post-commit hint it produced would be collected in a set that is
	// discarded when the control plane's own transaction commits. See businessTransaction.
	t.ctx = context.WithValue(t.ctx, businessTransactionKey{}, t)
	t.exec("SELECT pg_advisory_xact_lock(67420911)")
	out = fn(t)
	if err = tx.Commit(); err == nil {
		s.signalOperations(t.queued)
		s.signalRuns(SignalWorkAvailable, t.workSignals)
		s.signalRuns(SignalThreadCommandAvailable, t.commandSignals)
		if s.Events != nil {
			for _, ev := range t.events {
				s.Events.Publish(ev)
			}
		}
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

// identityWithAlias binds the two independently verified Huawei identifiers to
// one user. A conflicting binding is never merged by name or employee number.
func identityWithAlias(t *transaction, c *Claims) Object {
	if c.Source != "huawei-corp" || c.GlobalUserID == "" {
		return identity(t, c.Source, c.Subject, c.DisplayName)
	}
	globalID := canonicalGlobalID(c.GlobalUserID)
	require(globalID != "", 401, "invalid_identity")
	byUUID := t.one("SELECT u.* FROM users u JOIN user_identities i ON i.user_id=u.id WHERE i.source='huawei-corp' AND i.subject=$1", c.Subject)
	byGlobal := t.one("SELECT u.* FROM users u JOIN user_identities i ON i.user_id=u.id WHERE i.source='huawei-global' AND i.subject=$1", globalID)
	require(byUUID == nil || byGlobal == nil || byUUID.S("id") == byGlobal.S("id"), 409, "identity_conflict")
	u := byUUID
	if u == nil {
		u = byGlobal
	}
	if u == nil {
		u = identity(t, c.Source, c.Subject, c.DisplayName)
		byUUID = u
	}
	if byUUID == nil {
		t.exec("INSERT INTO user_identities(user_id,source,subject) VALUES($1,'huawei-corp',$2)", u.S("id"), c.Subject)
	}
	if byGlobal == nil {
		t.exec("INSERT INTO user_identities(user_id,source,subject) VALUES($1,'huawei-global',$2)", u.S("id"), globalID)
	}
	require(u.S("status") == "active" && u["deletedAt"] == nil, 403, "user_disabled")
	return u
}

func canonicalGlobalID(s string) string {
	if s == "" || len(s) > 20 {
		return ""
	}
	for _, ch := range s {
		if ch < '0' || ch > '9' {
			return ""
		}
	}
	s = strings.TrimLeft(s, "0")
	return s
}

// Bootstrap atomically provisions a tenant, its initial administrator, and the
// tenant's sole collaboration space from a deployment command.
func (s *Store) Bootstrap(ctx context.Context, name, source, subject, display string) (Object, error) {
	return s.transact(ctx, func(t *transaction) Object {
		require(name != "" && len(name) <= 200, 400, "invalid_name")
		u := identity(t, source, subject, display)
		tenant, space := provisionTenant(t, u.S("id"), name, "default-"+strings.ReplaceAll(newID(), "-", ""))
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

// project loads a tenant project only when the caller is an active tenant
// member of its sole collaboration space. Unknown projects stay hidden.
func project(t *transaction, tid, uid, pid string) Object {
	p, _ := projectInSpace(t, tid, uid, pid)
	return p
}

// workspace loads authorized runtime content. Safe summaries use runtimeOverview instead.
func workspace(t *transaction, tid, uid, wid string, admin bool) Object {
	require(validID(wid), 404, "not_found")
	w := t.one("SELECT * FROM workspaces WHERE id=$1 AND tenant_id=$2 AND deleted_at IS NULL AND issue_run_id IS NULL", wid, tid)
	require(w != nil, 404, "not_found")
	require(runtimeUsable(t, w, uid), 403, "runtime_use_forbidden")
	if admin {
		membership(t, tid, uid, true)
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
