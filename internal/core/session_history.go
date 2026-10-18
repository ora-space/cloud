package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/wanglongan587/cloud/internal/objectstore"
)

// SessionHistoryByteLimit bounds one full archive read before JSON decoding.
const SessionHistoryByteLimit int64 = 32 << 20

// SessionHistoryRecordLimit bounds the number of JSONL objects returned by one read.
const SessionHistoryRecordLimit = 100000

// SessionHistory reads an authorized run's registered archive without changing business state.
// Storage I/O happens between two short database transactions: the second checks current access
// and the exact registration again before any record can leave Cloud. Private keys and signed
// storage URLs never become part of the public response or error.
func (s *Store) SessionHistory(ctx context.Context, r *PublicRequest) (Object, int, error) {
	registration, err := s.transact(ctx, func(t *transaction) Object {
		return sessionHistoryRegistration(t, r)
	})
	if err != nil {
		return nil, 0, err
	}
	data, err := objectstore.ReadVerified(ctx, s.ObjectStore, registration.S("historyKey"), registration.N("historySize"), registration.S("historySha256"), SessionHistoryByteLimit)
	if err != nil {
		return nil, 0, sessionHistoryReadFault(err)
	}
	records, err := sessionHistoryRecords(ctx, data)
	if err != nil {
		return nil, 0, err
	}
	_, err = s.transact(ctx, func(t *transaction) Object {
		current := sessionHistoryRegistration(t, r)
		require(jsonText(current) == jsonText(registration), 404, "history_unavailable")
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	return Object{"source": "archive", "records": records, "sizeBytes": int64(len(data)), "sha256": registration.S("historySha256")}, 200, nil
}

func sessionHistoryRegistration(t *transaction, r *PublicRequest) Object {
	require(r != nil && r.Method == "GET", 405, "method_not_allowed")
	uid := sessionReadIdentity(t, r.Identity)
	member := membership(t, r.TenantID, uid, false)
	issue(t, r.TenantID, r.IssueID)
	require(validID(r.RunID), 404, "not_found")
	run := t.one(`SELECT id,executor_type,thread_state FROM issue_runs
		WHERE id=$1 AND tenant_id=$2 AND issue_id=$3 AND deleted_at IS NULL`, r.RunID, r.TenantID, r.IssueID)
	require(run != nil && run.S("executorType") == "agent" && run.S("threadState") != "", 404, "not_found")
	registered := t.one(`SELECT id,execution_id,history_key,history_size,history_sha256,expires_at FROM revisions
		WHERE run_id=$1 AND tenant_id=$2 AND (expires_at IS NULL OR expires_at>clock_timestamp())`, r.RunID, r.TenantID)
	require(registered != nil, 404, "history_unavailable")
	// The exact layout is fixed by deliveryWorkInput. A persisted key still cannot cross scope,
	// become an arbitrary S3 read, or name a bundle rather than this run's session history.
	parts := strings.Split(registered.S("historyKey"), "/")
	require(len(parts) == 5 && parts[0] == "revisions" && parts[1] == member.S("tenantId") && parts[2] == run.S("id") &&
		canonicalHistoryWorkID(parts[3]) && parts[4] == "session.jsonl", 502, "history_invalid")
	require(registered.N("historySize") <= SessionHistoryByteLimit, 413, "history_too_large")
	return registered
}

func canonicalHistoryWorkID(value string) bool {
	id, err := uuid.Parse(value)
	return err == nil && id.String() == value
}

// This read-only counterpart to identityWithAlias deliberately does not provision users or bind
// new aliases. It resolves either already verified Huawei identity and preserves conflict checks.
// Unknown identities cannot have tenant membership, so they receive the same membership denial.
func sessionReadIdentity(t *transaction, c *Claims) string {
	require(c != nil && c.Source != "" && len(c.Source) <= 128 && c.Subject != "" && len(c.Subject) <= 512 && len(c.DisplayName) <= 200, 401, "invalid_identity")
	u := t.one("SELECT u.* FROM users u JOIN user_identities i ON i.user_id=u.id WHERE i.source=$1 AND i.subject=$2", c.Source, c.Subject)
	if c.Source == "huawei-corp" && c.GlobalUserID != "" {
		globalID := canonicalGlobalID(c.GlobalUserID)
		require(globalID != "", 401, "invalid_identity")
		byGlobal := t.one("SELECT u.* FROM users u JOIN user_identities i ON i.user_id=u.id WHERE i.source='huawei-global' AND i.subject=$1", globalID)
		require(u == nil || byGlobal == nil || u.S("id") == byGlobal.S("id"), 409, "identity_conflict")
		if u == nil {
			u = byGlobal
		}
	}
	require(u != nil, 403, "membership_required")
	require(u.S("status") == "active" && u["deletedAt"] == nil, 403, "user_disabled")
	return u.S("id")
}

func sessionHistoryReadFault(err error) *Fault {
	status, code := 502, "history_unavailable"
	if errors.Is(err, objectstore.ErrReadTooLarge) {
		status, code = 413, "history_too_large"
	} else if errors.Is(err, objectstore.ErrReadInvalid) {
		code = "history_invalid"
	}
	return &Fault{Status: status, Code: code, Params: Object{}}
}

// JSONL retains file order, duplicate sequence updates, unknown fields, and exact numeric tokens.
// Replaying last-write-wins history is the consumer's job, not a mutation of archived evidence.
func sessionHistoryRecords(ctx context.Context, data []byte) ([]json.RawMessage, error) {
	records := make([]json.RawMessage, 0)
	for len(data) > 0 {
		if ctx.Err() != nil {
			return nil, sessionHistoryReadFault(ctx.Err())
		}
		line, remaining, found := bytes.Cut(data, []byte{'\n'})
		data = remaining
		if !found {
			data = nil
		}
		// Validate the original line: TrimSpace also accepts Unicode/form-feed whitespace that
		// JSON does not, and must not turn invalid archived bytes into an accepted object.
		rawLine := line
		line = bytes.TrimSpace(line)
		if len(line) == 0 || line[0] != '{' || !utf8.Valid(rawLine) || !json.Valid(rawLine) {
			return nil, &Fault{Status: 502, Code: "history_invalid", Params: Object{}}
		}
		if len(records) == SessionHistoryRecordLimit {
			return nil, &Fault{Status: 413, Code: "history_too_large", Params: Object{}}
		}
		records = append(records, json.RawMessage(line))
	}
	return records, nil
}
