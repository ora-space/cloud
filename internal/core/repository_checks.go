package core

import (
	"context"
	"time"
)

// Read-only checks run through the executor's probe purpose: the reconciliation of uncertain
// publishes (publish ADR D5) and push-capability verifications of credentials (identity-access D4).
// Neither can write anything, which is why each gets a probe authorization of its own and never a
// publish one (executor ADR D2).

// RepositoryCheck is one uncertain publish claimed for a check. Each check has its own one-time
// authorization; Problem is set instead when the credential cannot be used right now, which
// counts as a failed check (the publish stays uncertain).
type RepositoryCheck struct {
	OperationID, TenantID, ProjectID, BindingID string
	RepositoryURL, TargetRef                    string
	SourceCommit, ExpectedOldOID                string
	CredentialID                                string
	CredentialVersion                           int64
	Attempt                                     int64
	AuthorizationID                             string
	IssuedAt, ExpiresAt                         time.Time
	Problem                                     string
}

// checkBackoff spaces checks exponentially from two minutes up to one hour; twenty attempts span
// roughly sixteen hours (publish ADR D5 item 4).
func checkBackoff(attempt int64) time.Duration {
	d := 2 * time.Minute
	for i := int64(1); i < attempt && d < time.Hour; i++ {
		d *= 2
	}
	return min(d, time.Hour)
}

// BeginRepositoryChecks claims due uncertain publishes. Claiming increments the attempt and moves
// the next check forward, so a second worker does not check the same publish concurrently.
func (s *Store) BeginRepositoryChecks(ctx context.Context, limit int) ([]RepositoryCheck, error) {
	var out []RepositoryCheck
	_, err := s.transact(ctx, func(t *transaction) Object {
		out = nil
		for _, o := range t.list("SELECT * FROM repository_operations WHERE state='uncertain' AND next_check_at<=clock_timestamp() ORDER BY next_check_at LIMIT $1", limit) {
			// A worker can disappear after claiming its final attempt. Finish it when
			// its retry time arrives instead of trying to increment past the SQL bound.
			if o.N("checkAttempts") >= maxCheckAttempts {
				finishRepositoryOperation(t, o.S("id"), "failed", "outcome_unknown", Object{})
				continue
			}
			attempt := o.N("checkAttempts") + 1
			t.exec("UPDATE repository_operations SET check_attempts=$2,next_check_at=clock_timestamp()+make_interval(secs=>$3) WHERE id=$1", o.S("id"), attempt, seconds(checkBackoff(attempt)))
			row := t.one("SELECT clock_timestamp() AS now")
			b := t.one("SELECT * FROM repository_bindings WHERE id=$1", o.S("bindingId"))
			c := RepositoryCheck{
				OperationID: o.S("id"), TenantID: o.S("tenantId"), ProjectID: o.S("projectId"), BindingID: o.S("bindingId"),
				RepositoryURL: b.S("repositoryUrl"), TargetRef: "refs/heads/" + o.S("targetBranch"), SourceCommit: o.S("sourceCommit"), ExpectedOldOID: o.S("expectedOldOid"),
				CredentialID: o.S("credentialRefId"), Attempt: attempt,
			}
			// A check only reads, so the credential needs no publish capability now; it must
			// still be usable at all.
			if problem := credentialProblem(t, o.S("tenantId"), c.CredentialID, c.RepositoryURL, ""); problem != "" {
				c.Problem = problem
			} else {
				c.CredentialVersion = t.one("SELECT store_version FROM credential_refs WHERE id=$1", c.CredentialID).N("storeVersion")
				c.AuthorizationID = newID()
				c.IssuedAt = parseTime(row.S("now"))
				c.ExpiresAt = c.IssuedAt.Add(authorizationLifetime)
			}
			out = append(out, c)
		}
		return nil
	})
	return out, err
}

// SettleRepositoryCheck records one check's observation. failure is a safe code when the check
// could not read the target; found reports whether the target exists. Only the attempt that
// claimed the check may settle it.
func (s *Store) SettleRepositoryCheck(ctx context.Context, id string, attempt int64, observed string, found bool, failure string) error {
	_, err := s.transact(ctx, func(t *transaction) Object {
		o := t.one("SELECT * FROM repository_operations WHERE id=$1 AND state='uncertain' AND check_attempts=$2", id, attempt)
		if o == nil {
			return nil
		}
		if failure != "" {
			auditRepositoryOperation(t, id, "check_failed", "", Object{"attempt": attempt, "failure": failure})
			t.exec("UPDATE repository_operations SET result=result||jsonb_build_object('lastCheckFailure',$2::text) WHERE id=$1", id, failure)
			if attempt >= maxCheckAttempts {
				finishRepositoryOperation(t, id, "failed", "outcome_unknown", Object{})
			} else {
				invalidateRepository(t, id)
			}
			return nil
		}
		if !found {
			observed = zeroOIDLike(o.S("expectedOldOid"))
		}
		auditRepositoryOperation(t, id, "checked", "", Object{"attempt": attempt, "observedOid": observed})
		switch observed {
		case o.S("sourceCommit"):
			finishRepositoryOperation(t, id, "succeeded", "", Object{"confirmedByVerification": true, "postObservedOid": observed})
			requestSyncAfterPublish(t, o)
		case o.S("expectedOldOid"):
			// The check runs after the task deadline, so the executor can no longer push: the
			// target still at the old value means the push did not take effect.
			finishRepositoryOperation(t, id, "failed", "not_applied", Object{"postObservedOid": observed})
		default:
			finishRepositoryOperation(t, id, "conflict", "moved_after_uncertain", Object{"postObservedOid": observed})
		}
		return nil
	})
	return err
}

func zeroOIDLike(oid string) string {
	if len(oid) == 64 {
		return "0000000000000000000000000000000000000000000000000000000000000000"
	}
	return "0000000000000000000000000000000000000000"
}

// CredentialCheck is one push-capability verification moved to running with its authorization.
type CredentialCheck struct {
	VerificationID, TenantID, CredentialID, Repository string
	CredentialVersion, LeaseGeneration                 int64
	AuthorizationID                                    string
	IssuedAt, ExpiresAt                                time.Time
}

// StartCredentialVerifications moves pending verifications to running, each with one probe
// authorization. lease covers the whole probe; a verification is never renewed or rerun.
func (s *Store) StartCredentialVerifications(ctx context.Context, limit int, lease time.Duration) ([]CredentialCheck, error) {
	var out []CredentialCheck
	_, err := s.transact(ctx, func(t *transaction) Object {
		out = nil
		for _, v := range t.list("SELECT * FROM credential_verifications WHERE state='pending' ORDER BY created_at LIMIT $1", limit) {
			c := t.one("SELECT * FROM credential_refs WHERE id=$1", v.S("credentialRefId"))
			if c.S("secretState") != "stored" || c.N("storeVersion") != v.N("storeVersion") {
				t.exec("UPDATE credential_verifications SET state='failed',result='verification_failed',finished_at=clock_timestamp() WHERE id=$1", v.S("id"))
				continue
			}
			id := newID()
			t.exec(`UPDATE credential_verifications SET state='running',lease_generation=lease_generation+1,authorization_id=$2,lease_expires_at=clock_timestamp()+make_interval(secs=>$3)
			 WHERE id=$1`, v.S("id"), id, seconds(lease))
			row := t.one("SELECT lease_generation,clock_timestamp() AS now FROM credential_verifications WHERE id=$1", v.S("id"))
			at := parseTime(row.S("now"))
			out = append(out, CredentialCheck{
				VerificationID: v.S("id"), TenantID: v.S("tenantId"), CredentialID: c.S("id"), Repository: v.S("repository"),
				CredentialVersion: v.N("storeVersion"), LeaseGeneration: row.N("leaseGeneration"), AuthorizationID: id, IssuedAt: at, ExpiresAt: at.Add(authorizationLifetime),
			})
		}
		return nil
	})
	return out, err
}

// SettleCredentialVerification records a verification's result: verified adds publish to the
// version's capabilities, push_denied keeps it out, verification_failed may be retried later.
func (s *Store) SettleCredentialVerification(ctx context.Context, id string, gen int64, result string) error {
	_, err := s.transact(ctx, func(t *transaction) Object {
		require(result == "verified" || result == "push_denied" || result == "verification_failed", 400, "invalid_result")
		v := t.one("SELECT * FROM credential_verifications WHERE id=$1 AND state='running' AND lease_generation=$2", id, gen)
		require(v != nil, 409, ErrLeaseLost.Code)
		state := "succeeded"
		if result == "verification_failed" {
			state = "failed"
		}
		t.exec("UPDATE credential_verifications SET state=$2,result=$3,finished_at=clock_timestamp() WHERE id=$1", id, state, result)
		switch result {
		case "verified":
			recordCapability(t, v.S("credentialRefId"), v.N("storeVersion"), CapabilityPublish, id)
		case "push_denied":
			dropCapability(t, v.S("credentialRefId"), v.N("storeVersion"), CapabilityPublish)
		}
		return nil
	})
	return err
}
