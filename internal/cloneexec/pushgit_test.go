package cloneexec

import (
	"strings"
	"testing"
)

// This file covers the two halves of a push that are pure syntax: the Git command lines this path will
// and will not build, and the reading of Git's own report of what the remote answered.
//
// They are tested here rather than only through the acceptance run because they are where the write's
// limits are actually expressed. The acceptance run proves the limits hold against a real remote; these
// cases prove that the *only* thing this package can build is a one-ref compare-and-swap, so a limit
// cannot be lost by a change that the acceptance run's happy path would not notice.

const (
	pushTestHelper   = "/opt/ora/git-credential-ora"
	pushTestAddress  = "https://git.example.invalid/group/private"
	pushTestRef      = "refs/heads/main"
	pushTestOld      = "0a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b"
	pushTestNew      = "9f8e7d6c5b4a39281706f5e4d3c2b1a09f8e7d6c"
	pushTestCA       = "/etc/ora/upstream-ca.pem"
	pushTestNonCanon = "git@github.com:group/private.git"
)

// TestTheWriteCommandIsOneRefOneCompareAndSwap is the shape of the only command in this package that
// changes anything outside the executor.
//
// Every one of these assertions is a restriction the push decision fixes, and each is asserted on the
// built argument list rather than on the caller that builds it, because the list is what Git receives:
// a deletion is the absence of a source, a wildcard is a `*` in the refspec, a tag write is a target
// outside `refs/heads/`, and a multi-ref write is a second refspec or a space inside one.
func TestTheWriteCommandIsOneRefOneCompareAndSwap(t *testing.T) {
	arguments, err := pushUpdateArguments(pushTestHelper, pushTestAddress, pushTestRef, pushTestOld, pushLocalRef, gitTransport{sslCAInfo: pushTestCA})
	if err != nil {
		t.Fatalf("pushUpdateArguments: %v", err)
	}
	joined := strings.Join(arguments, " ")

	if !strings.Contains(joined, "push --porcelain --no-verify") {
		t.Fatalf("the write is not the push this path documents: %s", joined)
	}
	// The write is expressed as exactly one refspec, source and target both fully qualified. One colon
	// and no `+` prefix is what makes a force, a delete and a multi-ref write unrepresentable here.
	refspec := pushLocalRef + ":" + pushTestRef
	if !strings.HasSuffix(joined, "-- "+pushTestAddress+" "+refspec) {
		t.Fatalf("the write does not end at one refspec against the address: %s", joined)
	}
	if strings.Count(joined, pushLocalRef+":") != 1 || strings.Contains(joined, "+"+pushLocalRef) {
		t.Fatalf("the write names more than one source, or forces one: %s", joined)
	}
	if strings.ContainsAny(refspec, "*?[ ") {
		t.Fatalf("the refspec is a pattern or a list: %s", refspec)
	}
	// The compare-and-swap is stated with the expected value rather than left to the local ref: a lease
	// checked against this process's own copy would be checked against a value this process wrote.
	if !strings.Contains(joined, "--force-with-lease="+pushTestRef+":"+pushTestOld) {
		t.Fatalf("the write carries no compare-and-swap against the authorized old commit: %s", joined)
	}
	// A tag must not ride along, and hooks must be off even though the closed config already sets
	// `core.hooksPath=/dev/null`. Both are second locks on the one command that writes.
	if !strings.Contains(joined, "-c push.followTags=false") || !strings.Contains(joined, "core.hooksPath=/dev/null") {
		t.Fatalf("the write can carry a tag or run a hook: %s", joined)
	}
	// `--` keeps an address beginning with `-` from being read as another option.
	if !strings.Contains(joined, "-- "+pushTestAddress) {
		t.Fatalf("the address is not separated from the options: %s", joined)
	}
	// The closed protocol set is the same one the clone and mirror paths use, which is the point of
	// sharing it: a push must not be able to reach a transport the other two cannot.
	if !strings.Contains(joined, "-c protocol.allow=never") {
		t.Fatalf("the write does not run under the closed protocol set: %s", joined)
	}
}

// TestTheWriteCommandRefusesAnAddressOrRefItWillNotDial is the boundary half: this path either builds a
// command for an https address and one fully qualified branch ref, or it builds nothing at all.
//
// Refusing to build is the safe direction and is not the same as building something Git would reject:
// a value that reached the argument list would be a value this process handed to Git, and the cases
// below are the spellings that would widen the write if they did.
func TestTheWriteCommandRefusesAnAddressOrRefItWillNotDial(t *testing.T) {
	cases := []struct {
		name    string
		address string
		ref     string
		old     string
	}{
		{name: "a scp-like address", address: pushTestNonCanon, ref: pushTestRef, old: pushTestOld},
		{name: "an ssh address", address: "ssh://git@git.example.invalid/group/private", ref: pushTestRef, old: pushTestOld},
		{name: "an address with whitespace", address: pushTestAddress + " --upload-pack=sh", ref: pushTestRef, old: pushTestOld},
		{name: "a short branch name", address: pushTestAddress, ref: "main", old: pushTestOld},
		{name: "a tag", address: pushTestAddress, ref: "refs/tags/v1.0.0", old: pushTestOld},
		{name: "a wildcard", address: pushTestAddress, ref: "refs/heads/*", old: pushTestOld},
		{name: "a force prefix", address: pushTestAddress, ref: "+refs/heads/main", old: pushTestOld},
		{name: "two refs", address: pushTestAddress, ref: "refs/heads/main refs/heads/other", old: pushTestOld},
		{name: "a traversal spelling", address: pushTestAddress, ref: "refs/heads/../../main", old: pushTestOld},
		{name: "the null object id as the lease", address: pushTestAddress, ref: pushTestRef, old: "0000000000000000000000000000000000000000"},
		{name: "an abbreviated lease", address: pushTestAddress, ref: pushTestRef, old: pushTestOld[:12]},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if arguments, err := pushUpdateArguments(pushTestHelper, testCase.address, testCase.ref, testCase.old, pushLocalRef, gitTransport{sslCAInfo: pushTestCA}); err == nil {
				t.Fatalf("this path built a write it documents as unrepresentable: %v", arguments)
			}
		})
	}
}

// TestTheFetchAndReadCommandsStayReads keeps the two non-writing commands inside their own limits.
//
// The wildcard on the fetch is the one place a pattern appears in this package, and it is a pattern over
// a *read* of the one repository the authorization already named — the write target is never derived
// from it. The read-back is a single ref under `--refs`, and neither command may be given a ref it could
// interpret as a pattern.
func TestTheFetchAndReadCommandsStayReads(t *testing.T) {
	fetch, err := pushFetchArguments(pushTestHelper, pushTestAddress, gitTransport{sslCAInfo: pushTestCA})
	if err != nil {
		t.Fatalf("pushFetchArguments: %v", err)
	}
	joinedFetch := strings.Join(fetch, " ")
	if !strings.Contains(joinedFetch, "fetch --quiet --no-tags --no-write-fetch-head --prune --no-recurse-submodules") {
		t.Fatalf("the fetch is not the read this path documents: %s", joinedFetch)
	}
	if !strings.HasSuffix(joinedFetch, "-- "+pushTestAddress+" "+pushFetchRefspec) {
		t.Fatalf("the fetch does not end at the address and the branch refspec: %s", joinedFetch)
	}
	// The refspec reaches only branch heads, so a tag object cannot arrive and cannot then be published.
	if !strings.HasPrefix(pushFetchRefspec, "+refs/heads/") || strings.Contains(pushFetchRefspec, "tags") {
		t.Fatalf("the fetch refspec reaches beyond branch heads: %s", pushFetchRefspec)
	}

	read, err := pushRemoteRefArguments(pushTestHelper, pushTestAddress, pushTestRef, gitTransport{sslCAInfo: pushTestCA})
	if err != nil {
		t.Fatalf("pushRemoteRefArguments: %v", err)
	}
	joinedRead := strings.Join(read, " ")
	if !strings.HasSuffix(joinedRead, "ls-remote --refs -- "+pushTestAddress+" "+pushTestRef) {
		t.Fatalf("the read-back is not one ref of the address: %s", joinedRead)
	}
	if _, err := pushRemoteRefArguments(pushTestHelper, pushTestAddress, "refs/heads/*", gitTransport{sslCAInfo: pushTestCA}); err == nil {
		t.Fatal("the read-back accepted a pattern ref")
	}
}

// TestTheReportReaderReadsExactlyOneRef keeps the outcome classification from being decided by prose.
//
// `git push --porcelain` prints `To <address>`, one line per refspec, and `Done`. Only the line whose
// target is the authorized ref is a report, and there must be exactly one: two matching lines would mean
// this attempt touched something the authorization does not name, and no matching line means the attempt
// produced nothing this package can read. Both of those are "not established", never a guess.
func TestTheReportReaderReadsExactlyOneRef(t *testing.T) {
	const other = "refs/heads/other"
	cases := []struct {
		name    string
		stdout  string
		want    byte
		summary string
		ok      bool
	}{
		{
			name:   "a fast-forward",
			stdout: "To https://git.example.invalid/group/private\n \trefs/push/target:refs/heads/main\t0a1b2c3..9f8e7d6\nDone\n",
			want:   ' ', ok: true, summary: "0a1b2c3..9f8e7d6",
		},
		{
			name:   "an up-to-date ref",
			stdout: "To https://git.example.invalid/group/private\n=\trefs/push/target:refs/heads/main\t[up to date]\nDone\n",
			want:   '=', ok: true, summary: "[up to date]",
		},
		{
			name:   "a stale lease",
			stdout: "To https://git.example.invalid/group/private\n!\trefs/push/target:refs/heads/main\t[rejected] (stale info)\nDone\n",
			want:   '!', ok: true, summary: "[rejected] (stale info)",
		},
		{
			// The force prefix is a status this path never asks for, and the reader reports it rather
			// than hiding it: the classification decides what to do with it, and it decides "unknown".
			name:   "a forced update",
			stdout: "To https://git.example.invalid/group/private\n+\trefs/push/target:refs/heads/main\tforced update\nDone\n",
			want:   '+', ok: true, summary: "forced update",
		},
		{name: "another ref", stdout: "To x\n \trefs/push/target:" + other + "\t0a1b2c3..9f8e7d6\nDone\n"},
		{name: "no refspec line at all", stdout: "To x\nDone\n"},
		{name: "empty output", stdout: ""},
		{
			name:   "two matching lines",
			stdout: "To x\n \trefs/push/target:refs/heads/main\ta\n \trefs/push/target:refs/heads/main\tb\nDone\n",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			report, ok := parsePushReport(testCase.stdout, pushTestRef)
			if ok != testCase.ok {
				t.Fatalf("report read as (%+v, %v), want ok=%v", report, ok, testCase.ok)
			}
			if !testCase.ok {
				return
			}
			if report.Flag != testCase.want || report.Summary != testCase.summary {
				t.Fatalf("report = (%q, %q), want (%q, %q)", report.Flag, report.Summary, testCase.want, testCase.summary)
			}
		})
	}
}

// TestTheRemoteReaderDistinguishesNoSuchRefFromAnUnreadableRead is the reconciliation half.
//
// `git ls-remote` prints nothing and exits zero for a ref the remote does not have, so "the ref is gone"
// is an answer and not a failure — and it has to stay distinguishable from a read this package cannot
// parse, because the first is a third value that makes the outcome a conflict while the second leaves
// the outcome exactly where it was. A reader that merged them would turn an unreadable answer into a
// confident "somebody else's commit is there".
func TestTheRemoteReaderDistinguishesNoSuchRefFromAnUnreadableRead(t *testing.T) {
	cases := []struct {
		name   string
		stdout string
		want   string
		ok     bool
	}{
		{name: "the ref is there", stdout: pushTestNew + "\trefs/heads/main\n", want: pushTestNew, ok: true},
		{name: "the ref is gone", stdout: "", want: "", ok: true},
		{name: "blank lines only", stdout: "\n\n", want: "", ok: true},
		{name: "a different ref", stdout: pushTestNew + "\trefs/heads/other\n"},
		{name: "an abbreviated id", stdout: pushTestNew[:12] + "\trefs/heads/main\n"},
		{name: "an upper-case id", stdout: strings.ToUpper(pushTestNew) + "\trefs/heads/main\n"},
		{name: "no tab", stdout: pushTestNew + " refs/heads/main\n"},
		{name: "two lines", stdout: pushTestNew + "\trefs/heads/main\n" + pushTestOld + "\trefs/heads/main\n"},
		{name: "a peeled tag line", stdout: pushTestOld + "\trefs/heads/main^{}\n"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			commit, ok := parseRemoteRef(testCase.stdout, pushTestRef)
			if ok != testCase.ok || commit != testCase.want {
				t.Fatalf("read as (%q, %v), want (%q, %v)", commit, ok, testCase.want, testCase.ok)
			}
		})
	}
}
