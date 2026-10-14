package main

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/internal/mirror"
	"github.com/wanglongan587/cloud/internal/mirrorpublish"
)

// The reclaim loop is the one piece of this command that runs without a request to answer, so what is
// asserted here is its lifetime rather than its work: it has to stop when the process does, and a pass
// that cannot ask its question has to be reported and retried rather than end the loop. The work itself
// is `mirrorpublish.Maintain`'s, tested there.

// syncBuffer is a log destination a test can read while the loop is writing to it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// testMirrorStore is a store on a temporary volume, which is all the loop needs to be startable: the
// store opens no database and the references below are deliberately unanswerable.
func testMirrorStore(t *testing.T) *mirror.Store {
	t.Helper()
	store, err := mirror.New(mirror.Config{
		Root:           t.TempDir(),
		MaxMirrorBytes: 1 << 20,
		MaxVolumeBytes: 4 << 20,
		Grace:          time.Hour,
		Clock:          time.Now,
	})
	if err != nil {
		t.Fatalf("mirror.New: %v", err)
	}
	return store
}

// TestReclaimPassesAreReportedForTheWorkTheyDid is the loop's outward behavior, which is the only
// thing about it a deployment can observe: the process answers no request, so what it did exists in
// these log lines and nowhere else. The case that matters most is the observation, because leaving it
// out is invisible in the obvious test — a deployment whose only report is the deletion cannot be
// told apart from one that deleted a mirror the pass before it was last read.
func TestReclaimPassesAreReportedForTheWorkTheyDid(t *testing.T) {
	key := mirror.Key{ProjectID: "p1", Repository: "https://git.example.invalid/repo", Commit: strings.Repeat("a", 40)}
	cases := []struct {
		name   string
		report mirrorpublish.Report
		want   bool
	}{
		{
			// The steady state, and the reason this is a predicate rather than "log every pass": a
			// loop that announced every pass would write a line every interval forever.
			name: "a pass that found nothing to do",
			want: false,
		},
		{
			// The case this predicate was fixed for: the countdown to a deletion has started, and
			// this is the only line that says so.
			name:   "a pass that first saw a mirror unreferenced",
			report: mirrorpublish.Report{Sweep: mirror.SweepReport{ObservedUnreferenced: []mirror.Key{key}}},
			want:   true,
		},
		{
			name:   "a pass that destroyed a mirror",
			report: mirrorpublish.Report{Sweep: mirror.SweepReport{Destroyed: []mirror.Key{key}}},
			want:   true,
		},
		{
			name:   "a pass that withdrew a mirror",
			report: mirrorpublish.Report{Revoked: []mirror.Key{key}},
			want:   true,
		},
		{
			// A key that came back is not new work — it is the countdown stopping, and the pass that
			// reports it would otherwise be the pass that says nothing changed.
			name:   "a pass that cleared an unreferenced mark",
			report: mirrorpublish.Report{Sweep: mirror.SweepReport{ReclaimedReferences: []mirror.Key{key}}},
			want:   false,
		},
		{
			name:   "a pass that removed abandoned staging",
			report: mirrorpublish.Report{Sweep: mirror.SweepReport{StagingRemoved: 1}},
			want:   true,
		},
		{
			name:   "a pass that removed an orphan tree",
			report: mirrorpublish.Report{Sweep: mirror.SweepReport{OrphansRemoved: 1}},
			want:   true,
		},
		{
			// References that are ready but needed nothing done. Every pass of a healthy deployment
			// looks like this, so treating it as work would make the log one line per interval.
			name:   "a pass whose references were all already published",
			report: mirrorpublish.Report{Ready: []mirror.Key{key}},
			want:   false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := reclaimWorthReporting(&c.report); got != c.want {
				t.Fatalf("reclaimWorthReporting(%+v) = %v, want %v", c.report, got, c.want)
			}
		})
	}
}

// TestRunMirrorReclaimStopsWithItsContext is the shutdown path. The loop is a goroutine of a process
// that shuts down on a signal, so a loop that ignored its context would hold the exit open — and the
// deployment would only ever stop by being killed.
func TestRunMirrorReclaimStopsWithItsContext(t *testing.T) {
	logs := &syncBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		// A nil pool is a database that cannot answer, which is the state this loop must survive. The
		// interval is short so the loop is mid-pass rather than parked when the context is cancelled.
		runMirrorReclaim(ctx, nil, testMirrorStore(t), time.Millisecond, slog.New(slog.NewTextHandler(logs, nil)))
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the reclaim loop did not stop when its context was cancelled")
	}
}

// TestRunMirrorReclaimKeepsGoingWhenAPassCannotAsk pins the other half of the lifetime: a pass that
// could not read Cloud's answer is a warning, not the end of the loop. Ending it would mean a
// deployment that was briefly between database connections never reclaimed anything again, and the
// failure would be one log line at moment zero and silence afterwards.
func TestRunMirrorReclaimKeepsGoingWhenAPassCannotAsk(t *testing.T) {
	logs := &syncBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		runMirrorReclaim(ctx, nil, testMirrorStore(t), time.Millisecond, slog.New(slog.NewTextHandler(logs, nil)))
	}()
	// The loop retries on its own interval, so waiting for one line proves it took a second pass rather
	// than exiting at the first failure.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Count(logs.String(), "mirror reclaim pass failed") >= 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the reclaim loop did not stop when its context was cancelled")
	}
	if got := strings.Count(logs.String(), "mirror reclaim pass failed"); got < 2 {
		t.Fatalf("the loop reported %d failed passes, want it to have tried again: %s", got, logs.String())
	}
	// The reason is in the log, because an operator reading "the reclaim loop is running but doing
	// nothing" has to be able to find out why.
	if !strings.Contains(logs.String(), "the reference reader needs Cloud's database pool") {
		t.Fatalf("the failure does not say what was wrong: %s", logs.String())
	}
}
