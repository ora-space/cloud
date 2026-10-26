package core

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/wanglongan587/cloud/internal/objectstore"
)

// Reading one registered scheduled artifact (ADR D4).
//
// The read is two steps for the same reason the Revision history read is: authorization and the
// registered row belong to one short transaction, and the object store must never be called inside
// it. The row decides which object is served - a caller names a run and an index, never a key - and
// the persisted key is re-checked against the layout Cloud generates, so a stored value cannot turn
// into an arbitrary object read.

// ScheduledArtifactDownload contains verified bytes and the registered display name for attachment delivery.
// Transport adapters must serve Data as an attachment, never render its declared kind.
type ScheduledArtifactDownload struct {
	Name string
	Data []byte
}

// SessionRunArtifact reads and verifies one artifact through Cloud's private storage endpoint.
//
// Visibility follows the issue: the caller must be an effective tenant member and the run must live
// in the named issue. An artifact that was never registered - because it was refused, never
// uploaded, or pruned by retention - is a 404. Access and registration are checked again after I/O
// so revocation or pruning during the download cannot release stale bytes.
func (s *Store) SessionRunArtifact(ctx context.Context, r *PublicRequest) (*ScheduledArtifactDownload, error) {
	artifact, err := s.transact(ctx, func(t *transaction) Object {
		return scheduledArtifactRegistration(t, r)
	})
	if err != nil {
		return nil, scheduledArtifactReadFault(err)
	}
	if s.ObjectStore == nil {
		return nil, &Fault{Status: 503, Code: "object_store_unavailable", Params: Object{}}
	}
	data, err := objectstore.ReadVerified(ctx, s.ObjectStore, artifact.S("objectKey"), artifact.N("sizeBytes"), artifact.S("sha256"), maxScheduledArtifactBytes)
	if err != nil {
		return nil, &Fault{Status: 404, Code: "artifact_unavailable", Params: Object{}}
	}
	_, err = s.transact(ctx, func(t *transaction) Object {
		current := scheduledArtifactRegistration(t, r)
		require(jsonText(current) == jsonText(artifact), 404, "artifact_unavailable")
		return nil
	})
	if err != nil {
		return nil, scheduledArtifactReadFault(err)
	}
	return &ScheduledArtifactDownload{Name: artifact.S("displayName"), Data: data}, nil
}

// SessionRunArtifacts lists what one run registered, without handing out any URL.
//
// The list is what a reader needs before asking for bytes: name, kind, size and digest per index.
// It is deliberately separate from the download read, so rendering a history view never mints a
// capability, and an empty list is a valid answer - a run may simply have declared nothing.
func (s *Store) SessionRunArtifacts(ctx context.Context, r *PublicRequest) (Object, int, error) {
	artifacts, err := s.transact(ctx, func(t *transaction) Object {
		scheduledArtifactRun(t, r)
		rows := t.list(`SELECT artifact_index, display_name, kind, size_bytes, sha256
			FROM scheduled_run_artifacts WHERE issue_run_id=$1 AND tenant_id=$2 ORDER BY artifact_index`,
			r.RunID, r.TenantID)
		items := make([]Object, 0, len(rows))
		for _, row := range rows {
			items = append(items, Object{
				"index": row.N("artifactIndex"), "name": row.S("displayName"), "kind": row.S("kind"),
				"sizeBytes": row.N("sizeBytes"), "sha256": row.S("sha256"),
			})
		}
		return Object{"artifacts": items}
	})
	if err != nil {
		return nil, 0, scheduledArtifactReadFault(err)
	}
	return artifacts, 200, nil
}

// scheduledArtifactRun authorizes a run-scoped artifact read and fails when the run is not the
// caller's to read.
func scheduledArtifactRun(t *transaction, r *PublicRequest) Object {
	require(r != nil && r.Method == "GET", 405, "method_not_allowed")
	uid := sessionReadIdentity(t, r.Identity)
	membership(t, r.TenantID, uid, false)
	issue(t, r.TenantID, r.IssueID)
	require(validID(r.RunID), 404, "not_found")
	run := t.one(`SELECT id FROM issue_runs
		WHERE id=$1 AND tenant_id=$2 AND issue_id=$3 AND deleted_at IS NULL`, r.RunID, r.TenantID, r.IssueID)
	require(run != nil, 404, "not_found")
	return run
}

// scheduledArtifactRegistration authorizes the read and returns the registered artifact row.
func scheduledArtifactRegistration(t *transaction, r *PublicRequest) Object {
	require(r != nil && r.Method == "GET", 405, "method_not_allowed")
	scheduledArtifactRun(t, r)
	index, err := strconv.Atoi(r.ArtifactIndex)
	require(err == nil && index >= 0 && index < maxScheduledArtifactItems, 404, "artifact_unavailable")
	artifact := t.one(`SELECT object_key, display_name, kind, size_bytes, sha256
		FROM scheduled_run_artifacts WHERE issue_run_id=$1 AND artifact_index=$2 AND tenant_id=$3`,
		r.RunID, index, r.TenantID)
	require(artifact != nil, 404, "artifact_unavailable")
	// The layout is Cloud's own (`scheduledArtifactKey`), so a persisted key still cannot cross scope,
	// become an arbitrary read, or name another run's index.
	parts := strings.Split(artifact.S("objectKey"), "/")
	require(len(parts) == 4 && parts[0] == "scheduled-runs" && parts[1] == r.TenantID &&
		parts[2] == r.RunID && parts[3] == strconv.Itoa(index), 502, "artifact_invalid")
	return artifact
}

// Artifact visibility hides tenant membership failures behind the same 404 as a missing run.
func scheduledArtifactReadFault(err error) error {
	var fault *Fault
	if errors.As(err, &fault) && fault.Code == "membership_required" {
		return &Fault{Status: 404, Code: "not_found", Params: Object{}}
	}
	return err
}
