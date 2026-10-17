package core

import (
	"context"
	"errors"
	"time"
)

// The worker side of repository Operations (sync ADR D2, publish ADR D3). A worker leases pending
// rows, moves each to running exactly once while issuing its one authorization, renews the lease
// for as long as the executor works, and settles with fencing on (id, lease generation). Expired
// leases are swept on the database clock: a leased row goes back to pending (it never had an
// authorization), a running one ends — a sync as failed(lease_lost) with a replay, a publish as
// uncertain — because its executor may still be acting under the ID.

// ErrLeaseLost reports that the caller's lease generation no longer owns the Operation.
var ErrLeaseLost = &Fault{Code: "lease_lost", Status: 409, Params: Object{}}

// RepositoryTask is one leased Operation as the worker sees it.
type RepositoryTask struct {
	OperationID, Type, TenantID, ProjectID, BindingID string
	RepositoryURL, Branch                             string
	LeaseGeneration                                   int64
	// Publish only.
	SourceRef, SourceCommit, TargetBranch, ExpectedOldOID string
}

// RepositoryStart is the result of moving a leased Operation to running. Refused is set instead
// when the Operation ended at the start (its credential became unusable, its approval lapsed);
// then no authorization exists and nothing may run.
type RepositoryStart struct {
	Task                RepositoryTask
	AuthorizationID     string
	IssuedAt, ExpiresAt time.Time
	Deadline            time.Time
	CredentialID        string
	CredentialVersion   int64
	ExpectedTrackingOID string
	Refused             string
}

func taskOf(o, b Object) RepositoryTask {
	return RepositoryTask{
		OperationID: o.S("id"), Type: o.S("type"), TenantID: o.S("tenantId"), ProjectID: o.S("projectId"), BindingID: o.S("bindingId"),
		RepositoryURL: b.S("repositoryUrl"), Branch: b.S("branch"), LeaseGeneration: o.N("leaseGeneration"),
		SourceRef: o.S("sourceRef"), SourceCommit: o.S("sourceCommit"), TargetBranch: o.S("targetBranch"), ExpectedOldOID: o.S("expectedOldOid"),
	}
}

// LeaseRepositoryOperations leases up to limit pending Operations for owner. A publish is only
// leased while no other publish of the same Binding branch is leased or running (publish ADR D3).
func (s *Store) LeaseRepositoryOperations(ctx context.Context, owner string, limit int, lease time.Duration) ([]RepositoryTask, error) {
	var out []RepositoryTask
	_, err := s.transact(ctx, func(t *transaction) Object {
		out = nil
		rows := t.list(`SELECT o.id FROM repository_operations o WHERE o.state='pending'
		 AND (o.type='repository_sync' OR NOT EXISTS(SELECT 1 FROM repository_operations x WHERE x.type='repository_publish' AND x.binding_id=o.binding_id AND x.target_branch=o.target_branch AND x.state IN ('leased','running')))
		 ORDER BY o.created_at, o.id LIMIT $1`, limit)
		for _, row := range rows {
			o := t.one("SELECT * FROM repository_operations WHERE id=$1", row.S("id"))
			if o.S("type") == RepositoryPublish && t.one("SELECT 1 AS x FROM repository_operations WHERE type='repository_publish' AND binding_id=$1 AND target_branch=$2 AND state IN ('leased','running')", o.S("bindingId"), o.S("targetBranch")) != nil {
				continue
			}
			t.exec("UPDATE repository_operations SET state='leased',lease_generation=lease_generation+1,lease_owner=$2,lease_expires_at=clock_timestamp()+make_interval(secs=>$3) WHERE id=$1", o.S("id"), owner, seconds(lease))
			o = t.one("SELECT * FROM repository_operations WHERE id=$1", o.S("id"))
			out = append(out, taskOf(o, t.one("SELECT * FROM repository_bindings WHERE id=$1", o.S("bindingId"))))
		}
		return nil
	})
	return out, err
}

// leasedOperation loads o only while gen still owns its unexpired lease in one of states.
func leasedOperation(t *transaction, id string, gen int64, states ...string) Object {
	o := t.one("SELECT * FROM repository_operations WHERE id=$1 AND lease_generation=$2 AND lease_expires_at>clock_timestamp()", id, gen)
	for _, state := range states {
		if o != nil && o.S("state") == state {
			return o
		}
	}
	panic(ErrLeaseLost)
}

// StartRepositoryOperation moves a leased Operation to running and issues its authorization in
// the same transaction (sync ADR invariant 4). expectedTracking is the tracking ref the worker
// read just now, outside the transaction; for a publish it is ignored. Credential usability and,
// for a publish, the approval are checked again here: this is the last moment before the
// executor could redeem anything.
func (s *Store) StartRepositoryOperation(ctx context.Context, id string, gen int64, expectedTracking string, lease, timeout time.Duration) (RepositoryStart, error) {
	var start RepositoryStart
	_, err := s.transact(ctx, func(t *transaction) Object {
		o := leasedOperation(t, id, gen, "leased")
		b := t.one("SELECT * FROM repository_bindings WHERE id=$1", o.S("bindingId"))
		start = RepositoryStart{Task: taskOf(o, b)}
		credential := o.S("credentialRefId")
		capability := ""
		if o.S("type") == RepositorySync {
			require(GitObjectID(expectedTracking), 400, "invalid_expected_tracking")
			if o.S("trigger") != "credential_candidate" {
				// A sync always reads with the Binding's current reference.
				credential = b.S("credentialRefId")
			}
		} else {
			capability = CapabilityPublish
			if refusal := publishStartRefusal(t, o); refusal != "" {
				finishRepositoryOperation(t, id, "cancelled", refusal, Object{})
				start.Refused = refusal
				return nil
			}
		}
		var version any
		if credential != "" {
			if problem := credentialProblem(t, o.S("tenantId"), credential, b.S("repositoryUrl"), capability); problem != "" {
				start.Refused = "credential_unavailable"
				if o.S("type") == RepositorySync {
					settleSync(t, o, "failed", "credential_unavailable", Object{"credentialProblem": problem})
				} else {
					finishRepositoryOperation(t, id, "failed", "credential_unavailable", Object{"credentialProblem": problem})
				}
				return nil
			}
			c := t.one("SELECT store_version FROM credential_refs WHERE id=$1", credential)
			version = c.N("storeVersion")
			start.CredentialID, start.CredentialVersion = credential, c.N("storeVersion")
		}
		var expected any
		if o.S("type") == RepositorySync {
			expected, start.ExpectedTrackingOID = expectedTracking, expectedTracking
		}
		var cred any
		if credential != "" {
			cred = credential
		}
		start.AuthorizationID = newID()
		t.exec(`UPDATE repository_operations SET state='running',authorization_id=$2,started_at=clock_timestamp(),task_deadline=clock_timestamp()+make_interval(secs=>$3),
		 lease_expires_at=clock_timestamp()+make_interval(secs=>$4),expected_tracking_oid=$5,credential_ref_id=$6,credential_version=$7
		 WHERE id=$1`, id, start.AuthorizationID, seconds(timeout), seconds(lease), expected, cred, version)
		row := t.one("SELECT started_at,task_deadline FROM repository_operations WHERE id=$1", id)
		start.IssuedAt = parseTime(row.S("startedAt"))
		start.ExpiresAt = start.IssuedAt.Add(authorizationLifetime)
		start.Deadline = parseTime(row.S("taskDeadline"))
		auditRepositoryOperation(t, id, "running", "", Object{"authorizationId": start.AuthorizationID, "credentialVersion": version, "expectedTrackingOid": expected})
		invalidateRepository(t, id)
		return nil
	})
	return start, err
}

func parseTime(s string) time.Time {
	at, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		panic(databaseFailure{err})
	}
	return at.UTC()
}

// RenewRepositoryLease extends gen's lease while the executor works. It fails once the lease
// lapsed or moved: the sweep may already have ended the Operation.
func (s *Store) RenewRepositoryLease(ctx context.Context, id string, gen int64, lease time.Duration) error {
	_, err := s.transact(ctx, func(t *transaction) Object {
		leasedOperation(t, id, gen, "leased", "running")
		t.exec("UPDATE repository_operations SET lease_expires_at=clock_timestamp()+make_interval(secs=>$2) WHERE id=$1", id, seconds(lease))
		return nil
	})
	return err
}

// ReleaseRepositoryLease returns a leased Operation the worker could not start (for example the
// tracking ref could not be read) to pending. It never touches a running one.
func (s *Store) ReleaseRepositoryLease(ctx context.Context, id string, gen int64) error {
	_, err := s.transact(ctx, func(t *transaction) Object {
		leasedOperation(t, id, gen, "leased")
		t.exec("UPDATE repository_operations SET state='pending',lease_owner=NULL,lease_expires_at=NULL WHERE id=$1", id)
		return nil
	})
	return err
}

// SweepRepositoryLeasesOnce ends what lost its lease. leased → pending is safe because no
// authorization exists yet; running cannot be retried under the same ID (contract D5 item 3).
func (s *Store) SweepRepositoryLeasesOnce(ctx context.Context) error {
	_, err := s.transact(ctx, func(t *transaction) Object {
		t.exec("UPDATE repository_operations SET state='pending',lease_owner=NULL,lease_expires_at=NULL WHERE state='leased' AND lease_expires_at<=clock_timestamp()")
		for _, o := range t.list("SELECT * FROM repository_operations WHERE state='running' AND lease_expires_at<=clock_timestamp() ORDER BY lease_expires_at LIMIT 50") {
			if o.S("type") == RepositorySync {
				settleSync(t, o, "failed", "lease_lost", Object{})
			} else {
				enterUncertain(t, o, "lease_lost")
			}
		}
		t.exec("UPDATE credential_verifications SET state='pending',lease_expires_at=NULL WHERE state='leased' AND lease_expires_at<=clock_timestamp()")
		t.exec("UPDATE credential_verifications SET state='failed',result='verification_failed',finished_at=clock_timestamp() WHERE state='running' AND lease_expires_at<=clock_timestamp()")
		return nil
	})
	return err
}

// IsLeaseLost reports whether err means the caller no longer owns the Operation.
func IsLeaseLost(err error) bool {
	var f *Fault
	return errors.As(err, &f) && f.Code == ErrLeaseLost.Code
}
