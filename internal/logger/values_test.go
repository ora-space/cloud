package logger

import (
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// TestValuesRecordsThroughTheConfiguredLogger is the whole point of the adapter: a package that logs
// in the key/value shape must land in the logger the process was configured with, at the level it
// asked for, with its pairs intact. Anything the adapter changed — a second encoder, a fixed level,
// a pair dropped — would be a log stream an operator has to read separately and correlate by hand.
func TestValuesRecordsThroughTheConfiguredLogger(t *testing.T) {
	core, recorded := observer.New(zapcore.DebugLevel)
	values := NewValues(zap.New(core, zap.AddCaller()))

	values.Info("mirror published", "mirror", "digest", "tenant", "tenant-1")
	values.Warn("mirror could not be published", "mirror", "digest", "error", "upstream unreachable")

	entries := recorded.All()
	if len(entries) != 2 {
		t.Fatalf("recorded %d entries, want 2: %v", len(entries), entries)
	}
	info, warn := entries[0], entries[1]
	if info.Level != zapcore.InfoLevel || info.Message != "mirror published" {
		t.Fatalf("the first entry = %s %q, want INFO \"mirror published\"", info.Level, info.Message)
	}
	if got := info.ContextMap(); got["mirror"] != "digest" || got["tenant"] != "tenant-1" {
		t.Fatalf("the first entry's fields = %v, want the pairs it was given", got)
	}
	if warn.Level != zapcore.WarnLevel || warn.Message != "mirror could not be published" {
		t.Fatalf("the second entry = %s %q, want WARN with its message", warn.Level, warn.Message)
	}
	if got := warn.ContextMap(); got["error"] != "upstream unreachable" {
		t.Fatalf("the second entry's fields = %v, want the error it was given", got)
	}
	// The caller is the code that logged, not this adapter: an operator reading the line needs the
	// frame that decided to write it.
	if caller := info.Caller.Function; !strings.Contains(caller, "TestValuesRecordsThroughTheConfiguredLogger") {
		t.Fatalf("the recorded caller = %q, want the function that logged", caller)
	}
}

// TestValuesWithoutALoggerDiscards pins the nil case, which is the one every caller relies on
// without stating it: a component given no logger must not bring the process down when it logs.
func TestValuesWithoutALoggerDiscards(t *testing.T) {
	values := NewValues(nil)
	values.Info("nothing is listening", "mirror", "digest")
	values.Warn("nothing is listening", "mirror", "digest")
}
