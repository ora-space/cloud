package gitserver

import (
	"context"
	"fmt"

	"github.com/wanglongan587/cloud/internal/gitread"
	"github.com/wanglongan587/cloud/internal/mirror"
)

// CloudReader is what the Git Server needs from Cloud's control plane: the authoritative binding for
// one read, read at request time. It is declared here, at the consuming boundary, so that the Git
// Server depends on the one question it asks rather than on the control plane's whole surface.
type CloudReader interface {
	ReadGitMirror(ctx context.Context, origin gitread.Origin, digest string) (gitread.Snapshot, error)
}

// MirrorStateReader composes the two halves of a read authorization into one answer.
//
// Cloud's authoritative reader answers the binding — who is asking, which workspace and project
// that is, and which mirror key this workspace is addressed by. It does not answer the mirror's own
// state, and it must not guess it: readiness and generation belong to the component that holds the
// bytes. This decorator reads the mirror store for the key Cloud resolved and fills those fields
// in, so the policy sees one snapshot in which every field has an owner.
//
// A key the store has no record for leaves the mirror fields at their zero values, which the policy
// refuses. That is deliberate: Cloud saying "this workspace may read this key" is not the same as
// the key existing, and the difference must not be smoothed over into an allow.
type MirrorStateReader struct {
	authoritative CloudReader
	mirrors       *mirror.Store
}

// MirrorState builds the composed reader. Both halves are required: without Cloud's binding there
// is nothing to authorize, and without the store there is no mirror state to authorize against.
func MirrorState(authoritative CloudReader, mirrors *mirror.Store) (*MirrorStateReader, error) {
	if authoritative == nil {
		return nil, fmt.Errorf("the mirror state reader requires Cloud's authoritative reader")
	}
	if mirrors == nil {
		return nil, fmt.Errorf("the mirror state reader requires the mirror store")
	}
	return &MirrorStateReader{authoritative: authoritative, mirrors: mirrors}, nil
}

// Read answers Cloud's binding and the mirror store's state for one request.
func (m *MirrorStateReader) Read(ctx context.Context, origin gitread.Origin, digest string) (gitread.Snapshot, error) {
	snapshot, err := m.authoritative.ReadGitMirror(ctx, origin, digest)
	if err != nil {
		return gitread.Snapshot{}, err
	}
	if !snapshot.Found {
		return snapshot, nil
	}
	record, found, err := m.mirrors.Lookup(snapshot.Key)
	if err != nil {
		// The store could not be read. That is unavailability, not a refusal: the caller did
		// nothing wrong, and answering "forbidden" would turn a damaged volume into a decision.
		return gitread.Snapshot{}, fmt.Errorf("read the mirror record: %w", err)
	}
	if !found {
		return snapshot, nil
	}
	snapshot.MirrorGeneration = record.Generation
	snapshot.MirrorReady = record.State == mirror.StateReady
	snapshot.MirrorRevoked = record.State == mirror.StateRevoked
	return snapshot, nil
}
