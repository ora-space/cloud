package capture

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

func TestBufferBound(t *testing.T) {
	cases := []struct {
		name      string
		limit     int
		writes    []string
		kept      string
		dropped   int
		truncated bool
	}{
		{name: "under the bound", limit: 16, writes: []string{"git said no"}, kept: "git said no"},
		{name: "exactly the bound", limit: 4, writes: []string{"ab", "cd"}, kept: "abcd"},
		{name: "one write crossing the bound", limit: 4, writes: []string{"abcdef"}, kept: "abcd", dropped: 2, truncated: true},
		{name: "writes after the bound is full", limit: 2, writes: []string{"ab", "cd", "e"}, kept: "ab", dropped: 3, truncated: true},
		{name: "zero limit keeps nothing", limit: 0, writes: []string{"abc"}, kept: "", dropped: 3, truncated: true},
		{name: "empty write", limit: 4, writes: []string{""}, kept: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			buffer := &Buffer{Limit: tc.limit}
			total := 0
			for _, w := range tc.writes {
				n, err := buffer.Write([]byte(w))
				if err != nil || n != len(w) {
					t.Fatalf("Write(%q) = %d, %v; want %d, nil: a write must always be reported complete", w, n, err, len(w))
				}
				total += len(w)
			}
			if got := string(buffer.Bytes()); got != tc.kept {
				t.Fatalf("kept %q, want %q", got, tc.kept)
			}
			if buffer.Dropped != tc.dropped || buffer.Truncated() != tc.truncated {
				t.Fatalf("dropped=%d truncated=%v, want %d %v", buffer.Dropped, buffer.Truncated(), tc.dropped, tc.truncated)
			}
			if buffer.Len() != total {
				t.Fatalf("Len() = %d, want %d: the length is what the child wrote, not what was kept", buffer.Len(), total)
			}
		})
	}
}

func TestBufferStringMarksTruncation(t *testing.T) {
	buffer := &Buffer{Limit: 16}
	for i := 0; i < 100; i++ {
		_, _ = buffer.Write([]byte("0123456789"))
	}
	if got, want := buffer.String(), "0123456789012345…(984 bytes dropped)"; got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
	whole := &Buffer{Limit: 16}
	_, _ = whole.Write([]byte("short"))
	if got := whole.String(); got != "short" {
		t.Fatalf("an untruncated stream rendered as %q", got)
	}
}

// TestBufferCopyLargerThanBound is the regression case for the SIGPIPE defect: io.Copy through the
// buffer must read the whole stream without a short-write error.
func TestBufferCopyLargerThanBound(t *testing.T) {
	oversized := bytes.Repeat([]byte("a"), 3<<10)
	buffer := &Buffer{Limit: 1 << 10}
	copied, err := io.Copy(buffer, bytes.NewReader(oversized))
	if err != nil {
		t.Fatalf("copying a stream larger than the bound failed: %v", err)
	}
	if copied != int64(len(oversized)) {
		t.Fatalf("copied %d of %d bytes", copied, len(oversized))
	}
	if got := buffer.String(); !strings.HasPrefix(got, strings.Repeat("a", 1<<10)+"…") {
		t.Fatalf("kept %q, want the first 1024 bytes and a marker", got)
	}
}
