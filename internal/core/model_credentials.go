package core

import (
	"context"
	"encoding/base64"
)

// ModelCredential checks the verified end user's current account and connection ownership before
// model-gateway encrypts a replacement key. It returns public metadata only.
func (s *Store) ModelCredential(ctx context.Context, identity *Claims, connectionID string) (Object, error) {
	return s.transact(ctx, func(t *transaction) Object {
		u := identityWithAlias(t, identity)
		return modelConnectionView(modelConnection(t, u.S("id"), connectionID))
	})
}

// PutModelCredential stores an immutable authenticated ciphertext produced by model-gateway.
// Core never receives a plaintext key. Existing runs retain their frozen credential reference.
// A repeated idempotency key replays by operation scope/version and the gateway's keyed,
// secret-bound credential ID, without hashing plaintext or randomized ciphertext.
func (s *Store) PutModelCredential(ctx context.Context, identity *Claims, connectionID string, expectedVersion int64, key, credentialID string, ciphertext []byte, keyID string) (Object, error) {
	return s.transact(ctx, func(t *transaction) Object {
		uid := identityWithAlias(t, identity).S("id")
		hash := requestHash("PUT", "model-credential/"+connectionID, Object{"version": expectedVersion, "credentialId": credentialID, "keyId": keyID})
		if old := personalModelReplay(t, uid, key, hash); old != nil {
			return old.O("response")
		}
		c := modelConnection(t, uid, connectionID)
		version(c, expectedVersion)
		require(validID(credentialID) && len(ciphertext) >= 24 && len(ciphertext) <= 16384 && keyID != "" && len(keyID) <= 200, 400, "invalid_model_credential")
		t.exec("INSERT INTO personal_model_credentials(id,user_id,ciphertext,key_id) VALUES($1,$2,$3,$4)", credentialID, uid, base64.StdEncoding.EncodeToString(ciphertext), keyID)
		t.exec("UPDATE personal_model_connections SET credential_id=$2,version=version+1,updated_at=now() WHERE id=$1", connectionID, credentialID)
		out := modelConnectionView(modelConnection(t, uid, connectionID))
		recordPersonalModelWrite(t, uid, key, hash, out, 200)
		return out
	})
}

// ClearModelCredential removes the owner's current credential reference and revokes outstanding
// grants. Immutable encrypted records remain referenced by historical run snapshots.
func (s *Store) ClearModelCredential(ctx context.Context, identity *Claims, connectionID string, expectedVersion int64, key string) (Object, error) {
	return s.transact(ctx, func(t *transaction) Object {
		uid := identityWithAlias(t, identity).S("id")
		require(key != "", 400, "idempotency_key_required")
		hash := requestHash("DELETE", "model-credential/"+connectionID, Object{"version": expectedVersion})
		if old := personalModelReplay(t, uid, key, hash); old != nil {
			return old.O("response")
		}
		c := modelConnection(t, uid, connectionID)
		version(c, expectedVersion)
		t.exec("UPDATE personal_model_connections SET credential_id=NULL,version=version+1,updated_at=now() WHERE id=$1", connectionID)
		revokeConnectionModelGrants(t, connectionID)
		out := modelConnectionView(modelConnection(t, uid, connectionID))
		recordPersonalModelWrite(t, uid, key, hash, out, 200)
		return out
	})
}
