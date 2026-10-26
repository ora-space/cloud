package repoexec

import (
	"strings"
)

// pushLine is one ref's report in `git push --porcelain` output: "<flag>\t<from>:<to>\t<summary>".
type pushLine struct {
	flag    byte
	summary string
}

// parsePorcelain returns the report line for each destination ref. A destination reported twice is
// not readable as one outcome and is dropped, so the caller treats it as unreported.
func parsePorcelain(stdout string) map[string]pushLine {
	lines := map[string]pushLine{}
	seen := map[string]int{}
	for _, raw := range strings.Split(stdout, "\n") {
		fields := strings.Split(raw, "\t")
		if len(fields) < 2 {
			// "To <url>" and "Done" carry no refspec.
			continue
		}
		flag := byte(' ')
		if fields[0] != "" {
			flag = fields[0][0]
		}
		_, to, ok := strings.Cut(fields[1], ":")
		if !ok || !strings.HasPrefix(to, "refs/") {
			continue
		}
		seen[to]++
		var summary string
		if len(fields) > 2 {
			summary = strings.TrimSpace(fields[2])
		}
		lines[to] = pushLine{flag: flag, summary: summary}
	}
	for ref, n := range seen {
		if n > 1 {
			delete(lines, ref)
		}
	}
	return lines
}

// pushOutcome is what a push report says about one ref.
type pushOutcome int

const (
	// outcomeUnknown: no readable report (missing, cut, connection lost, remote failed to report).
	outcomeUnknown pushOutcome = iota
	// outcomeOK: the update was applied or was already in place (' ', '+', '*', '=').
	outcomeOK
	// outcomeStale: the lease did not hold; nothing was sent for this ref.
	outcomeStale
	// outcomeRejected: the remote explicitly refused the update (hook, protection, permission).
	outcomeRejected
	// outcomeAtomicFailed: refused only because another ref of the same atomic push failed.
	outcomeAtomicFailed
)

// outcome reads one porcelain line. A '!' is an explicit refusal only when Git names it as one; a
// helper or connection failure that Git renders as '!' ("remote failure", "failed to report status")
// says nothing about whether the update happened.
func (l pushLine) outcome() pushOutcome {
	switch l.flag {
	case ' ', '+', '*', '=':
		return outcomeOK
	case '!':
	default:
		return outcomeUnknown
	}
	s := l.summary
	switch {
	case strings.Contains(s, "stale info"):
		return outcomeStale
	case strings.Contains(s, "atomic push failed"):
		return outcomeAtomicFailed
	case strings.Contains(s, "remote failure"), strings.Contains(s, "failed to report status"), strings.Contains(s, "hung up"):
		return outcomeUnknown
	case strings.HasPrefix(s, "[remote rejected]"), strings.HasPrefix(s, "[rejected]"):
		return outcomeRejected
	}
	return outcomeUnknown
}

// parseLsRemote reads `git ls-remote --refs` output into ref → OID. ok is false when any line is not
// a "<oid>\t<ref>" pair, in which case nothing it printed is used.
func parseLsRemote(stdout string) (refs map[string]string, ok bool) {
	refs = map[string]string{}
	for _, raw := range strings.Split(stdout, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		oid, ref, found := strings.Cut(line, "\t")
		if !found || !validOID(oid) || !strings.HasPrefix(ref, "refs/") {
			return nil, false
		}
		if _, dup := refs[ref]; dup {
			return nil, false
		}
		refs[ref] = oid
	}
	return refs, true
}

func validOID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for i := range len(s) {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func zeroOID(s string) bool { return s != "" && strings.Trim(s, "0") == "" }

// sameOID compares an observed value with an expected one, reading an absent ref ("") as the zero
// OID, which is how authorizations spell "must not exist".
func sameOID(observed, expected string) bool {
	if observed == "" {
		return zeroOID(expected)
	}
	return observed == expected
}

// externalFailure maps a failed external fetch or ls-remote to a stable code from Git's stderr. Git
// runs with LC_ALL=C, so the messages are stable English. anonymous says no credential was staged:
// an authentication prompt then means the repository needs one; with a credential it means the
// credential was refused.
func externalFailure(stderr string, anonymous bool) string {
	s := stderr
	authPrompt := strings.Contains(s, "could not read Username") || strings.Contains(s, "could not read Password") ||
		strings.Contains(s, "Authentication failed") || strings.Contains(s, "returned error: 401") ||
		strings.Contains(s, "returned error: 403")
	notFound := strings.Contains(s, "not found") && strings.Contains(s, "repository") || strings.Contains(s, "returned error: 404")
	switch {
	case strings.Contains(s, "couldn't find remote ref"):
		return CodeUpstreamBranchMissing
	case authPrompt && anonymous, notFound && anonymous:
		// Forges answer an anonymous request for a private repository with 401 or 404.
		return CodeRequiresCredential
	case authPrompt:
		return CodeCredentialRejected
	}
	return CodeFetchFailed
}

// pushNeverSent reports a push that failed during ref discovery (GET info/refs), before any update
// command could have been sent: Git says "unable to access" for that phase and "RPC failed" for the
// POST that carries the update.
func pushNeverSent(stderr string) bool {
	if strings.Contains(stderr, "RPC failed") || strings.Contains(stderr, "send-pack") {
		return false
	}
	return strings.Contains(stderr, "Authentication failed") || strings.Contains(stderr, "unable to access") ||
		strings.Contains(stderr, "could not read Username") || strings.Contains(stderr, "could not read Password")
}

// gitServerFailure maps a failed Git Server request to a code: an explicit HTTP refusal is the Git
// Server deciding by Operation state (ended, lease expired, wrong scope); anything else is the Git
// Server being unreachable or unhealthy.
func gitServerFailure(stderr string) string {
	for _, status := range []string{"returned error: 400", "returned error: 401", "returned error: 403", "returned error: 404"} {
		if strings.Contains(stderr, status) {
			return CodeGitServerRejected
		}
	}
	return CodeGitServerUnavailable
}

// classifyPublish applies the ADR D8 table to a publish push report and the post-push verification.
// post is the observed target value ("" = absent) and postOK whether it could be read.
func classifyPublish(push pushOutcome, post string, postOK bool, expectedOld, source string) (c Classification, confirmed bool) {
	switch push {
	case outcomeOK:
		return Applied, false
	case outcomeStale:
		return Conflict, false
	case outcomeRejected:
		switch {
		case !postOK:
			// The platform refused, but without verification "unchanged" is not established.
			return Uncertain, false
		case post == source:
			return Applied, true
		case sameOID(post, expectedOld):
			return Rejected, false
		}
		return Conflict, false
	}
	if postOK && post == source {
		return Applied, true
	}
	// Includes "still expectedOld": the push may still be queued at the platform.
	return Uncertain, false
}
