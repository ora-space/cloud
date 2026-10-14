package mirror

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// SweepReport is what one sweep did, so a deployment can log a reclaim instead of assuming it.
type SweepReport struct {
	// ObservedUnreferenced lists keys that were already ready and were seen unreferenced for the first
	// time; their grace period starts now.
	ObservedUnreferenced []Key
	// ReclaimedReferences lists keys whose record was referenced again and whose unreferenced mark was
	// cleared. A key that comes back stops the clock rather than keeping a stale countdown.
	ReclaimedReferences []Key
	// Destroyed lists keys whose content was removed because the grace period had passed.
	Destroyed []Key
	// StagingRemoved counts abandoned staging directories reclaimed from writers that died.
	StagingRemoved int
	// OrphansRemoved counts published trees no record names, left behind by a publication that was
	// interrupted between the content rename and the record write.
	OrphansRemoved int
}

// Sweep reconciles the store against Cloud's authoritative reference set.
//
// live is the set of keys Cloud currently says are referenced. It is the only input that decides what
// survives: the store never treats its own records as evidence that a mirror is still wanted, because
// that would let a mirror keep itself alive (D6). Three things are reclaimed, in increasing order of
// destructiveness:
//
//   - staging directories whose writer is gone. Liveness is tested by taking the key's lock, not by
//     age, so a slow but healthy preparation is never mistaken for debris.
//   - published trees no record names, which are the harmless half of an interrupted publication.
//   - mirrors unreferenced for longer than the grace period. The unreferenced instant is recorded on
//     first observation, so the period means "unreferenced for this long" rather than "old" — a
//     mirror that is referenced again has its mark cleared and is never destroyed.
//
// A destroy skips a key whose lock is held, and never runs inside a critical section shared with
// readers. Grace is the actual protection for a read that is in flight: a mirror is destroyed only
// after Cloud stopped referencing it, which is the same state that makes a new read authorization
// fail. An in-flight `git upload-pack` on content removed mid-stream is therefore possible in
// principle and is bounded by the grace period, not eliminated; the reader sees a Git protocol error
// and retries, which is a refused read, never a wrong read.
func (s *Store) Sweep(ctx context.Context, live []Key, now time.Time) (SweepReport, error) {
	report := SweepReport{}
	referenced := make(map[string]struct{}, len(live))
	for _, key := range live {
		if err := key.Validate(); err != nil {
			return report, err
		}
		referenced[key.Digest()] = struct{}{}
	}
	records, err := s.Records()
	if err != nil {
		return report, err
	}
	for i := range records {
		record := records[i]
		if err := ctx.Err(); err != nil {
			return report, err
		}
		_, isReferenced := referenced[record.Key.Digest()]
		switch {
		case isReferenced:
			if record.UnreferencedSince != nil {
				record.UnreferencedSince = nil
				record.ChangedAt = now
				if err := s.writeRecord(&record); err != nil {
					return report, err
				}
				report.ReclaimedReferences = append(report.ReclaimedReferences, record.Key)
			}
		case record.State == StateDestroyed || record.State == StateFailed:
			// Nothing to hold and nothing to reclaim: a failed or destroyed record has no content.
		case record.UnreferencedSince == nil:
			record.UnreferencedSince = &now
			record.ChangedAt = now
			if err := s.writeRecord(&record); err != nil {
				return report, err
			}
			report.ObservedUnreferenced = append(report.ObservedUnreferenced, record.Key)
		case now.Sub(*record.UnreferencedSince) >= s.cfg.Grace:
			destroyed, err := s.destroyUnreferenced(record.Key)
			if err != nil {
				return report, err
			}
			if destroyed {
				report.Destroyed = append(report.Destroyed, record.Key)
			}
		}
	}
	staging, err := s.reclaimStaging()
	if err != nil {
		return report, err
	}
	report.StagingRemoved = staging
	orphans, err := s.reclaimOrphans(records, now)
	if err != nil {
		return report, err
	}
	report.OrphansRemoved = orphans
	return report, nil
}

// destroyUnreferenced destroys a key only when no writer is preparing it. A held lock means a
// publication is in flight, and the next sweep will see the key again — losing one grace period is
// cheaper than deleting content out from under a publication.
func (s *Store) destroyUnreferenced(key Key) (bool, error) {
	lock, err := s.keyLock(key.Digest())
	if err != nil {
		return false, err
	}
	free, err := lock.TryLock()
	if err != nil {
		return false, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer func() {
		if free {
			_ = lock.Unlock()
		}
		_ = lock.Close()
	}()
	if !free {
		return false, nil
	}
	if _, err := s.Destroy(key); err != nil {
		if errors.Is(err, ErrNotReady) {
			// The record vanished between listing and destroying; there is nothing left to reclaim.
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// reclaimStaging removes staging directories whose writer no longer exists. Taking the key's lock
// non-blockingly is the liveness test: a live writer holds it for the whole preparation, so a lock
// that can be taken proves the writer crashed, whatever the directory's age looks like.
func (s *Store) reclaimStaging() (int, error) {
	entries, err := os.ReadDir(s.stagingDir())
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	removed := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		digest, _, found := strings.Cut(entry.Name(), ".")
		if !found || len(digest) != 64 {
			// Not a staging directory this store created; leave it and say nothing, because removing a
			// path this package does not recognize is how a cleanup becomes a deletion.
			continue
		}
		lock, err := s.keyLock(digest)
		if err != nil {
			return removed, err
		}
		free, err := lock.TryLock()
		if err != nil {
			return removed, fmt.Errorf("%w: %v", ErrUnavailable, err)
		}
		if !free {
			_ = lock.Close()
			continue
		}
		removeErr := os.RemoveAll(filepath.Join(s.stagingDir(), entry.Name()))
		_ = lock.Unlock()
		_ = lock.Close()
		if removeErr != nil {
			return removed, fmt.Errorf("%w: %v", ErrUnavailable, removeErr)
		}
		removed++
	}
	return removed, nil
}

// reclaimOrphans removes published trees that no record names. A publication renames content into
// place and only then writes the ready record, so a crash in that window leaves a complete, valid,
// and completely invisible tree — which is the safe direction, and which would otherwise be charged
// to the volume forever.
func (s *Store) reclaimOrphans(records []Record, now time.Time) (int, error) {
	wanted := make(map[string]string, len(records))
	for i := range records {
		if records[i].Readable() {
			wanted[records[i].Key.Digest()] = filepath.Base(s.contentDir(records[i].Key.Digest(), records[i].Generation))
		}
	}
	keys, err := os.ReadDir(s.publishedDir())
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	removed := 0
	for _, keyDir := range keys {
		if !keyDir.IsDir() {
			continue
		}
		digest := keyDir.Name()
		generations, err := os.ReadDir(filepath.Join(s.publishedDir(), digest))
		if err != nil {
			return removed, fmt.Errorf("%w: %v", ErrUnavailable, err)
		}
		for _, generation := range generations {
			if !generation.IsDir() || generation.Name() == wanted[digest] {
				continue
			}
			// Age is the only signal available for a tree nothing refers to, and it is the right one
			// here: a tree in the publish window is younger than the grace period by orders of
			// magnitude, while a leaked one is not.
			info, err := generation.Info()
			if err != nil {
				continue
			}
			if now.Sub(info.ModTime()) < s.cfg.Grace {
				continue
			}
			target := filepath.Join(s.publishedDir(), digest, generation.Name())
			if err := os.RemoveAll(target); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return removed, fmt.Errorf("%w: %v", ErrUnavailable, err)
			}
			removed++
		}
		if err := pruneEmptyDir(filepath.Join(s.publishedDir(), digest)); err != nil {
			return removed, err
		}
	}
	return removed, nil
}

// pruneEmptyDir removes a key's published directory once its last generation is gone, so the
// published area does not accumulate a directory per key ever published.
func pruneEmptyDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if len(entries) > 0 {
		return nil
	}
	if err := os.Remove(dir); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return nil
}
