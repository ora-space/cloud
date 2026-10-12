package mirror

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gofrs/flock"
)

// Reason codes a mirror can fail or be withdrawn with. They are stable because they are reported to
// an operator and recorded in the mirror's own record; they never carry upstream or credential
// detail, because a mirror failure is not a place a secret may surface.
const (
	// ReasonSizeLimit means the staged content exceeded the per-mirror cap. It is terminal for the
	// attempt and the staged content is discarded: a partially published oversized mirror is exactly
	// what the cap exists to prevent.
	ReasonSizeLimit = "mirror_size_limit"
	// ReasonVolumeLimit means publishing would exceed the store's total cap. The mirror is not
	// published; the attempt may succeed later, so this reason is not terminal.
	ReasonVolumeLimit = "mirror_volume_limit"
	// ReasonInterrupted means a preparation stopped without publishing or failing — its writer died.
	ReasonInterrupted = "mirror_preparation_interrupted"
	// ReasonWithdrawn is the default reason for a deliberate revocation.
	ReasonWithdrawn = "mirror_withdrawn"
	// ReasonWriterFailed means the writer reported its own failure.
	ReasonWriterFailed = "mirror_writer_failed"
)

// ErrNotReady reports that a key does not name a mirror that may be read right now. Every cause —
// no record, still preparing, failed, revoked, destroyed — answers with this one error, because the
// Git Server must not let a caller distinguish "no such mirror" from "not yours to read".
var ErrNotReady = errors.New("mirror is not readable")

// ErrAlreadyPublished reports that the key already names a readable mirror. The content is
// immutable, so the caller reuses it instead of materializing the same commit a second time.
var ErrAlreadyPublished = errors.New("mirror is already published")

// ErrRevoked reports that the key was deliberately withdrawn. It is durable by design: a
// withdrawal an ordinary preparation could undo would not be a withdrawal.
var ErrRevoked = errors.New("mirror was revoked")

// ErrUnavailable reports that the store could not answer. It is deliberately distinct from
// ErrNotReady: "I cannot tell" must fail closed as unavailability, never as a policy refusal.
var ErrUnavailable = errors.New("mirror store is unavailable")

// AlreadyPublishedError carries the existing record so a caller that lost the race can use it.
type AlreadyPublishedError struct{ Record Record }

func (e *AlreadyPublishedError) Error() string {
	return fmt.Sprintf("%s: %s generation %d", ErrAlreadyPublished, e.Record.Key, e.Record.Generation)
}

// Is reports the error as ErrAlreadyPublished so callers can match with errors.Is.
func (e *AlreadyPublishedError) Is(target error) bool { return target == ErrAlreadyPublished }

// Config fixes a mirror store's limits. Every field is deployment-fixed: no caller passes a limit
// with a request, so no request can raise one.
type Config struct {
	// Root holds staging, published content and records. It must not be shared with a checkout root
	// or any directory a workload can write.
	Root string
	// MaxMirrorBytes caps one mirror's published size. Checked when the writer publishes, which is the
	// first moment the size is known without trusting the writer's own accounting.
	MaxMirrorBytes int64
	// MaxVolumeBytes caps the total published size across all mirrors.
	MaxVolumeBytes int64
	// Grace is how long a mirror stays after Cloud's authoritative references stop naming it. It is
	// measured from the moment a sweep first observes the key unreferenced, never from the mirror's age.
	Grace time.Duration
	// Clock is injected so grace periods are testable. Nil means time.Now.
	Clock func() time.Time
}

// Store is the mirror store. It is safe for concurrent use: preparations are serialized per key by a
// file lock, which is also what makes it safe to run the writer and the Git Server as separate
// processes over one root.
type Store struct {
	cfg Config
	// volumeLock guards the measure-then-rename step of publication. It is process-local because the
	// volume cap is a deployment budget, not a correctness boundary: two processes racing it can
	// overshoot by at most one mirror, and nothing is corrupted when they do.
	volumeLock sync.Mutex
}

// New validates the configuration and creates the directory skeleton. It does not touch the
// network, does not run Git, and never replaces content that is already there.
func New(cfg Config) (*Store, error) {
	if strings.TrimSpace(cfg.Root) == "" {
		return nil, errors.New("mirror store requires a root directory")
	}
	if !filepath.IsAbs(cfg.Root) {
		return nil, fmt.Errorf("mirror root %q must be an absolute path", cfg.Root)
	}
	if cfg.MaxMirrorBytes <= 0 {
		return nil, errors.New("mirror store requires a positive per-mirror size limit")
	}
	if cfg.MaxVolumeBytes < cfg.MaxMirrorBytes {
		return nil, errors.New("mirror store volume limit must not be smaller than the per-mirror limit")
	}
	if cfg.Grace < 0 {
		return nil, errors.New("mirror grace period must not be negative")
	}
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	store := &Store{cfg: cfg}
	for _, dir := range []string{store.stagingDir(), store.publishedDir(), store.recordsDir(), store.locksDir()} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create mirror store directory %s: %w", dir, err)
		}
	}
	return store, nil
}

// Root is the configured root, for a deployment that wants to report where its content lives.
func (s *Store) Root() string { return s.cfg.Root }

func (s *Store) stagingDir() string   { return filepath.Join(s.cfg.Root, "staging") }
func (s *Store) publishedDir() string { return filepath.Join(s.cfg.Root, "published") }
func (s *Store) recordsDir() string   { return filepath.Join(s.cfg.Root, "records") }
func (s *Store) locksDir() string     { return filepath.Join(s.cfg.Root, "locks") }

func (s *Store) recordPath(digest string) string {
	return filepath.Join(s.recordsDir(), digest+".json")
}

func (s *Store) contentDir(digest string, generation int64) string {
	return filepath.Join(s.publishedDir(), digest, fmt.Sprintf("gen-%d", generation))
}

// Preparation is one in-progress publication of one key. It holds the key's exclusive lock, so the
// caller must always finish it with Publish, Fail or Close — and Close is safe to defer.
type Preparation struct {
	store   *Store
	key     Key
	digest  string
	lock    *flock.Flock
	dir     string
	started time.Time
	// previous is the record that stood before this preparation, which decides whether a failure is
	// allowed to change anything.
	previous Record
	closed   bool
}

// Begin acquires the key's lock and opens a staging directory to write content into.
//
// It refuses a key whose mirror is already readable and present (ErrAlreadyPublished, with the
// record) and one that was deliberately withdrawn (ErrRevoked). Everything else begins normally,
// because everything else is a case where materializing the commit again is the correct action: a
// key never published, a failed attempt, content destroyed after the grace period, and a record that
// claims ready while its content is gone — that last one is damage, and the way out of it is a fresh
// generation rather than an operator editing records by hand.
func (s *Store) Begin(ctx context.Context, key Key) (*Preparation, error) {
	if err := key.Validate(); err != nil {
		return nil, err
	}
	digest := key.Digest()
	lock := flock.New(filepath.Join(s.locksDir(), digest+".lock"))
	locked, err := lock.TryLockContext(ctx, 50*time.Millisecond)
	if err != nil {
		return nil, fmt.Errorf("%w: waiting for the key lock: %v", ErrUnavailable, err)
	}
	if !locked {
		return nil, fmt.Errorf("%w: the key lock could not be taken", ErrUnavailable)
	}
	prep := &Preparation{store: s, key: key, digest: digest, lock: lock, started: s.cfg.Clock()}
	previous, found, err := s.Lookup(key)
	if err != nil {
		return nil, prep.abort(err)
	}
	prep.previous = previous
	if found {
		switch previous.State {
		case StateReady:
			if s.contentPresent(&previous) {
				return nil, prep.abort(&AlreadyPublishedError{Record: previous})
			}
			// The record promises content that is not on disk. Replacing it is the only recovery, and
			// the generation counter is what makes the replacement distinguishable from the original.
		case StateRevoked:
			return nil, prep.abort(ErrRevoked)
		case StatePreparing:
			// Holding the lock proves the previous writer is gone: the lock died with it. Its staging
			// directory is debris, and leaving it would double-count against the volume cap.
			if err := s.removeStaging(digest); err != nil {
				return nil, prep.abort(err)
			}
		case StateFailed, StateDestroyed:
		}
	}
	dir, err := os.MkdirTemp(s.stagingDir(), digest+".")
	if err != nil {
		return nil, prep.abort(fmt.Errorf("%w: create staging directory: %v", ErrUnavailable, err))
	}
	prep.dir = dir
	// Record "preparing" only when it cannot downgrade something readable. A republish of a key whose
	// mirror is live must not make the live mirror unreadable for as long as the republish takes.
	if !found || !previous.Readable() {
		record := Record{
			Key:        key,
			State:      StatePreparing,
			Generation: previous.Generation,
			Ref:        previous.Ref,
			CreatedAt:  prep.started,
			ChangedAt:  prep.started,
		}
		if err := s.writeRecord(&record); err != nil {
			return nil, prep.abort(err)
		}
	}
	return prep, nil
}

// Dir is where the writer materializes the bare repository. Its contents are invisible to readers
// until Publish renames the whole directory into the published area.
func (p *Preparation) Dir() string { return p.dir }

// Key is the key this preparation publishes.
func (p *Preparation) Key() Key { return p.key }

// Publish verifies, measures and atomically makes the staged content readable.
//
// The order is the visibility rule: the content is renamed into place first, and only then is the
// record written to say the mirror is ready. A reader therefore never sees "ready" pointing at
// content that has not arrived yet. The reverse window — content in place with no ready record — is
// invisible by construction, and a sweep reclaims it.
func (p *Preparation) Publish(ctx context.Context, verification PublishVerification) (Record, error) {
	if p.closed {
		return Record{}, errors.New("preparation is already finished")
	}
	if err := verification.Validate(p.key); err != nil {
		return Record{}, err
	}
	size, err := directorySize(p.dir)
	if err != nil {
		return Record{}, p.finishWithError(err)
	}
	if size > p.store.cfg.MaxMirrorBytes {
		_, err := p.failLocked(ReasonSizeLimit, fmt.Errorf("staged mirror is %d bytes, over the %d-byte limit", size, p.store.cfg.MaxMirrorBytes))
		return Record{}, err
	}
	// The volume accounting and the rename share one store-wide critical section, so two publications
	// of different keys cannot both measure a store with room for one of them.
	p.store.volumeLock.Lock()
	defer p.store.volumeLock.Unlock()
	used, err := p.store.publishedBytes()
	if err != nil {
		return Record{}, p.finishWithError(err)
	}
	if used+size > p.store.cfg.MaxVolumeBytes {
		_, err := p.failLocked(ReasonVolumeLimit, fmt.Errorf("publishing %d bytes would put the store at %d of %d bytes", size, used+size, p.store.cfg.MaxVolumeBytes))
		return Record{}, err
	}
	generation := p.previous.Generation + 1
	if generation < 1 {
		generation = 1
	}
	target := p.store.contentDir(p.digest, generation)
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return Record{}, p.finishWithError(fmt.Errorf("%w: %v", ErrUnavailable, err))
	}
	if err := os.Rename(p.dir, target); err != nil {
		return Record{}, p.finishWithError(fmt.Errorf("%w: publish content: %v", ErrUnavailable, err))
	}
	now := p.store.cfg.Clock()
	record := Record{
		Key:         p.key,
		State:       StateReady,
		Generation:  generation,
		Ref:         verification.Ref,
		Bytes:       size,
		CreatedAt:   p.started,
		ChangedAt:   now,
		PublishedAt: &now,
	}
	if err := p.store.writeRecord(&record); err != nil {
		// The content is in place but no record names it. That is the invisible direction: nothing can
		// read it, and the sweep reclaims it.
		p.release()
		return Record{}, err
	}
	p.release()
	return record, nil
}

// Fail abandons the preparation with a stable reason code and discards the staged content.
//
// A failure of a republish must not take a working mirror with it: the content of a given key is
// immutable, so if an earlier generation is readable it stays readable. The returned record is
// whatever stands after the failure.
func (p *Preparation) Fail(reason string) (Record, error) {
	if p.closed {
		return Record{}, errors.New("preparation is already finished")
	}
	if reason == "" {
		reason = ReasonWriterFailed
	}
	return p.failLocked(reason, nil)
}

// failLocked records the failure (when the key has no readable mirror) and removes the staging
// directory. The caller's own error, if any, is returned after the cleanup so a failure to clean up
// never masks the reason the publication failed.
func (p *Preparation) failLocked(reason string, cause error) (Record, error) {
	p.release()
	if err := p.store.removeStaging(p.digest); err != nil {
		return Record{}, err
	}
	if p.previous.Readable() {
		return p.previous, cause
	}
	now := p.store.cfg.Clock()
	record := Record{
		Key:        p.key,
		State:      StateFailed,
		Generation: p.previous.Generation,
		Ref:        p.previous.Ref,
		CreatedAt:  p.started,
		ChangedAt:  now,
		Reason:     reason,
	}
	if err := p.store.writeRecord(&record); err != nil {
		return Record{}, err
	}
	return record, cause
}

// finishWithError abandons the preparation after an infrastructure failure. It leaves the durable
// state alone unless the key had no readable mirror, in which case the key must not keep claiming a
// preparation that is over.
func (p *Preparation) finishWithError(cause error) error {
	if !p.closed {
		p.release()
		if !p.previous.Readable() {
			now := p.store.cfg.Clock()
			interrupted := Record{
				Key: p.key, State: StateFailed, Generation: p.previous.Generation, Ref: p.previous.Ref,
				CreatedAt: p.started, ChangedAt: now, Reason: ReasonInterrupted,
			}
			_ = p.store.writeRecord(&interrupted)
		}
	}
	return cause
}

// Close releases the lock and discards the staging directory when the preparation was never
// published or failed. Safe to call after Publish or Fail, and safe to defer.
func (p *Preparation) Close() error {
	if p.closed {
		return nil
	}
	p.release()
	return p.store.removeStaging(p.digest)
}

func (p *Preparation) release() {
	if p.closed {
		return
	}
	p.closed = true
	if err := p.lock.Unlock(); err != nil {
		// Unlocking is best-effort: the lock dies with the process anyway, and a caller has no action
		// to take. Errors from the file lock here would only mask the publication's own outcome.
		_ = err
	}
	if err := p.lock.Close(); err != nil {
		_ = err
	}
}

// abort releases the lock and returns cause, for the paths that fail before a staging directory
// exists.
func (p *Preparation) abort(cause error) error {
	if p.dir != "" {
		_ = os.RemoveAll(p.dir)
	}
	p.release()
	return cause
}

// Lookup reads a key's record. The bool distinguishes "no mirror was ever recorded here" from an
// error reading the store, which callers must not conflate.
func (s *Store) Lookup(key Key) (Record, bool, error) {
	if err := key.Validate(); err != nil {
		return Record{}, false, err
	}
	return s.readRecord(key.Digest())
}

func (s *Store) readRecord(digest string) (Record, bool, error) {
	body, err := os.ReadFile(s.recordPath(digest))
	if errors.Is(err, fs.ErrNotExist) {
		return Record{}, false, nil
	}
	if err != nil {
		return Record{}, false, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	var record Record
	if err := json.Unmarshal(body, &record); err != nil {
		return Record{}, false, fmt.Errorf("%w: mirror record %s is unreadable: %v", ErrUnavailable, digest, err)
	}
	return record, true, nil
}

// writeRecord replaces a record atomically: a reader sees the whole previous record or the whole
// new one, never a truncated mixture.
func (s *Store) writeRecord(record *Record) error {
	body, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return fmt.Errorf("encode mirror record: %w", err)
	}
	target := s.recordPath(record.Key.Digest())
	temp, err := os.CreateTemp(s.recordsDir(), ".record-*")
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	name := temp.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err := temp.Write(append(body, '\n')); err != nil {
		temp.Close()
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if err := os.Chmod(name, 0o600); err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if err := os.Rename(name, target); err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return nil
}

// Handle is a readable mirror: the record that permits the read and the directory holding content.
type Handle struct {
	Record Record
	Dir    string
}

// Open resolves a key to readable content.
//
// It answers ErrNotReady for every reason a caller is not entitled to read — nothing recorded,
// still preparing, failed, withdrawn, destroyed — so the Git Server cannot accidentally tell a
// caller which of those it was. A record that says ready while its content is missing answers
// ErrUnavailable instead: that is damage, not policy, and it must not look like a refusal.
func (s *Store) Open(key Key) (Handle, error) {
	record, found, err := s.Lookup(key)
	if err != nil {
		return Handle{}, err
	}
	if !found || !record.Readable() {
		return Handle{}, ErrNotReady
	}
	dir := s.contentDir(key.Digest(), record.Generation)
	if !s.contentPresent(&record) {
		return Handle{}, fmt.Errorf("%w: published content for %s generation %d is missing", ErrUnavailable, key, record.Generation)
	}
	return Handle{Record: record, Dir: dir}, nil
}

// contentPresent reports whether a readable record's published tree is actually on disk. It is the
// difference between "this mirror may not be read" and "this store is damaged", and both the reader
// and the writer need to tell those apart.
func (s *Store) contentPresent(record *Record) bool {
	info, err := os.Stat(s.contentDir(record.Key.Digest(), record.Generation))
	return err == nil && info.IsDir()
}

// Revoke stops a mirror being readable, durably.
//
// The record is rewritten before anything is deleted, so the read permission ends at the write and
// the bytes can wait for a grace period. A revocation is not undone by materializing the key again;
// see Begin.
func (s *Store) Revoke(key Key, reason string) (Record, error) {
	record, found, err := s.Lookup(key)
	if err != nil {
		return Record{}, err
	}
	if !found {
		return Record{}, ErrNotReady
	}
	if reason == "" {
		reason = ReasonWithdrawn
	}
	record.State = StateRevoked
	record.Reason = reason
	record.ChangedAt = s.cfg.Clock()
	if err := s.writeRecord(&record); err != nil {
		return Record{}, err
	}
	return record, nil
}

// Destroy removes a mirror's published content and marks the record terminal. Callers use it after
// the grace period; it is idempotent so a retried sweep is safe.
func (s *Store) Destroy(key Key) (Record, error) {
	s.volumeLock.Lock()
	defer s.volumeLock.Unlock()
	record, found, err := s.Lookup(key)
	if err != nil {
		return Record{}, err
	}
	if !found {
		return Record{}, ErrNotReady
	}
	if record.State == StateDestroyed {
		return record, nil
	}
	if err := os.RemoveAll(filepath.Join(s.publishedDir(), key.Digest())); err != nil {
		return Record{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	record.State = StateDestroyed
	record.Bytes = 0
	record.Ref = ""
	record.ChangedAt = s.cfg.Clock()
	if err := s.writeRecord(&record); err != nil {
		return Record{}, err
	}
	return record, nil
}

// Records lists every mirror the store knows, ordered by key digest so a report is reproducible.
func (s *Store) Records() ([]Record, error) {
	entries, err := os.ReadDir(s.recordsDir())
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	records := make([]Record, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		record, found, err := s.readRecord(strings.TrimSuffix(entry.Name(), ".json"))
		if err != nil {
			return nil, err
		}
		if found {
			records = append(records, record)
		}
	}
	sort.Slice(records, func(i, j int) bool {
		return records[i].Key.Digest() < records[j].Key.Digest()
	})
	return records, nil
}

// removeStaging deletes every staging directory belonging to one key. There is at most one while a
// caller holds the key's lock, but a crashed writer can leave more than one.
func (s *Store) removeStaging(digest string) error {
	entries, err := os.ReadDir(s.stagingDir())
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), digest+".") {
			continue
		}
		if err := os.RemoveAll(filepath.Join(s.stagingDir(), entry.Name())); err != nil {
			return fmt.Errorf("%w: %v", ErrUnavailable, err)
		}
	}
	return nil
}

// publishedBytes sums the size of every readable mirror, which is what the volume cap is about:
// revoked and destroyed mirrors no longer hold content.
func (s *Store) publishedBytes() (int64, error) {
	records, err := s.Records()
	if err != nil {
		return 0, err
	}
	var total int64
	for i := range records {
		if records[i].Readable() {
			total += records[i].Bytes
		}
	}
	return total, nil
}

// directorySize sums a directory tree. It is how the store measures what a writer produced without
// trusting the writer's own accounting.
func directorySize(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(_ string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return total, nil
}
