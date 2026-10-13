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
// The lease is written with the database's clock rather than this process's. A lease timed by a
// worker's own clock would let a worker whose clock runs fast steal an attempt from a slower one that
// is still working, which is the failure the lease exists to prevent.
const (
	mirrorPreparationLease    = 5 * time.Minute
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
	// ControlEpoch fences the Operation itself. A settlement that names an epoch the Operation has
	// moved past must not land.
	ControlEpoch int64

	TenantID    string
	OwnerUserID string
	ProjectID   string
	WorkspaceID string
	// Repository is the Project's real upstream identity, never the internal mirror address.
	Repository string
	// Ref is the fully qualified ref this preparation pins.
	Ref string
	// CredentialID and CredentialVersion name the exact material the fetch is authorized with, at the
	// version validated when this attempt was claimed.
	CredentialID      string
	CredentialVersion int64
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
// preparation that is already claimed, already settled, or waiting out its backoff is not due.
func (s *Store) mirrorPreparationCandidates(ctx context.Context, limit int) ([]string, error) {
	var ids []string
	_, err := s.transact(ctx, func(t *transaction) Object {
		rows := t.list(`SELECT o.id FROM operations o
			JOIN projects p ON p.id=o.project_id
			JOIN workspaces w ON w.id=o.workspace_id
			LEFT JOIN mirror_preparations m ON m.operation_id=o.id
			WHERE o.step='node' AND o.state='running' AND o.workspace_id IS NOT NULL
			  AND p.credential_ref_id IS NOT NULL
			  AND w.requested_ref IS NOT NULL AND w.requested_ref <> ''
			  AND (m.operation_id IS NULL
			       OR (m.state IN ('admitting','dispatched')
			           AND (m.retry_at IS NULL OR m.retry_at <= now())
			           AND (m.lease_expires_at IS NULL OR m.lease_expires_at <= now())
			           AND m.generation < $1))
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
		// ready mirror next to the attempt that superseded it.
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
			mirrorPreparationLease.Seconds())
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
//nolint:gocritic // The claim is passed by value on purpose: the generation this settles is the one the attempt was claimed under, so it must not be reachable for writing from here.
func (s *Store) settleMirrorPreparation(ctx context.Context, p MirrorPreparation, outcome MirrorPreparationOutcome, cause error) (bool, error) {
	applied := false
	_, err := s.transact(ctx, func(t *transaction) Object {
		if cause == nil {
			applied = t.execRows(`UPDATE mirror_preparations SET state='ready',commit_id=$3,mirror_generation=$4,
					error_code=NULL,lease_expires_at=NULL,retry_at=NULL,updated_at=now()
				WHERE operation_id=$1 AND generation=$2 AND state='dispatched'`,
				p.OperationID, p.Generation, outcome.Commit, outcome.MirrorGeneration) > 0
			return Object{}
		}

		// A failure either ends the preparation or releases the attempt for another one. Which of the two
		// is decided here rather than by the caller, because it depends on the attempt count this
		// transaction is holding, and reading that outside would race the claim that increments it.
		code := ErrorCode(cause).Code
		terminal := terminalCredentialRefusals[code]
		m := t.one("SELECT * FROM mirror_preparations WHERE operation_id=$1", p.OperationID)
		if m == nil || m.N("generation") != p.Generation || m.S("state") != "dispatched" {
			return Object{} // Already superseded: the failure belongs to an attempt that is over.
		}
		applied = true
		if !terminal && m.N("generation") < mirrorPreparationAttempts {
			// Release the lease and hold the attempt back. The row stays `dispatched` with a lapsed lease,
			// which is what makes the next pass see it as due again.
			t.exec(`UPDATE mirror_preparations SET error_code=$2,lease_expires_at=now(),
					retry_at=now()+make_interval(secs => $3),updated_at=now()
				WHERE operation_id=$1 AND generation=$4`,
				p.OperationID, code, mirrorPreparationBackoff.Seconds(), p.Generation)
			return Object{}
		}
		t.exec(`UPDATE mirror_preparations SET state='failed',error_code=$2,lease_expires_at=NULL,retry_at=NULL,
				updated_at=now() WHERE operation_id=$1 AND generation=$3`,
			p.OperationID, code, p.Generation)
		// A preparation that has ended must settle its Operation. Leaving it `running` at `node` would
		// make the Controller retry the gate forever with nothing left working on the mirror — the
		// silent retry this worker exists to prevent. The settlement is epoch-fenced like every other
		// control-plane write, so it cannot land on an Operation that has moved on.
		t.exec(`UPDATE operations SET state='failed',error_code=$2,retry_at=NULL,version=version+1,updated_at=now()
			WHERE id=$1 AND state='running' AND step='node' AND controller_epoch=$3`,
			p.OperationID, mirrorPreparationFailureCode(code), p.ControlEpoch)
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
	// No attempt was claimed, so there is no epoch to carry; the write re-reads the Operation's own and
	// stays fenced on the step it was admitted at.
	_, err := s.transact(ctx, func(t *transaction) Object {
		t.exec(`UPDATE operations SET state='failed',error_code=$2,retry_at=NULL,version=version+1,updated_at=now()
			WHERE id=$1 AND state='running' AND step='node'`, operation, code)
		return Object{}
	})
	return err
}
