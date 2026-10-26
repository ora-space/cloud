package core

import (
	"errors"
	"time"

	"github.com/wanglongan587/cloud/internal/objectstore"
)

// errScheduledDeclarationUnavailable reports a delivery execution whose session has no stored
// declaration: an internal contradiction rather than anything a Node could have caused.
var errScheduledDeclarationUnavailable = errors.New("scheduled artifact declaration is unavailable")

// scheduledArtifactGrants signs one upload URL per declared artifact of a scheduled run.
//
// A grant names a key Cloud already chose and carries the declared checksum into the signature, so
// the Node can neither pick a location nor store different bytes than it declared. Nothing about the
// URL is written down: the signature is returned to the caller and forgotten.
//
// Grants expire no later than the run's frozen deadline. An already-started PUT can still finish
// later; registration fences it on the database clock and the recurring GC removes its object.
func scheduledArtifactGrants(t *transaction, executionID string, requested []Object) Object {
	e := t.one("SELECT * FROM node_executions WHERE execution_id=$1 AND kind='deliver_scheduled_artifacts'", executionID)
	require(e != nil, 404, "not_found")
	require(len(e.O("result")) == 0 && e["terminatedByForceStopId"] == nil, 409, "dispatch_conflict")
	require(t.objectStore != nil, 409, "object_store_unconfigured")
	delivery := t.one("SELECT * FROM scheduled_run_artifact_deliveries WHERE issue_run_id=$1", e.S("operationId"))
	require(delivery != nil && delivery.S("state") == "delivering", 409, "dispatch_conflict")
	clock := t.one("SELECT clock_timestamp() AS now")
	at, timeErr := time.Parse(time.RFC3339Nano, clock.S("now"))
	require(timeErr == nil, 409, "dispatch_conflict")
	deadline, timeErr := time.Parse(time.RFC3339Nano, delivery.S("deadlineAt"))
	require(timeErr == nil, 409, "dispatch_conflict")
	config := *t.objectStore
	if config.UploadGrantTTL <= 0 {
		config.UploadGrantTTL = 15 * time.Minute
	}
	config.UploadGrantTTL = min(config.UploadGrantTTL, deadline.Sub(at)).Truncate(time.Second)
	// SigV4 has whole-second precision. Never round a sub-second remainder up past the deadline.
	require(config.UploadGrantTTL >= time.Second, 409, "artifact_delivery_expired")
	declaration, err := scheduledArtifactDeclarationOf(t, delivery.S("tenantId"), e)
	require(err == nil, 400, "invalid_upload_checksum")
	grants := make([]Object, 0, len(requested))
	for _, want := range requested {
		artifact := declaration.artifact(int(want.N("index")))
		require(artifact != nil, 400, "invalid_upload_checksum")
		require(want.N("size") == artifact.Size && want.S("sha256") == artifact.Sha256, 400, "invalid_upload_checksum")
		signed, presignErr := objectstore.PresignPUTChecksum(&config, artifact.Key, artifact.Sha256, at)
		require(presignErr == nil, 400, "invalid_upload_checksum")
		grants = append(grants, Object{
			"objectKey": artifact.Key, "url": signed.URL, "method": signed.Method,
			"headers": signed.Headers, "expiresAt": signed.Expires,
		})
	}
	return Object{"grants": grants}
}

// scheduledArtifactDeclarationOf reads what the run declared, from the immutable session terminal
// result of the delivery execution's own session, and re-derives Cloud's object keys.
func scheduledArtifactDeclarationOf(t *transaction, tenantID string, deliveryExecution Object) (scheduledArtifactDeclaration, error) {
	sessionExecution := deliveryExecution.O("input").S("sessionExecutionId")
	session := t.one("SELECT result FROM node_executions WHERE execution_id=$1 AND kind='agent_session'", sessionExecution)
	if session == nil {
		return scheduledArtifactDeclaration{}, errScheduledDeclarationUnavailable
	}
	return parseScheduledArtifactManifest(tenantID, deliveryExecution.S("operationId"), session.O("result").O("artifactManifest"))
}

// artifact returns the declared entry at one index, or nil when the run did not declare it.
func (d scheduledArtifactDeclaration) artifact(index int) *scheduledArtifact {
	for i := range d.Artifacts {
		if d.Artifacts[i].Index == index {
			return &d.Artifacts[i]
		}
	}
	return nil
}
