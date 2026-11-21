package store

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// DesktopTokenPrefix starts every desktop token, so log redaction and secret scanners can tell
// one apart from other random strings. Only the SHA-256 of the whole token is stored.
const DesktopTokenPrefix = "ora_dt_"

// DesktopTokenIdle is how long a desktop token lives without being used (A6 D6). There is no
// absolute limit and no rotation.
const DesktopTokenIdle = 30 * 24 * time.Hour

// desktopTokenTouch is how stale last_used_at may get before a request refreshes it, so a busy
// desktop writes once an hour rather than on every request. Uses within an hour of a refresh are
// not recorded, so a token expires between DesktopTokenIdle minus this and DesktopTokenIdle after
// its last use.
const desktopTokenTouch = time.Hour

// StartDesktopToken issues a desktop token for an active account. The account is re-checked in the
// same statement: the browser approval that led here may have been minutes ago, and a token for an
// account disabled since then must not be issued. A missing, disabled or deleted account is
// ErrNotFound.
func (s *Store) StartDesktopToken(ctx context.Context, userID string) (string, User, error) {
	raw := make([]byte, 32)
	if _, e := rand.Read(raw); e != nil {
		return "", User{}, fmt.Errorf("desktop token: %w", e)
	}
	token := DesktopTokenPrefix + base64.RawURLEncoding.EncodeToString(raw)
	var u User
	e := s.pool.QueryRow(ctx, `
		WITH issued AS (
			INSERT INTO sessions (token_hash, user_id, kind, expires_at, last_used_at)
			SELECT $1, u.id, 'desktop', now() + $3::interval, now()
			FROM users u WHERE u.id = $2 AND u.status = 'active' AND u.deleted_at IS NULL
			RETURNING user_id
		)
		SELECT u.id, u.display_name, u.status, u.version, u.created_at, u.deleted_at
		FROM issued JOIN users u ON u.id = issued.user_id`,
		hash(token), userID, DesktopTokenIdle.String()).
		Scan(&u.ID, &u.DisplayName, &u.Status, &u.Version, &u.CreatedAt, &u.DeletedAt)
	if errors.Is(e, pgx.ErrNoRows) {
		return "", User{}, ErrNotFound
	}
	if e != nil {
		return "", User{}, fmt.Errorf("create desktop token: %w", e)
	}
	return token, u, nil
}

// DesktopTokenUser returns the active user behind a live desktop token, or ErrNotFound, and
// counts the request as a use: once last_used_at is older than an hour it moves to now and the
// token's expiry to 30 days from now. A web session presented as a bearer token is unknown.
func (s *Store) DesktopTokenUser(ctx context.Context, token string) (User, error) {
	var u User
	var stale bool
	e := s.pool.QueryRow(ctx, `
		SELECT u.id, u.display_name, u.status, u.version, u.created_at, u.deleted_at,
			s.last_used_at < now() - $2::interval
		FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = $1 AND s.kind = 'desktop' AND s.expires_at > now()
			AND u.status = 'active' AND u.deleted_at IS NULL`, hash(token), desktopTokenTouch.String()).
		Scan(&u.ID, &u.DisplayName, &u.Status, &u.Version, &u.CreatedAt, &u.DeletedAt, &stale)
	if errors.Is(e, pgx.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if e != nil {
		return User{}, fmt.Errorf("look up desktop token: %w", e)
	}
	if stale {
		// The guard on last_used_at keeps concurrent requests to one write; losing the race is fine.
		if _, e := s.pool.Exec(ctx, `
			UPDATE sessions SET last_used_at = now(), expires_at = now() + $2::interval
			WHERE token_hash = $1 AND kind = 'desktop' AND last_used_at < now() - $3::interval`,
			hash(token), DesktopTokenIdle.String(), desktopTokenTouch.String()); e != nil {
			return User{}, fmt.Errorf("touch desktop token: %w", e)
		}
	}
	return u, nil
}

// RevokeDesktopToken ends one desktop token, as desktop logout does; web sessions and other
// devices are untouched. Revoking an unknown token is not an error.
func (s *Store) RevokeDesktopToken(ctx context.Context, token string) error {
	if _, e := s.pool.Exec(ctx, `DELETE FROM sessions WHERE token_hash = $1 AND kind = 'desktop'`, hash(token)); e != nil {
		return fmt.Errorf("revoke desktop token: %w", e)
	}
	return nil
}
