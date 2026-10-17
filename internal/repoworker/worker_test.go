package repoworker

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/internal/cloneauth"
	"github.com/wanglongan587/cloud/internal/core"
	"github.com/wanglongan587/cloud/internal/repoexec"
)

func TestNewRejectsAnIncompleteConfiguration(t *testing.T) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generating the signing key: %v", err)
	}
	valid := func() *Config {
		return &Config{Store: &fakeStore{}, Executor: &fakeExecutor{}, Refs: &fakeRefs{}, Key: key, Owner: "worker-1"}
	}
	for _, tc := range []struct {
		name  string
		tweak func(*Config)
	}{
		{"no configuration at all", nil},
		{"no store", func(c *Config) { c.Store = nil }},
		{"no executor", func(c *Config) { c.Executor = nil }},
		{"no ref reader", func(c *Config) { c.Refs = nil }},
		{"no signing key", func(c *Config) { c.Key = nil }},
		{"a key of the wrong size", func(c *Config) { c.Key = key[:16] }},
		{"no owner", func(c *Config) { c.Owner = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.tweak == nil {
				if _, err := New(nil); err == nil {
					t.Fatal("New accepted a nil configuration")
				}
				return
			}
			cfg := valid()
			tc.tweak(cfg)
			if _, err := New(cfg); err == nil {
				t.Fatal("New accepted an incomplete configuration")
			}
		})
	}
	if _, err := New(valid()); err != nil {
		t.Fatalf("New rejected a complete configuration: %v", err)
	}
}

func TestNewAppliesTheDocumentedDefaults(t *testing.T) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generating the signing key: %v", err)
	}
	w, err := New(&Config{Store: &fakeStore{}, Executor: &fakeExecutor{}, Refs: &fakeRefs{}, Key: key, Owner: "worker-1"})
	if err != nil {
		t.Fatalf("building the worker: %v", err)
	}
	for _, tc := range []struct {
		name string
		got  any
		want any
	}{
		{"slots", w.cfg.Slots, 4},
		{"slot capacity", cap(w.slots), 4},
		{"lease", w.cfg.Lease, time.Minute},
		{"sync timeout", w.cfg.SyncTimeout, 30 * time.Minute},
		{"publish timeout", w.cfg.PublishTimeout, 5 * time.Minute},
		{"probe timeout", w.cfg.ProbeTimeout, 5 * time.Minute},
		{"interval", w.cfg.Interval, 2 * time.Second},
	} {
		if tc.got != tc.want {
			t.Fatalf("%s = %v, want %v", tc.name, tc.got, tc.want)
		}
	}
}

func TestPassSweepsAndBoundsWhatItStarts(t *testing.T) {
	store := &fakeStore{
		leased:    []core.RepositoryTask{syncTask(), syncTask(), syncTask(), syncTask()},
		sweepErrs: map[string]error{"approval expiry": errors.New("expired approvals are not due")},
	}
	entered := make(chan struct{}, 4)
	release := make(chan struct{})
	exec := &fakeExecutor{run: func(ctx context.Context, _ *repoexec.Task) (repoexec.Result, error) {
		entered <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
			return repoexec.Result{}, ctx.Err()
		}
		return repoexec.Result{Classification: repoexec.Failed, ErrorCode: "fetch_failed"}, nil
	}}
	w := newWorker(t, store, exec, &fakeRefs{}, func(c *Config) { c.Slots = 2 })

	w.Pass(context.Background())
	await(t, entered, "the first task to reach the executor")
	await(t, entered, "the second task to reach the executor")

	// Four tasks were pending but only the free slots may be leased.
	if store.leaseLimit != 2 {
		t.Fatalf("lease limit = %d, want the free slots", store.leaseLimit)
	}
	if store.leaseOwner != "worker-1" {
		t.Fatalf("lease owner = %q, want worker-1", store.leaseOwner)
	}
	if got := exec.peakLive(); got > 2 {
		t.Fatalf("%d tasks ran at once, want at most 2", got)
	}
	for _, sweep := range []string{"lease sweep", "approval expiry", "abandoned submissions", "store revocations"} {
		if got := store.ran(sweep); got != 1 {
			t.Fatalf("the %s sweep ran %d times, want 1", sweep, got)
		}
	}
	// A failing sweep must not stop the pass, and a full worker must not lease or start anything.
	w.Pass(context.Background())
	if got := store.ran("lease"); got != 1 {
		t.Fatalf("lease calls = %d, want 1: a pass with no free slot must not lease", got)
	}
	if got := store.ran("checks"); got != 0 {
		t.Fatalf("checks ran %d times with no free slot, want 0", got)
	}
	if got := store.ran("verifications"); got != 0 {
		t.Fatalf("verifications ran %d times with no free slot, want 0", got)
	}

	close(release)
	w.tasks.Wait()
	if got := len(store.syncs); got != 2 {
		t.Fatalf("%d syncs settled, want the 2 that ran", got)
	}
}

func TestPassRunsChecksAndVerificationsWhenSlotsAreFree(t *testing.T) {
	store := &fakeStore{checks: []core.RepositoryCheck{publishCheck()}, probes: []core.CredentialCheck{credentialCheck()}}
	exec := &fakeExecutor{run: func(_ context.Context, task *repoexec.Task) (repoexec.Result, error) {
		if task.Service == cloneauth.ServiceReceivePack {
			return repoexec.Result{Classification: repoexec.Applied, Advertised: true}, nil
		}
		return repoexec.Result{Classification: repoexec.Applied, Refs: map[string]string{"refs/heads/release": testCommit}}, nil
	}}
	w := newWorker(t, store, exec, &fakeRefs{}, func(c *Config) { c.Slots = 2 })

	w.Pass(context.Background())
	w.tasks.Wait()

	if got := store.ran("settle_check"); got != 1 {
		t.Fatalf("check settlements = %d, want 1", got)
	}
	if got := store.ran("settle_verification"); got != 1 {
		t.Fatalf("credential verification settlements = %d, want 1", got)
	}
}

func TestRunSettlesInFlightWorkWhenItIsCancelled(t *testing.T) {
	store := &fakeStore{leased: []core.RepositoryTask{publishTask()}}
	entered := make(chan struct{}, 1)
	exec := &fakeExecutor{run: func(ctx context.Context, _ *repoexec.Task) (repoexec.Result, error) {
		entered <- struct{}{}
		<-ctx.Done()
		return repoexec.Result{}, ctx.Err()
	}}
	w := newWorker(t, store, exec, &fakeRefs{}, func(c *Config) { c.Interval = time.Hour })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.Run(ctx)
	}()
	await(t, entered, "the executor to take the publish")
	cancel()
	await(t, done, "Run to return once its in-flight task settled")

	settle, ok := store.settledPublish()
	if !ok {
		t.Fatal("Run returned before its in-flight publish settled")
	}
	// A publish that was cancelled mid-call may have been pushed, so it stays reconcilable.
	want := core.PublishResult{Classification: "uncertain", ErrorCode: "executor_unreachable"}
	if settle.r != want {
		t.Fatalf("settled %+v, want %+v", settle.r, want)
	}
}
