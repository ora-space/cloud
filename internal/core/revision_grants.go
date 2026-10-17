package core

import (
	"time"

	"github.com/wanglongan587/cloud/internal/objectstore"
)

// revisionGrants signs the upload URL of the session history for a delivery execution that has no
// result yet. The code itself is pushed to the Git Server, which authorizes every push from current
// state, so the history key is the only object a grant can ever reach; a checksum for any other key
// is refused. Nothing about the URL is written down: the signature is returned to the caller and
// forgotten.
func revisionGrants(t *transaction, executionID string, checksums Object) Object {
	e := t.one("SELECT * FROM node_executions WHERE execution_id=$1 AND kind='deliver_revision'", executionID)
	require(e != nil, 404, "not_found")
	require(len(e.O("result")) == 0 && e["terminatedByForceStopId"] == nil, 409, "dispatch_conflict")
	liveDelivery(t, e)
	require(t.objectStore != nil, 409, "object_store_unconfigured")
	key := e.O("input").S("historyKey")
	require(key != "", 409, "dispatch_conflict")
	for name := range checksums {
		require(name == key, 400, "invalid_upload_checksum")
	}
	var signed objectstore.Grant
	var err error
	if len(checksums) > 0 {
		signed, err = objectstore.PresignPUTChecksum(t.objectStore, key, checksums.S(key), time.Now())
		require(err == nil, 400, "invalid_upload_checksum")
	} else {
		signed, err = objectstore.PresignPUT(t.objectStore, key, time.Now())
		require(err == nil, 409, "object_store_unconfigured")
	}
	return Object{"grants": []Object{{"objectKey": key, "url": signed.URL, "method": signed.Method, "headers": signed.Headers, "expiresAt": signed.Expires}}}
}
