package modelgateway

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
)

// VerifyExistingKey fails startup if a replaced/lost master key cannot open the database's
// previous encrypted records. It reads one record per key identity, clears the plaintext in
// memory, and never logs or exposes it. Missing encryption volumes cannot silently orphan keys.
func VerifyExistingKey(ctx context.Context, pool *sql.DB, cipher *Cipher) error {
	rows, err := pool.QueryContext(ctx, "SELECT DISTINCT ON (key_id) id::text,key_id,ciphertext FROM personal_model_credentials ORDER BY key_id,id")
	if err != nil {
		return errors.New("model credential key verification unavailable")
	}
	defer rows.Close()
	for rows.Next() {
		var id, keyID, value string
		if err = rows.Scan(&id, &keyID, &value); err != nil {
			return errors.New("model credential key verification unavailable")
		}
		sealed, decodeErr := base64.StdEncoding.DecodeString(value)
		if decodeErr != nil {
			return errors.New("model credential encryption records require repair")
		}
		plaintext, openErr := cipher.Open(id, keyID, sealed)
		if openErr != nil {
			return errors.New("model credential encryption key does not match existing records; restore its protected volume")
		}
		clear(plaintext)
	}
	if rows.Err() != nil {
		return errors.New("model credential key verification unavailable")
	}
	return nil
}
