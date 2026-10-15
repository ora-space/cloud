package core

import (
	"context"
	"time"
)

// The preparation attempts are bounded three ways, and each bound answers a different failure.
//
// The lease bounds one attempt. An attempt that dies — the process is killed, the executor never
// answers — leaves no transaction to roll back, so without a deadline the row would read `dispatched`
// forever and the Workspace would sit behind the gate with nothing working on it. The attempt count
// bounds the preparation as a whole: a mirror that cannot be prepared has to settle its Operation with
// a reason instead of retrying silently until someone notices. The backoff keeps an upstream that is
// briefly unreachable from spending every attempt in one burst.
//
// The count and the lease are two different questions and are held in two different columns. `attempts`
// counts the attempts that concluded without publishing a mirror; `generation` is the fence a late
// result is refused against. Reading the budget off `generation` conflates them — an attempt that was
// claimed and then lost concludes nothing, so a generation number that has reached the bound describes
// a preparation that is still owed attempts, and refusing it leaves the row claimed with nothing
// watching it (`0040`).
//
// A lost attempt is deliberately not counted, and what bounds it is the lease instead: it lapses, the
// next pass claims the row again and the fetch is dispatched again — work that is happening, which is
// not the silent retry the budget exists to prevent. The two categories do not overlap much either: a
// fetch that fails, or one whose executor never answers within the dispatch timeout, is *answered* by
// the dispatcher and counted, so what is left to the lease is this process dying between the claim and
// the result. Letting that spend the budget would make a Controller restart the thing that ends a
// user's Operation — for a mirror that may well have been published.
//
// The lease is written with the database's clock rather than this process's. A lease timed by a
// worker's own clock would let a worker whose clock runs fast steal an attempt from a slower one that
// is still working, which is the failure the lease exists to prevent.
const (
	// MirrorPreparationLease bounds one attempt, and it is exported because it is also the ceiling on how
	// long an authorization for that attempt may live: an authorization outliving its attempt's lease
	// would permit a fetch whose result the generation fence will refuse, and the deployment has no
	// honest reason to want one. `internal/config` checks the configured mirror lifetime against it, so
	// the two bounds cannot drift apart in a deployment file.
	MirrorPreparationLease    = 5 * time.Minute
	mirrorPreparationAttempts = 3
	mirrorPreparationBackoff  = 30 * time.Second
	// mirrorPreparationBatch bounds one pass. It is a batch rather than a drain so that a backlog is
	// worked through across passes, and so one pass cannot hold a connection for an unbounded time.
	mirrorPreparationBatch = 8
)

// MirrorPreparationFailed is the error code an Operation settles with when its mirror could not be
// prepared within the attempt bound. It is distinct from `mirror_not_ready`, which is what a Workspace
// is told while a preparation is still legitimately in flight: this one says the preparation is over.
const MirrorPreparationFailed = "mirror_preparation_failed"

// MirrorPreparation is one attempt at preparing a mirror, as claimed from the database.
//
// Everything here is Cloud's own state, read in the claim transaction. The values are carried to the
// executor as the binding the authorization is issued against, which is why the claim and the
// authorization must agree: a value read here and signed later is one an executor can compare against
// what it was asked to fetch.
type MirrorPreparation struct {
	// OperationID identifies the preparation. One Operation carries one preparation, which is what makes
	// a late result from an earlier attempt identifiable rather than confusable with the current one.
	OperationID string
	// AuthorizationID is minted at claim time. It is the identity the Store records consumption under,
	// so it is fixed before dispatch rather than derived from the executor's reply.
	AuthorizationID string
	// Generation is the monotonic attempt fence. A result is accepted only if this still equals the
	// row's generation, so an answer to a superseded attempt is refused rather than applied.
	Generation int64
	// ControlEpoch is the Controller epoch the attempt was admitted under, carried to the executor as
	// part of the binding the authorization is issued against. It is not a settlement fence: what ends
	// an Operation because its preparation failed is the attempt budget, which belongs to the
	// preparation and not to any one Controller.
	ControlEpoch int64

	TenantID    string
	OwnerUserID string
	ProjectID   string
	WorkspaceID string
	// Repository is the Project's real upstream as the deployment spelled it — the address the executor
	// dials, never the internal mirror address. The authorization binds its canonical identity, which the
	// executor re-derives from this value; the two differ by the decorations a forge adds to one
	// repository, and keeping both is what lets an upstream that serves only `/repo.git` be fetched.
	Repository string
	// Ref is the fully qualified ref this preparation pins.
	Ref string
	// CredentialID and CredentialVersion name the exact material the fetch is authorized with, at the
	// version validated when this attempt was claimed.
	CredentialID      string
	CredentialVersion int64
	// Capabilities is the capability set admission required of the reference, in canonical order. It is
	// carried for the same reason a credentialed clone carries it: the continuity decision's D4 fixes the
	// capability in the execution plan, so the authorization must bind the set the guard approved rather
	// than a set the dispatcher decides at signing time. It is the requirement — reading one repository —
	// and not the reference's whole set, which may permit work this preparation does not do.
	Capabilities []string
	// RuntimeGeneration is the Workspace runtime generation this preparation serves.
	RuntimeGeneration int64
}

// MirrorPreparationOutcome is what a successful preparation publishes.
//
// Both fields are required evidence rather than description: the commit is the immutable content a
// later clone is pinned to, and the mirror generation is what a reader compares against Cloud's rows.
type MirrorPreparationOutcome struct {
	Commit           string
	MirrorGeneration int64
}

// MirrorPreparer performs one claimed preparation.
//
// It is defined here because this is where it is consumed, and it is separate from CredentialedCloner
// because the two purposes must not be interchangeable: a mirror preparation reads one ref and
// publishes an artifact, a clone produces a checkout. An implementation must not be able to serve one
// with the other's authorization, and keeping the seams apart is what makes that structural instead of
// a check someone has to remember.
type MirrorPreparer interface {
	Prepare(ctx context.Context, p MirrorPreparation) (MirrorPreparationOutcome, error)
}

// SetMirrorPreparer installs the mirror executor, once, at startup. A deployment that installs none
// prepares no mirrors, and the gate on the `node` -> `clone` transition keeps refusing with
// `mirror_not_ready` — the preparation is never claimed, so no Operation is left half-prepared.
func (s *Store) SetMirrorPreparer(p MirrorPreparer) { s.mirrorPreparer = p }

// mirrorPreparationCandidates lists the Operations that need an attempt now.
//
// The filter is the whole policy of "who needs preparing", and it is deliberately narrow: an Operation
// that has not reached `node` is not cloning yet, an anonymous Project has no mirror at all, and a
// preparation that is already working, already settled, or waiting out its backoff is not due.
//
// Three branches pick an Operation out. The first is an Operation with no preparation at all. The
// second is an attempt that is due again: a row whose lease has lapsed, which is what a lost attempt
// leaves behind, or one being held back by the backoff after a retryable failure. That branch is
// bounded by the counted attempts and not by the generation number, for the reason the bounds above
// give. The third is a preparation that is `ready` and no longer describes what the Operation needs —
// the credential version, or the upstream, ref, runtime generation or credential identity the gate
// re-checks. A ready mirror is not evidence that the current requirement is prepared, so a drifted row
// would otherwise be refused by the gate forever with nothing working on it.
//
// The drift branch compares the whole list the gate compares rather than the one entry a user can move
// today (rotating a token advances the reference's version; the rest are written by operations the
// Project's single active operation keeps from overlapping). Two lists that must agree are one list:
// they came apart once already, which is why a rotated mirror had no path back to being prepared.
//
// Only the Operations that reach `clone` are candidates. A `start` or `restart` Operation stands at
// `node` too, but its Workspace already holds its content, so nothing on that path reads the mirror:
// preparing one for it would spend an upstream fetch on content nobody clones, and would let a
// credential that has since been revoked end a restart that does not need it.
//
// The drift branch requires the reference to be available, because a frozen or invalid reference is a
// refusal for the gate to report in its own words — while comparing versions against it would make the
// row look claimable, and a handful of such rows would fill every batch in `ORDER BY updated_at` order.
func (s *Store) mirrorPreparationCandidates(ctx context.Context, limit int) ([]string, error) {
	var ids []string
	_, err := s.transact(ctx, func(t *transaction) Object {
		rows := t.list(`SELECT o.id FROM operations o
			JOIN projects p ON p.id=o.project_id
			JOIN workspaces w ON w.id=o.workspace_id
			LEFT JOIN mirror_preparations m ON m.operation_id=o.id
			LEFT JOIN credential_refs c ON c.id=p.credential_ref_id
			WHERE o.step='node' AND o.state='running' AND o.workspace_id IS NOT NULL
			  AND o.kind IN ('create_project','create_workspace')
			  AND p.credential_ref_id IS NOT NULL
			  AND w.requested_ref IS NOT NULL AND w.requested_ref <> ''
			  AND (m.operation_id IS NULL
			       OR (m.state IN ('admitting','dispatched')
			           AND (m.retry_at IS NULL OR m.retry_at <= now())
			           AND (m.lease_expires_at IS NULL OR m.lease_expires_at <= now())
			           AND m.attempts < $1)
			       OR (m.state='ready' AND c.availability='available'
			           AND (m.repository_url IS DISTINCT FROM p.repository_url
			                OR m.pinned_ref IS DISTINCT FROM 'refs/heads/'||w.requested_ref
			                OR m.runtime_generation IS DISTINCT FROM w.runtime_generation
			                OR m.credential_ref_id IS DISTINCT FROM p.credential_ref_id
			                OR m.credential_ref_version IS DISTINCT FROM c.version)))
			ORDER BY o.updated_at LIMIT $2`, mirrorPreparationAttempts, limit)
		for _, row := range rows {
			ids = append(ids, row.S("id"))
		}
		return Object{}
	})
	return ids, err
}

// claimMirrorPreparation admits one attempt for an Operation and returns what to dispatch.
//
// A nil preparation with a nil error means there is nothing to do: the Operation moved on. A non-nil
// error means the admission refused, and the caller classifies it.
//
// The claim is its own short transaction, and dispatching happens outside it. Admitting and
// dispatching a preparation take minutes; the execution contract forbids holding a transaction across
// that work, so the claim commits the attempt and the row's lease is what keeps a second worker off it.
func (s *Store) claimMirrorPreparation(ctx context.Context, operation string) (*MirrorPreparation, error) {
	var claimed *MirrorPreparation
	_, err := s.transact(ctx, func(t *transaction) Object {
		// FOR UPDATE so two passes cannot claim the same Operation concurrently; the row is held only for
		// the database work below, never across a dispatch.
		o := t.one("SELECT * FROM operations WHERE id=$1 FOR UPDATE", operation)
		if o == nil || o.S("state") != "running" || o.S("step") != "node" {
			return Object{}
		}
		w := t.one("SELECT * FROM workspaces WHERE id=$1", o.S("workspaceId"))
		require(w != nil, 404, "not_found")
		p := t.one("SELECT * FROM projects WHERE id=$1", o.S("projectId"))
		require(p != nil, 404, "not_found")

		// The same admission a credentialed clone goes through, reused rather than reimplemented. It is
		// what establishes that the reference currently permits the work and records the credential
		// version this attempt is admitted under, so the preparation cannot be built from a fetch the
		// Project is no longer entitled to. A refusal here is the caller's to classify.
		requireRepositoryAccess(t, o.S("projectId"))
		c := t.admittedCredential
		require(c != nil, 409, repositoryCredentialExecutorUnavailable)

		// The attempt's identity is minted here, before dispatch, because the Store records consumption
		// under it and a value derived from the executor's reply would not be fixed before the call.
		authorization := newID()
		generation := int64(1)
		if m := t.one("SELECT * FROM mirror_preparations WHERE operation_id=$1", o.S("id")); m != nil {
			generation = m.N("generation") + 1
		}
		// A reclaim moves the same row forward rather than inserting a second one: the row is keyed by the
		// Operation, and a retry that left the previous attempt's evidence behind would let a reader see a
		// ready mirror next to the attempt that superseded it. `attempts` is deliberately not in the update
		// list: it is the budget the preparation as a whole is spending, and a claim that cleared it would
		// let an upstream that fails on every attempt retry forever.
		t.exec(`INSERT INTO mirror_preparations(operation_id,tenant_id,project_id,workspace_id,repository_url,
				pinned_ref,runtime_generation,control_epoch,state,credential_ref_id,credential_ref_version,
				authorization_id,generation,lease_expires_at,retry_at)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,'dispatched',$9,$10,$11,$12,now()+make_interval(secs => $13),NULL)
			ON CONFLICT (operation_id) DO UPDATE SET
				repository_url=EXCLUDED.repository_url, pinned_ref=EXCLUDED.pinned_ref,
				runtime_generation=EXCLUDED.runtime_generation, control_epoch=EXCLUDED.control_epoch,
				state='dispatched', credential_ref_id=EXCLUDED.credential_ref_id,
				credential_ref_version=EXCLUDED.credential_ref_version,
				authorization_id=EXCLUDED.authorization_id, generation=EXCLUDED.generation,
				commit_id=NULL, mirror_generation=NULL, error_code=NULL,
				lease_expires_at=EXCLUDED.lease_expires_at, retry_at=NULL, updated_at=now()`,
			o.S("id"), p.S("tenantId"), o.S("projectId"), w.S("id"), p.S("repositoryUrl"),
			"refs/heads/"+w.S("requestedRef"), w.N("runtimeGeneration"), o.N("controllerEpoch"),
			p.S("credentialRefId"), c.CredentialVersion, authorization, generation,
			MirrorPreparationLease.Seconds())
		claimed = &MirrorPreparation{
			OperationID:       o.S("id"),
			AuthorizationID:   authorization,
			Generation:        generation,
			ControlEpoch:      o.N("controllerEpoch"),
			TenantID:          p.S("tenantId"),
			OwnerUserID:       c.OwnerUserID,
			ProjectID:         o.S("projectId"),
			WorkspaceID:       w.S("id"),
			Repository:        p.S("repositoryUrl"),
			Ref:               "refs/heads/" + w.S("requestedRef"),
			CredentialID:      c.CredentialID,
			CredentialVersion: c.CredentialVersion,
			Capabilities:      c.Capabilities,
			RuntimeGeneration: w.N("runtimeGeneration"),
		}
		return Object{}
	})
	return claimed, err
}

// settleMirrorPreparation records the answer to one attempt, accepting it only if that attempt is
// still current.
//
// The fence is `generation`. An answer that arrives after the lease lapsed and another attempt was
// claimed belongs to an attempt that is over, and applying it would let a slow or replayed reply
// overwrite the result of the attempt that replaced it — so it is refused and reported, not written.
// The caller learns which of the two happened from the returned bool.
//
// It is also where the attempt budget is spent: every attempt that concludes without publishing a
// mirror is counted, and that count — not the generation fence — is what says whether another attempt
// follows. A published mirror clears the count, because the budget bounds one requirement and a
// rotation is a new one: a mirror that was prepared is not a mirror that has been failing.
//
//nolint:gocritic // The claim is passed by value on purpose: the generation this settles is the one the attempt was claimed under, so it must not be reachable for writing from here.
func (s *Store) settleMirrorPreparation(ctx context.Context, p MirrorPreparation, outcome MirrorPreparationOutcome, cause error) (bool, error) {
	applied := false
	_, err := s.transact(ctx, func(t *transaction) Object {
		if cause == nil {
			applied = t.execRows(`UPDATE mirror_preparations SET state='ready',commit_id=$3,mirror_generation=$4,
					error_code=NULL,attempts=0,lease_expires_at=NULL,retry_at=NULL,updated_at=now()
				WHERE operation_id=$1 AND generation=$2 AND state='dispatched'`,
				p.OperationID, p.Generation, outcome.Commit, outcome.MirrorGeneration) > 0
			return Object{}
		}

		// A failure either ends the preparation or releases the attempt for another one. Which of the two
		// is decided here rather than by the caller, because it depends on the counted attempts this
		// transaction is holding, and reading that outside would race the claim that spends them.
		//
		// The row is locked because the count is read and then written: two settlements of one attempt
		// would otherwise each spend the budget once, and the lock also makes a settlement that lost a race
		// with a success see the `ready` state rather than counting an attempt the mirror answered.
		code := ErrorCode(cause).Code
		terminal := terminalCredentialRefusals[code]
		m := t.one("SELECT * FROM mirror_preparations WHERE operation_id=$1 FOR UPDATE", p.OperationID)
		if m == nil || m.N("generation") != p.Generation || m.S("state") != "dispatched" {
			return Object{} // Already superseded: the failure belongs to an attempt that is over.
		}
		applied = true
		attempts := m.N("attempts") + 1
		if !terminal && attempts < mirrorPreparationAttempts {
			// Release the lease and hold the attempt back. The row stays `dispatched` with a lapsed lease,
			// which is what makes the next pass see it as due again.
			t.exec(`UPDATE mirror_preparations SET attempts=$2,error_code=$3,lease_expires_at=now(),
					retry_at=now()+make_interval(secs => $4),updated_at=now()
				WHERE operation_id=$1 AND generation=$5`,
				p.OperationID, attempts, code, mirrorPreparationBackoff.Seconds(), p.Generation)
			return Object{}
		}
		t.exec(`UPDATE mirror_preparations SET state='failed',attempts=$2,error_code=$3,lease_expires_at=NULL,retry_at=NULL,
				updated_at=now() WHERE operation_id=$1 AND generation=$4`,
			p.OperationID, attempts, code, p.Generation)
		// A preparation that has ended must settle its Operation. Leaving it `running` at `node` would
		// make the Controller retry the gate forever with nothing left working on the mirror — the
		// silent retry this worker exists to prevent.
		//
		// The settlement is fenced on the Operation still being the one this pass worked on
		// (`state='running'`, still at `node`) and on nothing else. It is deliberately *not* fenced on
		// the epoch the attempt was claimed under: what ends here is this preparation, and the fact that
		// its attempts are spent does not belong to any one Controller. A Controller that restarted and
		// re-claimed the Operation at a new epoch is refused by this same gate for the same reason, and
		// fencing on the old epoch would refuse the write for every Controller alike — leaving the
		// Operation `running` at `node` for good, which is the wedge this settlement exists to remove.
		if t.execRows(`UPDATE operations SET state='failed',error_code=$2,retry_at=NULL,version=version+1,updated_at=now()
			WHERE id=$1 AND state='running' AND step='node' AND workspace_id IS NOT NULL`,
			p.OperationID, mirrorPreparationFailureCode(code)) == 1 {
			// The same obligation every other end of a Workspace-driving Operation carries: the
			// Workspace, the runtime reservation and the IssueRun are settled with it.
			failWorkspaceOperation(t, p.OperationID)
		}
		return Object{}
	})
	return applied, err
}

// mirrorPreparationFailureCode names the Operation's failure. A durable credential refusal keeps its
// own code, because it is the reason the Operation ended and an operator has to see the cause rather
// than the fact that a mirror was involved. Everything else — an upstream that never answered,
// attempts that ran out — is the preparation's own failure.
func mirrorPreparationFailureCode(code string) string {
	if terminalCredentialRefusals[code] {
		return code
	}
	return MirrorPreparationFailed
}

// PrepareMirrorsOnce runs one pass: claim a batch, dispatch each attempt, settle what came back.
//
// Returning the error rather than logging it is what lets the caller's loop decide — the same shape
// the other periodic passes in this package use. Nothing is held across a dispatch: the claim has
// committed before the executor is called and the settlement is a separate short transaction, so a
// pass that dies mid-dispatch loses nothing but its lease, which the next pass finds lapsed.
func (s *Store) PrepareMirrorsOnce(ctx context.Context) error {
	if s.mirrorPreparer == nil {
		return nil // No executor installed: no mirror is claimed, and the gate keeps refusing honestly.
	}
	ids, err := s.mirrorPreparationCandidates(ctx, mirrorPreparationBatch)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return err
		}
		claimed, err := s.claimMirrorPreparation(ctx, id)
		if err != nil {
			// A durable refusal settles the Operation here rather than through an attempt: there is nothing
			// to prepare, and leaving it running would retry the gate forever.
			if err := s.settleRefusedMirrorPreparation(ctx, id, err); err != nil {
				return err
			}
			continue
		}
		if claimed == nil {
			continue
		}
		outcome, cause := s.mirrorPreparer.Prepare(ctx, *claimed)
		if _, err := s.settleMirrorPreparation(ctx, *claimed, outcome, cause); err != nil {
			return err
		}
	}
	return nil
}

// settleRefusedMirrorPreparation ends an Operation whose preparation was refused before any attempt
// was made, and does nothing for a refusal that is merely retryable — the executor is not installed
// yet, a control row is being reconciled — because those must leave the Operation running so a later
// pass can still prepare it.
func (s *Store) settleRefusedMirrorPreparation(ctx context.Context, operation string, cause error) error {
	code := ErrorCode(cause).Code
	if !terminalCredentialRefusals[code] {
		return nil
	}
	// No attempt was claimed, so there is no generation to carry. The write stays fenced on the
	// Operation being the one that was refused — still `running`, still at the step the gate is reached
	// from, still a Workspace Operation — and, like the other settlements, settles what that Operation
	// was holding once it has recorded the failure.
	_, err := s.transact(ctx, func(t *transaction) Object {
		if t.execRows(`UPDATE operations SET state='failed',error_code=$2,retry_at=NULL,version=version+1,updated_at=now()
			WHERE id=$1 AND state='running' AND step='node' AND workspace_id IS NOT NULL`, operation, code) == 1 {
			failWorkspaceOperation(t, operation)
		}
		return Object{}
	})
	return err
}
