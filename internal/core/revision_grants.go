package core

import (
	"time"

	"github.com/wanglongan587/cloud/internal/objectstore"
)

// revisionDownloadGrants signs the read of the prior Revision bundle for a session execution that has
// no result yet (contract decision D2). The key is the one the session input fixed, so a grant can
// never reach another object. Like upload grants, nothing about the URL is written down.
func revisionDownloadGrants(t *transaction, executionID string) Object {
	e := t.one("SELECT * FROM node_executions WHERE execution_id=$1 AND kind='agent_session'", executionID)
	require(e != nil, 404, "not_found")
	require(len(e.O("result")) == 0 && e["terminatedByForceStopId"] == nil, 409, "dispatch_conflict")
	liveDelivery(t, e)
	require(t.objectStore != nil, 409, "object_store_unconfigured")
	key := e.O("input").O("priorRevision").O("bundle").S("key")
	require(key != "", 409, "dispatch_conflict")
	signed, err := objectstore.PresignGET(t.objectStore, key, time.Now())
	require(err == nil, 409, "object_store_unconfigured")
	return Object{"grants": []Object{{"objectKey": key, "url": signed.URL, "method": signed.Method, "headers": signed.Headers, "expiresAt": signed.Expires}}}
}

// revisionGrants signs upload URLs for a delivery execution that has no result yet.
// Nothing about the URL is written down: the signature is returned to the caller and forgotten.
func revisionGrants(t *transaction, executionID string, checksums Object) Object {
	e := t.one("SELECT * FROM node_executions WHERE execution_id=$1 AND kind='deliver_revision'", executionID)
	require(e != nil, 404, "not_found")
	require(len(e.O("result")) == 0 && e["terminatedByForceStopId"] == nil, 409, "dispatch_conflict")
	liveDelivery(t, e)
	require(t.objectStore != nil, 409, "object_store_unconfigured")
	input := e.O("input")
	keys := []string{input.S("bundleKey"), input.S("historyKey")}
	require(len(checksums) <= len(keys), 400, "invalid_upload_checksum")
	for key := range checksums {
		require(key == input.S("bundleKey") || key == input.S("historyKey"), 400, "invalid_upload_checksum")
	}
	grants := make([]Object, 0, len(keys))
	for _, key := range keys {
		require(key != "", 409, "dispatch_conflict")
		if len(checksums) > 0 && checksums[key] == nil {
			continue
		}
		var signed objectstore.Grant
		var err error
		if len(checksums) > 0 {
			signed, err = objectstore.PresignPUTChecksum(t.objectStore, key, checksums.S(key), time.Now())
			require(err == nil, 400, "invalid_upload_checksum")
		} else {
			signed, err = objectstore.PresignPUT(t.objectStore, key, time.Now())
		}
		require(err == nil, 409, "object_store_unconfigured")
		grants = append(grants, Object{"objectKey": key, "url": signed.URL, "method": signed.Method, "headers": signed.Headers, "expiresAt": signed.Expires})
	}
	return Object{"grants": grants}
}
