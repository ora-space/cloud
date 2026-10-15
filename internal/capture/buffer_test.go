package capture

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

// TestTheBoundIsKeptAndTheDroppedBytesAreCounted is the memory half: what a child writes past the
// bound changes two numbers and nothing else.
func TestTheBoundIsKeptAndTheDroppedBytesAreCounted(t *testing.T) {
	buffer := &Buffer{Limit: 16}
	for i := 0; i < 100; i++ {
		if _, err := buffer.Write([]byte("0123456789")); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	if got := buffer.String(); got != "0123456789"+"012345"+"…(984 bytes dropped)" {
		t.Fatalf("kept %q, want the first 16 bytes and the count of the rest", got)
	}
	if buffer.Dropped != 984 {
		t.Fatalf("dropped %d bytes, want 984", buffer.Dropped)
	}
	if buffer.Len() != 1000 {
		t.Fatalf("Len() = %d, want 1000: the length is what the child wrote, not what was kept", buffer.Len())
	}
	if !buffer.Truncated() {
		t.Fatal("a buffer that dropped bytes does not report the truncation")
	}
}

// TestAStreamUnderTheBoundIsKeptWholeAndUnmarked is the other side of the same rule, so that the
// marker and the count mean something.
func TestAStreamUnderTheBoundIsKeptWholeAndUnmarked(t *testing.T) {
	buffer := &Buffer{Limit: 16}
	if _, err := buffer.Write([]byte("git said no")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := buffer.String(); got != "git said no" {
		t.Fatalf("kept %q, want the stream unchanged", got)
	}
	if buffer.Truncated() || buffer.Len() != 11 {
		t.Fatalf("a stream inside the bound reports truncation=%v len=%d", buffer.Truncated(), buffer.Len())
	}
}

// TestAWritePastTheBoundIsReportedAsComplete is the half that keeps the bound from becoming the
// child's problem.
//
// This is how a child's output actually reaches a Buffer: `exec` copies the pipe into the writer it
// was handed. `io.Copy` treats a short write as an error, and a copy that ends early takes the pipe
// with it — which is a Git killed by SIGPIPE for printing more than this process wanted to keep. So
// the assertion is not only that the write is reported complete, but that copying a stream larger
// than the bound through this writer finishes with the whole stream read and no error.
func TestAWritePastTheBoundIsReportedAsComplete(t *testing.T) {
	buffer := &Buffer{Limit: 1 << 10}
	oversized := bytes.Repeat([]byte("a"), 3<<10)

	n, err := buffer.Write(oversized)
	if err != nil {
		t.Fatalf("one write larger than the bound failed: %v", err)
	}
	if n != len(oversized) {
		t.Fatalf("one write larger than the bound reported %d of %d bytes", n, len(oversized))
	}

	reader := &Buffer{Limit: 1 << 10}
	copied, err := io.Copy(reader, bytes.NewReader(oversized))
	if err != nil {
		t.Fatalf("copying a stream larger than the bound failed: %v", err)
	}
	if copied != int64(len(oversized)) {
		t.Fatalf("copied %d of %d bytes", copied, len(oversized))
	}
	if got := reader.String(); !strings.HasPrefix(got, strings.Repeat("a", 1<<10)) {
		t.Fatalf("kept %q, want the first 1024 bytes", got)
	}
}
