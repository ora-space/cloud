package core

import (
	"context"
	"encoding/hex"

	"github.com/wanglongan587/cloud/internal/objectstore"
)

// revisionVerificationFailure is the reason Cloud records on its own verdict when the declared
// objects could not be confirmed. It is Cloud's word, never a Node's: the wire's closed set
// deliberately excludes it so a Node cannot assert Cloud's verification outcome about objects Cloud
// has not looked at. The business settlement recognizes it as the one reason outside that set.
const revisionVerificationFailure = "verification_failed"

type revisionVerification struct {
	input string
	valid bool
}

func revisionSuccess(result Object) bool {
	return result.S("outcome") == "revision_delivered" || result.S("outcome") == "revision_unchanged"
}

// prepareRevision authenticates and validates the frozen declaration before contacting storage.
// Definitive negative evidence is settled, while transport failures remain retryable with no ACK.
func (s *Store) prepareRevision(ctx context.Context, r *ControlRequest) (*revisionVerification, error) {
	e, err := s.transact(ctx, func(t *transaction) Object {
		require(r.Service != nil && r.Service.Role == "controller", 403, "service_forbidden")
		leaseValid(t, r)
		require(len(r.SubmissionID) <= 200, 400, "invalid_submission")
		if old := t.one("SELECT * FROM control_submissions WHERE submission_id=$1", r.SubmissionID); old != nil {
			require(old.S("requestHash") == requestHash(r.Action, r.Service.Subject, r.Body), 409, "submission_conflict")
			return nil
		}
		e := t.one("SELECT * FROM node_executions WHERE execution_id=$1 AND operation_id=$2 AND kind='deliver_revision'", r.Body.S("executionId"), r.Body.S("operationId"))
		require(e != nil, 404, "not_found")
		if len(e.O("result")) > 0 {
			require(jsonText(e.O("result")) == jsonText(r.Body.O("result")), 409, "result_conflict")
			return nil
		}
		validateDeliveryDeclaration(e, r.Body.O("result"))
		liveDelivery(t, e)
		require(r.Body.N("sequence") == e.N("lastEventSequence")+1 && r.Body.S("event") != "", 409, "receipt_conflict")
		return e
	})
	if err != nil || e == nil {
		return nil, err
	}
	result := r.Body.O("result")
	verified := &revisionVerification{input: jsonText(e.O("input")), valid: true}
	objects := []Object{result.O("history")}
	if result.S("outcome") == "revision_delivered" {
		objects = append(objects, result.O("bundle"))
	}
	for _, object := range objects {
		valid, err := objectstore.Verify(ctx, s.ObjectStore, object.S("key"), object.N("size"), object.S("sha256"))
		if err != nil {
			return nil, &Fault{Code: "object_store_unavailable", Status: 503, Params: Object{}}
		}
		verified.valid = verified.valid && valid
	}
	return verified, nil
}

func validateDeliveryDeclaration(e, result Object) {
	input := e.O("input")
	require(result.O("node").S("nodeId") == e.S("nodeId"), 409, "result_conflict")
	require(result.S("baseCommit") == input.S("baseCommit") && commitID(result.S("finalCommit")) && result.S("revisionRef") == input.S("revisionRef"), 409, "result_conflict")
	history := result.O("history")
	require(history.S("key") == input.S("historyKey") && validStoredObject(history), 400, "invalid_result")
	if result.S("outcome") == "revision_unchanged" {
		// A resumed run that added nothing ends on its prior Revision's final commit (contract D4).
		prior := input.O("priorRevision")
		unchanged := result.S("finalCommit") == result.S("baseCommit") ||
			(prior.S("finalCommit") != "" && result.S("finalCommit") == prior.S("finalCommit"))
		require(unchanged && len(result.O("bundle")) == 0, 409, "result_conflict")
	} else {
		bundle := result.O("bundle")
		require(result.S("finalCommit") != result.S("baseCommit") && bundle.S("key") == input.S("bundleKey") && validStoredObject(bundle) && bundle.N("size") > 0, 409, "result_conflict")
	}
}

func validStoredObject(object Object) bool {
	digest, err := hex.DecodeString(object.S("sha256"))
	return err == nil && len(digest) == 32 && hex.EncodeToString(digest) == object.S("sha256") && object.N("size") >= 0
}

// liveDelivery fences cancellation, deletion, replaced Nodes and closed runtime bindings again
// after external verification. A stale result must not release a different Workspace generation.
func liveDelivery(t *transaction, e Object) {
	require(e["terminatedByForceStopId"] == nil, 409, "dispatch_conflict")
	w := t.one("SELECT * FROM workspaces WHERE id=$1 AND deleted_at IS NULL", e.S("workspaceId"))
	require(w != nil && w.S("issueRunId") == e.S("operationId") && w.S("observedState") == "ready" && w.B("admissionOpen"), 409, "dispatch_conflict")
	run := t.one("SELECT * FROM issue_runs WHERE id=$1 AND deleted_at IS NULL", e.S("operationId"))
	require(run != nil && run.S("status") != "cancelled" && run.S("status") != "completed" && run.S("status") != "failed", 409, "dispatch_conflict")
	c := runtimeControl(t, e.S("workspaceId"))
	require(c.S("state") == "maintenance" && c.S("maintenanceRunId") == e.S("operationId"), 409, "dispatch_conflict")
	work := t.one("SELECT * FROM execution_work WHERE id=$1", e.S("workId"))
	node := currentNode(t, e.S("workspaceId"))
	require(node.S("nodeId") == e.S("nodeId") && node.S("sandboxInstanceId") == work.O("target").S("sandboxInstanceId"), 409, "dispatch_conflict")
}

func settleVerifiedRevision(t *transaction, r *ControlRequest, e Object) {
	verification := r.verification
	require(verification != nil && verification.input == jsonText(e.O("input")), 409, "invalid_delivery_verification")
	liveDelivery(t, e)
	result := r.Body.O("result")
	outcome := "delivered"
	if result.S("outcome") == "revision_unchanged" {
		outcome = "unchanged"
	}
	var reason any
	if !verification.valid {
		outcome, reason = "failed", revisionVerificationFailure
	}
	t.exec("INSERT INTO revision_verifications(execution_id,outcome,reason) VALUES($1,$2,$3)", e.S("executionId"), outcome, reason)
	settled := Object{"outcome": outcome}
	if !verification.valid {
		settled["reason"] = reason
	} else {
		w := t.one("SELECT w.*,p.repository_url FROM workspaces w JOIN projects p ON p.id=w.project_id WHERE w.id=$1", e.S("workspaceId"))
		bundle, history := result.O("bundle"), result.O("history")
		var bundleKey, bundleSize, bundleDigest any
		if len(bundle) > 0 {
			bundleKey, bundleSize, bundleDigest = bundle.S("key"), bundle.N("size"), bundle.S("sha256")
		}
		// A run that resumed and moved off its base commit records its prior Revision; one that ended
		// on the prior's final commit reuses the prior's bundle instead of storing one (resume D5).
		var priorID, holderID any
		if prior := e.O("input").O("priorRevision"); prior.S("revisionId") != "" && result.S("finalCommit") != result.S("baseCommit") {
			priorID = prior.S("revisionId")
			if len(bundle) == 0 {
				holderID = resumedBundleHolder(t, prior.S("revisionId"))
			}
		}
		id := newID()
		t.exec(`INSERT INTO revisions(id,execution_id,tenant_id,run_id,workspace_id,project_id,repository_url,base_commit,final_commit,revision_ref,bundle_key,bundle_size,bundle_sha256,history_key,history_size,history_sha256,prior_revision_id,bundle_revision_id)
		 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)`, id, e.S("executionId"), w.S("tenantId"), e.S("operationId"), w.S("id"), w.S("projectId"), w.S("repositoryUrl"), result.S("baseCommit"), result.S("finalCommit"), result.S("revisionRef"), bundleKey, bundleSize, bundleDigest, history.S("key"), history.N("size"), history.S("sha256"), priorID, holderID)
		settled["revision"] = t.one("SELECT * FROM revisions WHERE id=$1", id)
		settled["revisionId"] = id
		if outcome == "delivered" {
			settled["outcome"] = "saved"
		}
	}
	deliverySettled(t, e.S("operationId"), e.S("executionId"), settled)
}
