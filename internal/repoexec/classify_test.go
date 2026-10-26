package repoexec

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/wanglongan587/cloud/internal/gitserver"
)

func TestOperationHeaderMatchesTheGitServer(t *testing.T) {
	if operationHeader != gitserver.OperationHeader {
		t.Fatalf("executor header %q, Git Server header %q", operationHeader, gitserver.OperationHeader)
	}
}

// The ADR D8 combination table, row by row.
func TestClassifyPublishTable(t *testing.T) {
	old, source, other := strings.Repeat("1", 40), strings.Repeat("2", 40), strings.Repeat("3", 40)
	zero := strings.Repeat("0", 40)
	for _, c := range []struct {
		name      string
		push      pushOutcome
		post      string
		postOK    bool
		expected  string
		want      Classification
		confirmed bool
	}{
		{"ok, any post", outcomeOK, other, true, old, Applied, false},
		{"ok, post unreadable", outcomeOK, "", false, old, Applied, false},
		{"stale info", outcomeStale, old, true, old, Conflict, false},
		{"rejected, post = expected old", outcomeRejected, old, true, old, Rejected, false},
		{"rejected creating, post absent", outcomeRejected, "", true, zero, Rejected, false},
		{"rejected, post = source", outcomeRejected, source, true, old, Applied, true},
		{"rejected, post other", outcomeRejected, other, true, old, Conflict, false},
		{"rejected, post unreadable", outcomeRejected, "", false, old, Uncertain, false},
		{"no report, post = source", outcomeUnknown, source, true, old, Applied, true},
		{"no report, post = expected old", outcomeUnknown, old, true, old, Uncertain, false},
		{"no report, post other", outcomeUnknown, other, true, old, Uncertain, false},
		{"no report, post unreadable", outcomeUnknown, "", false, old, Uncertain, false},
	} {
		got, confirmed := classifyPublish(c.push, c.post, c.postOK, c.expected, source)
		if got != c.want || confirmed != c.confirmed {
			t.Errorf("%s: %s confirmed=%v, want %s confirmed=%v", c.name, got, confirmed, c.want, c.confirmed)
		}
	}
}

func TestPorcelainOutcomes(t *testing.T) {
	stdout := "To https://example.com/r\n" +
		" \tabc:refs/heads/ff\t111..222\n" +
		"+\tabc:refs/heads/forced\t111...222 (forced update)\n" +
		"*\tabc:refs/heads/new\t[new reference]\n" +
		"=\tabc:refs/heads/same\t[up to date]\n" +
		"!\tabc:refs/heads/stale\t[rejected] (stale info)\n" +
		"!\tabc:refs/heads/hook\t[remote rejected] (pre-receive hook declined)\n" +
		"!\tabc:refs/heads/atomic\t[rejected] (atomic push failed)\n" +
		"!\tabc:refs/heads/lost\t[remote failure] (remote failed to report status)\n" +
		"!\tabc:refs/heads/twice\t[rejected] (stale info)\n" +
		" \tabc:refs/heads/twice\t111..222\n" +
		"Done\n"
	lines := parsePorcelain(stdout)
	for ref, want := range map[string]pushOutcome{
		"refs/heads/ff": outcomeOK, "refs/heads/forced": outcomeOK, "refs/heads/new": outcomeOK, "refs/heads/same": outcomeOK,
		"refs/heads/stale": outcomeStale, "refs/heads/hook": outcomeRejected, "refs/heads/atomic": outcomeAtomicFailed,
		"refs/heads/lost": outcomeUnknown,
	} {
		line, ok := lines[ref]
		if !ok || line.outcome() != want {
			t.Errorf("%s: %v (present %v), want %v", ref, line.outcome(), ok, want)
		}
	}
	if _, ok := lines["refs/heads/twice"]; ok {
		t.Error("a ref reported twice was read as one outcome")
	}
}

func TestParseLsRemote(t *testing.T) {
	a := strings.Repeat("a", 40)
	refs, ok := parseLsRemote(a + "\trefs/heads/main\n")
	if !ok || refs["refs/heads/main"] != a {
		t.Fatalf("refs = %v %v", refs, ok)
	}
	if refs, ok := parseLsRemote(""); !ok || len(refs) != 0 {
		t.Fatalf("empty answer = %v %v, want no refs", refs, ok)
	}
	for _, bad := range []string{"garbage\n", a + " refs/heads/main\n", "ABC\trefs/heads/x\n", a + "\tHEAD\n", a + "\trefs/x\n" + a + "\trefs/x\n"} {
		if _, ok := parseLsRemote(bad); ok {
			t.Errorf("parseLsRemote(%q) accepted", bad)
		}
	}
}

func TestExternalFailureCodes(t *testing.T) {
	for _, c := range []struct {
		stderr    string
		anonymous bool
		want      string
	}{
		{"fatal: couldn't find remote ref refs/heads/nope", false, CodeUpstreamBranchMissing},
		{"fatal: could not read Username for 'https://h': terminal prompts disabled", true, CodeRequiresCredential},
		{"fatal: repository 'https://h/r/' not found", true, CodeRequiresCredential},
		{"fatal: Authentication failed for 'https://h/r/'", false, CodeCredentialRejected},
		{"fatal: unable to access 'https://h/r/': The requested URL returned error: 403", false, CodeCredentialRejected},
		{"fatal: repository 'https://h/r/' not found", false, CodeFetchFailed},
		{"fatal: unable to access 'https://h/r/': Could not resolve host", false, CodeFetchFailed},
	} {
		if got := externalFailure(c.stderr, c.anonymous); got != c.want {
			t.Errorf("externalFailure(%q, %v) = %s, want %s", c.stderr, c.anonymous, got, c.want)
		}
	}
	if !pushNeverSent("fatal: unable to access 'https://h/r/': The requested URL returned error: 403") {
		t.Error("a discovery failure was not recognized as never sent")
	}
	if pushNeverSent("error: RPC failed; HTTP 403 curl 22 The requested URL returned error: 403\nfatal: unable to access") {
		t.Error("an RPC failure was read as never sent")
	}
}

// Redaction runs before truncation, covers the encodings a secret travels in, and strips URL
// userinfo.
func TestRedact(t *testing.T) {
	secret := "ghp_secretvalue"
	pair := "x-access-token:" + secret
	text := strings.Repeat("x", detailLimit-5) + secret + " tail " + base64.StdEncoding.EncodeToString([]byte(pair)) + " https://user:pw@host/path"
	got := redact(text, basicAuthSecrets("x-access-token", secret))
	if strings.Contains(got, secret) || strings.Contains(got, "ghp_") || len(got) > detailLimit {
		t.Fatalf("redact kept part of the secret or exceeded the limit: %q", got[len(got)-40:])
	}
	full := redact("token "+secret+" basic "+base64.StdEncoding.EncodeToString([]byte(pair))+" url https://user:pw@host/path", basicAuthSecrets("x-access-token", secret))
	if strings.Contains(full, secret) || strings.Contains(full, base64.StdEncoding.EncodeToString([]byte(pair))) || strings.Contains(full, "user:pw") {
		t.Fatalf("redact = %q", full)
	}
	if !strings.Contains(full, "https://[redacted]@host/path") {
		t.Fatalf("userinfo not replaced: %q", full)
	}
}
