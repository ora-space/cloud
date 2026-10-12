package gitserver

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/wanglongan587/cloud/internal/gitread"
	"github.com/wanglongan587/cloud/internal/mirror"
)

// cloudStub is Cloud's authoritative reader reduced to the one question the Git Server asks: the
// binding for this caller. Answers are fixed so each case can vary the binding and the store
// independently, and `seen` proves how many times Cloud was asked.
type cloudStub struct {
	snapshot gitread.Snapshot
	err      error
	seen     int
}

func (c *cloudStub) ReadGitMirror(_ context.Context, _ gitread.Origin, _ string) (gitread.Snapshot, error) {
	c.seen++
	return c.snapshot, c.err
}

// binding is what Cloud answers for the caller testOrigin() when it is fully bound to a published
// key: the live container, one tenant on both sides, and the project that owns the key. It carries
// no mirror state, because Cloud's reader does not own any (see internal/core's read test).
func binding(key mirror.Key) gitread.Snapshot {
	origin := testOrigin()
	return gitread.Snapshot{
		Found: true, Key: key, ContainerLive: true,
		WorkspaceID: origin.WorkspaceID, ContainerID: origin.ContainerID, RuntimeGeneration: 3,
		WorkspaceProjectID: key.ProjectID,
		WorkspaceTenantID:  testTenant, ProjectTenantID: testTenant,
	}
}

// request is the read this package decides, aimed at the key's digest from the caller testOrigin().
func request(key mirror.Key) gitread.Request {
	return gitread.Request{Origin: testOrigin(), MirrorDigest: key.Digest(), Operation: gitread.OperationUploadPack}
}

// stateFixture pairs a real mirror store with one published mirror and Cloud's binding for it.
type stateFixture struct {
	store  *mirror.Store
	key    mirror.Key
	record mirror.Record
	cloud  *cloudStub
	reader *MirrorStateReader
}

func newStateFixture(t *testing.T) *stateFixture {
	t.Helper()
	store := newMirrorStore(t)
	source, commit := sourceRepository(t, map[string]string{"README.md": "# demo\n"})
	key := testKey(commit)
	record := publishMirror(t, store, key, source)
	cloud := &cloudStub{snapshot: binding(key)}
	reader, err := MirrorState(cloud, store)
	if err != nil {
		t.Fatalf("MirrorState: %v", err)
	}
	return &stateFixture{store: store, key: key, record: record, cloud: cloud, reader: reader}
}

// read asks the composed reader for the fixture's key, the way the server does for one request.
func (f *stateFixture) read(t *testing.T) gitread.Snapshot {
	t.Helper()
	snapshot, err := f.reader.Read(context.Background(), testOrigin(), f.key.Digest())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return snapshot
}

// TestMirrorStateCompletesABindingWithTheStoresOwnState is the positive control for the whole read
// authorization: Cloud's binding plus the store's record is what turns a request into an allow, and
// the allow binds the generation the store published.
func TestMirrorStateCompletesABindingWithTheStoresOwnState(t *testing.T) {
	f := newStateFixture(t)
	snapshot := f.read(t)
	if !snapshot.MirrorReady || snapshot.MirrorGeneration != f.record.Generation {
		t.Fatalf("the composed snapshot carries %+v, want the store's ready generation %d", snapshot, f.record.Generation)
	}
	verdict := request(f.key).Evaluate(snapshot)
	if !verdict.Allow {
		t.Fatalf("a published mirror with a complete binding was refused with %s", verdict.Reason)
	}
	if verdict.Generation != f.record.Generation {
		t.Fatalf("the verdict binds generation %d, want %d", verdict.Generation, f.record.Generation)
	}
	if f.cloud.seen != 1 {
		t.Fatalf("Cloud was read %d times for one request", f.cloud.seen)
	}
}

// TestMirrorStateRefusesEveryStateAMirrorStopsBeingReadable walks the store's own lifecycle with a
// complete, correct binding behind it, so each refusal can only come from the mirror's state.
func TestMirrorStateRefusesEveryStateAMirrorStopsBeingReadable(t *testing.T) {
	cases := []struct {
		name   string
		damage func(t *testing.T, f *stateFixture)
		want   string
	}{
		{
			name: "the mirror was withdrawn",
			damage: func(t *testing.T, f *stateFixture) {
				if _, err := f.store.Revoke(f.key, "the upstream history was rewritten"); err != nil {
					t.Fatalf("Revoke: %v", err)
				}
			},
			want: gitread.ReasonMirrorRevoked,
		},
		{
			name: "the mirror was destroyed after the grace period",
			damage: func(t *testing.T, f *stateFixture) {
				if _, err := f.store.Destroy(f.key); err != nil {
					t.Fatalf("Destroy: %v", err)
				}
			},
			want: gitread.ReasonMirrorNotReady,
		},
		{
			name: "the preparation failed",
			damage: func(t *testing.T, f *stateFixture) {
				// A failed attempt is the same key taken from the beginning and abandoned. It leaves a
				// record that says so, which must not be readable.
				if _, err := f.store.Destroy(f.key); err != nil {
					t.Fatalf("Destroy: %v", err)
				}
				prep, err := f.store.Begin(context.Background(), f.key)
				if err != nil {
					t.Fatalf("Begin: %v", err)
				}
				defer func() { _ = prep.Close() }()
				if _, err := prep.Fail("the upstream refused the fetch"); err != nil {
					t.Fatalf("Fail: %v", err)
				}
			},
			want: gitread.ReasonMirrorNotReady,
		},
		{
			name: "no record exists for the key",
			damage: func(t *testing.T, f *stateFixture) {
				// The same as a preparation still in flight: nothing has been published, so there is
				// no record to consult at all. Cloud saying "this workspace may read this key" is not
				// the key existing, and the composition must not smooth the two together.
				if err := os.Remove(recordPath(f.store.Root(), f.key)); err != nil {
					t.Fatalf("remove the record: %v", err)
				}
			},
			want: gitread.ReasonMirrorNotReady,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newStateFixture(t)
			tc.damage(t, f)
			snapshot := f.read(t)
			verdict := request(f.key).Evaluate(snapshot)
			if verdict.Allow || verdict.Reason != tc.want {
				t.Fatalf("the mirror's state decided %+v, want %s", verdict, tc.want)
			}
		})
	}
}

// TestMirrorStateDoesNotAskTheStoreAboutAMirrorCloudRefused pins the order of the two halves: a
// caller Cloud does not bind has nothing to look up, and a store lookup that happened anyway would
// let the mirror's existence answer a question the binding already refused.
func TestMirrorStateDoesNotAskTheStoreAboutAMirrorCloudRefused(t *testing.T) {
	f := newStateFixture(t)
	f.cloud.snapshot = gitread.Snapshot{}
	snapshot := f.read(t)
	if snapshot.Found || snapshot.MirrorReady || snapshot.MirrorGeneration != 0 {
		t.Fatalf("an unbound request produced %+v", snapshot)
	}
	if verdict := request(f.key).Evaluate(snapshot); verdict.Allow {
		t.Fatal("an unbound request was authorized")
	}
}

// TestMirrorStateReportsAFailedCloudReadAsAnError keeps the authoritative failure out of the
// decision: the caller did nothing wrong, so the server must answer "unavailable", which is
// retryable, and not "forbidden", which would blame the sandbox.
func TestMirrorStateReportsAFailedCloudReadAsAnError(t *testing.T) {
	f := newStateFixture(t)
	f.cloud.err = errors.New("the database is unreachable")
	if _, err := f.reader.Read(context.Background(), testOrigin(), f.key.Digest()); err == nil {
		t.Fatal("a failed authoritative read was answered as a decision")
	}
}

// TestMirrorStateReportsAnUnreadableStoreAsUnavailable is the same rule for the other half: a
// damaged volume is unavailability, not a refusal about the caller.
func TestMirrorStateReportsAnUnreadableStoreAsUnavailable(t *testing.T) {
	f := newStateFixture(t)
	path := recordPath(f.store.Root(), f.key)
	// A record that cannot be parsed is read by the store as a damaged store rather than as an
	// absent mirror; the two must not be conflated, because only one of them is retryable.
	if err := os.WriteFile(path, []byte("{ this is not a record"), 0o600); err != nil {
		t.Fatalf("damage the record: %v", err)
	}
	if _, err := f.reader.Read(context.Background(), testOrigin(), f.key.Digest()); err == nil {
		t.Fatal("a damaged mirror store was answered as a decision")
	}
}

func TestMirrorStateRequiresBothHalves(t *testing.T) {
	store := newMirrorStore(t)
	if _, err := MirrorState(nil, store); err == nil {
		t.Fatal("a composed reader with no authoritative reader was built")
	}
	if _, err := MirrorState(&cloudStub{}, nil); err == nil {
		t.Fatal("a composed reader with no mirror store was built")
	}
}

// recordPath mirrors the store's own layout: records live under the root in a directory a reader
// outside the package has no accessor for. It is derived here rather than exported so that the
// layout stays the store's private business.
func recordPath(root string, key mirror.Key) string {
	return filepath.Join(root, "records", key.Digest()+".json")
}
