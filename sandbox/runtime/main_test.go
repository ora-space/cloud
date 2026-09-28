package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestOutputBound(t *testing.T) {
	b := &boundedBuffer{}
	p := []byte(strings.Repeat("x", outputLimit+17))
	n, e := b.Write(p)
	if e != nil || n != len(p) || len(b.String()) != outputLimit || !b.truncated {
		t.Fatal(n, e, len(b.String()), b.truncated)
	}
}

func TestTimeoutKillsProcessGroup(t *testing.T) {
	start := time.Now()
	r := run(context.Background(), request{Command: []string{"/bin/sh", "-c", "sleep 20 & wait"}, Cwd: t.TempDir(), Timeout: "100ms"})
	if !r.TimedOut || time.Since(start) > 3*time.Second {
		t.Fatalf("%+v", r)
	}
}

func TestExitAndStderr(t *testing.T) {
	r := run(context.Background(), request{Command: []string{"/bin/sh", "-c", "echo ok; echo bad >&2; exit 7"}, Cwd: t.TempDir()})
	if r.ExitCode != 7 || r.Stdout != "ok\n" || r.Stderr != "bad\n" {
		t.Fatalf("%+v", r)
	}
}
