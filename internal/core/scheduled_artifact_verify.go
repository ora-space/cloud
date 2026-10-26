package core

import (
	"context"

	"github.com/wanglongan587/cloud/internal/objectstore"
)

// artifactVerification is the object-store check of one artifact delivery.
//
// It is computed between two short transactions, exactly like the Revision delivery's verification:
// Cloud asks the object store whether every object the Node reported exists with the declared size
// and digest, and hands the answer to the takeover transaction bound to the frozen execution input,
// so a verdict can never be replayed against another execution. Definitive negative evidence is a
// rejection; an unreachable object store stays retryable with no takeover and no ACK.
type artifactVerification struct {
	input   string
	objects []Object
}

// prepareArtifactVerification authenticates the takeover, captures the frozen input and the declared
// artifacts of the session the delivery names, then checks every reported object outside any
// transaction.
func (s *Store) prepareArtifactVerification(ctx context.Context, r *ControlRequest) (*artifactVerification, error) {
	expected, err := s.transact(ctx, func(t *transaction) Object {
		require(r.Service != nil && r.Service.Role == "controller", 403, "service_forbidden")
		leaseValid(t, r)
		require(len(r.SubmissionID) <= 200, 400, "invalid_submission")
		if old := t.one("SELECT * FROM control_submissions WHERE submission_id=$1", r.SubmissionID); old != nil {
			require(old.S("requestHash") == requestHash(r.Action, r.Service.Subject, r.Body), 409, "submission_conflict")
			return nil
		}
		e := t.one("SELECT * FROM node_executions WHERE execution_id=$1 AND operation_id=$2 AND kind='deliver_scheduled_artifacts'", r.Body.S("executionId"), r.Body.S("operationId"))
		require(e != nil, 404, "not_found")
		require(r.Body.O("result").O("node").S("nodeId") == e.S("nodeId"), 409, "result_conflict")
		// A committed result needs no external check: the replay returns the stored fact.
		if len(e.O("result")) > 0 {
			validateNodeResult(e, r.Body.O("result"))
			return nil
		}
		validateNodeResult(e, r.Body.O("result"))
		require(r.Body.N("sequence") == e.N("lastEventSequence")+1 && r.Body.S("event") != "", 409, "receipt_conflict")
		delivery := t.one("SELECT *, clock_timestamp() < deadline_at AS open FROM scheduled_run_artifact_deliveries WHERE issue_run_id=$1", e.S("operationId"))
		require(delivery != nil, 409, "dispatch_conflict")
		// Closed windows only take over the original event and its receipt. Return the frozen
		// input so settlement can fence it without contacting storage or registering objects.
		if delivery.S("state") != "delivering" || !delivery.B("open") {
			return Object{"input": jsonText(e.O("input"))}
		}
		declaration, declarationErr := scheduledArtifactDeclarationOf(t, delivery.S("tenantId"), e)
		require(declarationErr == nil, 409, "dispatch_conflict")
		declared := Object{}
		for _, artifact := range declaration.Artifacts {
			declared[artifact.Key] = Object{"size": artifact.Size, "sha256": artifact.Sha256}
		}
		return Object{"input": jsonText(e.O("input")), "declared": declared}
	})
	if err != nil || expected == nil {
		return nil, err
	}
	if expected["declared"] == nil {
		return &artifactVerification{input: expected.S("input")}, nil
	}
	if s.ObjectStore == nil {
		return nil, &Fault{Status: 503, Code: "object_store_unavailable", Params: Object{}}
	}
	declared := expected.O("declared")
	verified := &artifactVerification{input: expected.S("input")}
	for _, object := range objectRows(r.Body.O("result")["artifacts"]) {
		key := object.S("key")
		want := declared.O(key)
		if len(want) == 0 {
			// Reporting an object Cloud never declared is not a verification failure to retry: the
			// item simply cannot be registered, and the settlement records the rejection.
			verified.objects = append(verified.objects, Object{"key": key, "verified": false})
			continue
		}
		matched, verifyErr := objectstore.Verify(ctx, s.ObjectStore, key, want.N("size"), want.S("sha256"))
		if verifyErr != nil {
			return nil, &Fault{Status: 503, Code: "object_store_unavailable", Params: Object{}}
		}
		verified.objects = append(verified.objects, Object{
			"key":      key,
			"verified": matched && object.N("size") == want.N("size") && object.S("sha256") == want.S("sha256"),
		})
	}
	return verified, nil
}
