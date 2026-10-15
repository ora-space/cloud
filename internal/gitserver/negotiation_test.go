package gitserver

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

// This file is about the fetch negotiation body: what a real git client puts on the wire when its
// negotiation grows, and what this surface owes a body it did not write itself. Both halves need
// their own evidence — a client that compresses (which no test of this package can arrange by hand,
// because the decision is the client's and it is made from the size of the body about to be sent),
// and a server that reads what the client meant.

// TestAFetchWhoseNegotiationGrowsIsServed is the fault reported against the mirror surface: a first
// clone worked and a fetch did not.
//
// The arrangement is what the client decides on. A clone asks for one ref, sends `done`, and is
// answered in one round with a body far below the 1 KiB a client compresses above. A workspace that
// has committed, fetching the next generation of its mirror, has to offer its own commits first:
// nothing points at the mirror's commit, so the walk reaches it only after every local commit, and
// the `have` batches grow — 16, then 16 again, then more — until one of them crosses the threshold.
// That POST arrives deflated, and this surface used to hand the deflate stream to `upload-pack`,
// which answered with a protocol error the caller could only read as a broken server.
func TestAFetchWhoseNegotiationGrowsIsServed(t *testing.T) {
	// Both protocol versions are in the report, and they negotiate differently enough to be worth
	// keeping apart: v2 sends its first batch in the request that also names the want.
	for _, version := range []string{"2", "0"} {
		t.Run("protocol v"+version, func(t *testing.T) {
			f := newFixture(t)
			source, first := sourceWithHistory(t, 60)
			firstKey := testKey(first)
			f.authorize(publishMirror(t, f.store, firstKey, source))

			// The workspace is materialized the way a scripted clone does it: fetch the mirror by URL,
			// then branch from what arrived. No remote-tracking ref is left behind, which is the
			// arrangement this case needs rather than an accident of it — a second ref sitting on the
			// mirror's commit is a negotiation tip the server answers in the first round.
			workspace := filepath.Join(t.TempDir(), "workspace")
			if err := os.MkdirAll(workspace, 0o755); err != nil {
				t.Fatalf("create the workspace: %v", err)
			}
			runGit(t, workspace, "init", "-q", "-b", "work")
			runGit(t, workspace, "fetch", "--quiet", f.mirrorURL(firstKey), mirrorRef)
			runGit(t, workspace, "checkout", "-q", "-b", "work", "FETCH_HEAD")
			// The agent's work: the workspace's own commits, which the mirror does not have.
			commitLocally(t, workspace, 60)

			// The next generation of the same project, one commit further along.
			appendCommit(t, source, "advance")
			secondKey := testKey(runGit(t, source, "rev-parse", "HEAD"))
			f.authorize(publishMirror(t, f.store, secondKey, source))

			recording, encodings := recordingSurface(t, f)
			url := recording.URL + "/mirror/v1/" + secondKey.Digest() + ".git"
			out, err := gitCommand(t, workspace, "-c", "protocol.version="+version,
				"fetch", "--quiet", url, "+"+mirrorRef+":refs/heads/fetched")
			if err != nil {
				t.Fatalf("the fetch failed: %v\n%s\nencodings: %v", err, out, encodings())
			}
			// The case is about a compressed body, so it has to have been one. Without this, a client
			// that stopped compressing at this threshold would leave the case passing while it no
			// longer reaches the code it exists for.
			if !slices.Contains(encodings(), "gzip") {
				t.Fatalf("the client sent no compressed negotiation body (encodings %v): this case no longer "+
					"crosses the threshold it exists for", encodings())
			}
			// And the fetch has to have fetched: a body read as a packet stream is only useful if the
			// answer to it is the content the caller asked for.
			if got := runGit(t, workspace, "rev-parse", "refs/heads/fetched"); got != secondKey.Commit {
				t.Fatalf("the fetched ref resolves %s, want %s", got, secondKey.Commit)
			}
		})
	}
}

// TestAGzippedNegotiationBodyIsAnsweredLikeThePlainOne is the server's half on its own: the same
// request, once as the client sends it and once compressed, must be the same request to git.
//
// It is what pins the decoding to the body. A surface that merely stopped failing would pass the case
// above for the wrong reason — an answer to the deflate bytes as if they were a packet stream could
// still be a 200 — and this compares complete answers instead: the bytes git produced for the body
// the client meant, and nothing else.
func TestAGzippedNegotiationBodyIsAnsweredLikeThePlainOne(t *testing.T) {
	f := newFixture(t)
	source, commit := sourceRepository(t, map[string]string{"README.md": "# demo\n"})
	key := testKey(commit)
	f.authorize(publishMirror(t, f.store, key, source))
	path := "/mirror/v1/" + key.Digest() + ".git/git-upload-pack"
	body := fetchNegotiationBody(commit)

	plain := f.post(t, path, nil, body)
	plainAnswer := readBody(t, plain)
	if plain.StatusCode != http.StatusOK {
		t.Fatalf("the plain negotiation answered %d, want 200 (body %q)", plain.StatusCode, plainAnswer)
	}
	// `0008NAK` is upload-pack's first packet for a client with nothing in common and a `done`. The
	// answer below is only worth comparing if this one is an answer at all.
	if !bytes.HasPrefix(plainAnswer, []byte("0008NAK")) {
		t.Fatalf("the plain negotiation was not answered by upload-pack: %q", plainAnswer)
	}

	compressed := f.post(t, path, http.Header{"Content-Encoding": {"gzip"}}, deflate(t, body))
	compressedAnswer := readBody(t, compressed)
	if compressed.StatusCode != http.StatusOK {
		t.Fatalf("the compressed negotiation answered %d, want 200 (body %q)", compressed.StatusCode, compressedAnswer)
	}
	if !bytes.Equal(plainAnswer, compressedAnswer) {
		t.Fatalf("the compressed negotiation was answered differently:\nplain:      %q\ncompressed: %q",
			plainAnswer, compressedAnswer)
	}
}

// TestANegotiationBodyTheSurfaceCannotReadNeverReachesGit pins the boundary of what this surface
// implements: one encoding, or none.
//
// Passing an unknown encoding through is what produces the fault above — a 500 for a request the
// server could have described in one line — and a 500 tells the caller to retry something that can
// never work. The git program here would answer anything it was given, so a response that is neither
// 415 nor 400 would also be evidence that the body reached the child.
func TestANegotiationBodyTheSurfaceCannotReadNeverReachesGit(t *testing.T) {
	f, base, _ := authorizedFixture(t, fakeGit(t, "#!/bin/sh\necho ran\n"), 0)

	cases := map[string]struct {
		header http.Header
		body   []byte
		want   int
	}{
		"an encoding this surface does not implement": {
			header: http.Header{"Content-Encoding": {"deflate"}},
			body:   []byte("a body in a coding this surface never agreed to read"),
			want:   http.StatusUnsupportedMediaType,
		},
		"a body that is not the gzip it claims to be": {
			header: http.Header{"Content-Encoding": {"gzip"}},
			body:   []byte("not a deflate stream at all"),
			want:   http.StatusBadRequest,
		},
	}
	for name, c := range cases {
		response := f.post(t, base+"/git-upload-pack", c.header, c.body)
		body := readBody(t, response)
		if response.StatusCode != c.want {
			t.Fatalf("%s: status = %d, want %d (body %q)", name, response.StatusCode, c.want, body)
		}
		if strings.Contains(string(body), "ran") {
			t.Fatalf("%s: the body reached the git child process", name)
		}
	}

	// A request the surface can read is still served afterwards: the refusal is about one body, not
	// about a surface that has stopped working. The stand-in git above answers every invocation, so
	// what this asserts of the advertisement is the preamble the surface writes itself, before it runs
	// anything — which is all the real git would change.
	response := f.get(t, base+"/info/refs?service=git-upload-pack", nil)
	body := readBody(t, response)
	if response.StatusCode != http.StatusOK || !strings.HasPrefix(string(body), pktLine("# service=git-upload-pack\n")+"0000") {
		t.Fatalf("the surface stopped serving after a refusal: status %d, body %q", response.StatusCode, body)
	}
}

// sourceWithHistory builds a real repository whose main has the given number of commits, so that a
// client cloning it has a history of its own to negotiate with later. One commit would never make a
// client send more than a single batch of `have` lines.
func sourceWithHistory(t *testing.T, commits int) (string, string) {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "init", "-q", "-b", "main")
	for i := 0; i < commits; i++ {
		if err := os.WriteFile(filepath.Join(dir, "history.txt"), []byte(strings.Repeat("x", i+1)+"\n"), 0o644); err != nil {
			t.Fatalf("write history.txt: %v", err)
		}
		runGit(t, dir, "add", "-A")
		runGit(t, dir, "commit", "-q", "-m", "history")
	}
	return dir, runGit(t, dir, "rev-parse", "HEAD")
}

// appendCommit adds one commit to a repository built by sourceWithHistory, which is how a mirror's
// next generation differs from the one a client already has.
func appendCommit(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "history.txt"), []byte(body+"\n"), 0o644); err != nil {
		t.Fatalf("write history.txt: %v", err)
	}
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-q", "-m", body)
}

// commitLocally adds the given number of commits to a working copy, standing in for the work an agent
// does in a workspace between two mirror generations.
func commitLocally(t *testing.T, dir string, commits int) {
	t.Helper()
	for i := 0; i < commits; i++ {
		if err := os.WriteFile(filepath.Join(dir, "work.txt"), []byte(strings.Repeat("y", i+1)+"\n"), 0o644); err != nil {
			t.Fatalf("write work.txt: %v", err)
		}
		runGit(t, dir, "add", "-A")
		runGit(t, dir, "commit", "-q", "-m", "work")
	}
}

// fetchNegotiationBody is the body a client sends for a clone: the want line for the mirror's only
// ref, the flush that ends the want section, and `done`.
//
// `no-progress` is what makes two invocations comparable. Without it upload-pack reports its progress
// over the sideband channel, and those packets are written by a second stream that interleaves with
// the pack wherever it happens to be scheduled — which is a difference between two answers that says
// nothing about the body that produced them.
func fetchNegotiationBody(commit string) []byte {
	return []byte(pktLine("want "+commit+" multi_ack_detailed side-band-64k thin-pack no-progress ofs-delta\n") +
		"0000" + pktLine("done\n"))
}

// deflate is the compression a git client applies to a negotiation body over 1 KiB.
func deflate(t *testing.T, body []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	writer := gzip.NewWriter(&out)
	if _, err := writer.Write(body); err != nil {
		t.Fatalf("compress the body: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("compress the body: %v", err)
	}
	return out.Bytes()
}

// readBody reads a whole response and closes it, so a case can assert on the bytes a caller receives.
func readBody(t *testing.T, response *http.Response) []byte {
	t.Helper()
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatalf("read the response: %v", err)
	}
	return body
}

// recordingSurface serves the fixture's surface behind a wrapper that remembers the encoding every
// request carried.
//
// It is the only way to observe the compression from outside: the client does it, before this package
// sees a byte. A case that asserts on it is pinning the client behavior it depends on, which is the
// difference between a case that exercises the compressed path and one that merely passes.
func recordingSurface(t *testing.T, f *fixture) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var encodings []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		encodings = append(encodings, r.Header.Get("Content-Encoding"))
		mu.Unlock()
		f.server.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	return server, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(encodings)
	}
}
