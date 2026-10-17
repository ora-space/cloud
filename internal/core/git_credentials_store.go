package core

import (
	"context"
	"errors"
)

// The Store-facing half of credential references: confirming a browser submission, settling
// submissions the browser never confirmed, and retrying Store revocations. Each step reads Cloud
// state in one short transaction, asks the Credential Store outside any transaction, and records
// the answer in another, so no lock is ever held across the network.

// errCredentialStoreUnconfigured is the confirm path's answer when no Store is wired.
var errCredentialStoreUnconfigured = &Fault{Code: "credential_store_unconfigured", Status: 503, Params: Object{}}

// ConfirmGitCredential settles the browser's report that it submitted a version. Cloud does not
// trust the report: it asks the Store which version and submission it holds, and only an exact
// match with the pending submission makes the reference stored (identity-access D3 step 3).
func (s *Store) ConfirmGitCredential(ctx context.Context, r *PublicRequest) (Object, int, error) {
	var credentialID string
	var already Object
	_, err := s.transact(ctx, func(t *transaction) Object {
		uid := identityWithAlias(t, r.Identity).S("id")
		membership(t, r.TenantID, uid, false)
		c := loadGitCredential(t, r.TenantID, r.GitCredentialID)
		requireCredentialManager(t, c, uid)
		submission := r.Body.S("submissionId")
		require(validID(submission), 400, "invalid_submission")
		if c.S("pendingSubmissionId") == "" && c.S("secretState") == "stored" {
			// A repeated confirmation after the first one settled is answered, not refused.
			already = gitCredentialView(c)
			return nil
		}
		require(c.S("pendingSubmissionId") == submission, 409, "submission_not_pending")
		credentialID = c.S("id")
		return nil
	})
	if err != nil || already != nil {
		return Object{"resource": already}, 200, err
	}
	if s.GitCredentialStore == nil {
		return nil, 0, errCredentialStoreUnconfigured
	}
	stored, err := s.GitCredentialStore.CredentialVersion(ctx, credentialID)
	if err != nil {
		return nil, 0, &Fault{Code: "credential_store_unavailable", Status: 503, Params: Object{}}
	}
	out, err := s.transact(ctx, func(t *transaction) Object {
		c := loadGitCredential(t, r.TenantID, credentialID)
		require(c.S("pendingSubmissionId") == r.Body.S("submissionId"), 409, "submission_not_pending")
		require(settleSubmission(t, c, stored), 409, "submission_not_stored")
		return Object{"resource": gitCredentialView(loadGitCredential(t, r.TenantID, credentialID))}
	})
	return out, 200, err
}

// settleSubmission adopts the pending version when the Store holds exactly it, written by exactly
// the pending submission. A new version starts with no capability: two tokens may differ in what
// they can do. Publishes approved against the previous version can no longer run, because their
// approver approved writing with the old token.
func settleSubmission(t *transaction, c Object, stored StoredCredential) bool {
	if stored.Revoked || stored.CurrentVersion != c.N("pendingStoreVersion") || stored.SubmissionID != c.S("pendingSubmissionId") {
		return false
	}
	t.exec(`UPDATE credential_refs SET secret_state='stored',store_version=pending_store_version,capabilities='[]'::jsonb,capability_evidence='{}'::jsonb,
	 availability=CASE WHEN availability='invalid' THEN 'available' ELSE availability END,
	 unavailable_reason=CASE WHEN availability='invalid' THEN NULL ELSE unavailable_reason END,
	 pending_submission_id=NULL,pending_store_version=NULL,pending_submission_expires_at=NULL,version=version+1 WHERE id=$1`, c.S("id"))
	for _, op := range t.list("SELECT id FROM repository_operations WHERE type='repository_publish' AND credential_ref_id=$1 AND state IN ('awaiting_approval','pending','leased') ORDER BY id", c.S("id")) {
		finishRepositoryOperation(t, op.S("id"), "cancelled", "credential_changed", Object{})
	}
	updated := t.one("SELECT * FROM credential_refs WHERE id=$1", c.S("id"))
	if requested, _ := updated["requestedCapabilities"].([]any); containsValue(requested, CapabilityPublish) {
		ensurePublishVerification(t, updated, "")
	}
	return true
}

func containsValue(list []any, v any) bool {
	for _, item := range list {
		if item == v {
			return true
		}
	}
	return false
}

// SettleAbandonedGitSubmissionsOnce decides submissions whose grant expired without a browser
// confirmation: Cloud asks the Store once and either adopts the version or clears the pending
// record ("abandoned"). A reference never submitted stays awaiting_secret and can be rotated.
func (s *Store) SettleAbandonedGitSubmissionsOnce(ctx context.Context) error {
	if s.GitCredentialStore == nil {
		return nil
	}
	due, err := s.transact(ctx, func(t *transaction) Object {
		return Object{"items": t.list("SELECT id,pending_submission_id FROM credential_refs WHERE pending_submission_id IS NOT NULL AND pending_submission_expires_at < clock_timestamp()-make_interval(secs=>$1) ORDER BY pending_submission_expires_at LIMIT 20", submissionSettleMargin.Seconds())}
	})
	if err != nil {
		return err
	}
	var errs []error
	for _, item := range due["items"].([]Object) {
		stored, err := s.GitCredentialStore.CredentialVersion(ctx, item.S("id"))
		if err != nil {
			errs = append(errs, err)
			continue
		}
		_, err = s.transact(ctx, func(t *transaction) Object {
			c := t.one("SELECT * FROM credential_refs WHERE id=$1", item.S("id"))
			if c == nil || c.S("pendingSubmissionId") != item.S("pendingSubmissionId") {
				return nil
			}
			if !settleSubmission(t, c, stored) {
				t.exec("UPDATE credential_refs SET pending_submission_id=NULL,pending_store_version=NULL,pending_submission_expires_at=NULL,version=version+1 WHERE id=$1", c.S("id"))
			}
			return nil
		})
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// RevokeGitCredentialsInStoreOnce completes revocations Cloud already recorded. It retries until
// the Store confirms; the Store's revocation is idempotent.
func (s *Store) RevokeGitCredentialsInStoreOnce(ctx context.Context) error {
	if s.GitCredentialStore == nil {
		return nil
	}
	due, err := s.transact(ctx, func(t *transaction) Object {
		return Object{"items": t.list("SELECT id FROM credential_refs WHERE store_revocation_pending ORDER BY revoked_at LIMIT 20")}
	})
	if err != nil {
		return err
	}
	var errs []error
	for _, item := range due["items"].([]Object) {
		if err := s.GitCredentialStore.RevokeCredential(ctx, item.S("id")); err != nil {
			errs = append(errs, err)
			continue
		}
		_, err := s.transact(ctx, func(t *transaction) Object {
			t.exec("UPDATE credential_refs SET store_revocation_pending=false WHERE id=$1", item.S("id"))
			return nil
		})
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}
