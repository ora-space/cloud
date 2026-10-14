package cloneexec

import (
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/wanglongan587/cloud/internal/cloneauth"
)

// pushLocalRef is the local ref the authorized commit is published from.
//
// The write is expressed as one refspec between two named refs rather than as a bare object name
// against a ref, because a refspec is what `git push` reports back and what `--force-with-lease`
// describes: the source ref is created at exactly the commit the authorization names, one line above
// the push, so the value the remote receives and the value the signature covers are the same commit by
// construction rather than by a reader comparing two spellings.
const pushLocalRef = "refs/push/target"

// pushFetchRefspec materializes every branch head of the authorized upstream into the scratch
// repository.
//
// It is a wildcard, and that is deliberately not the wildcard the authorization rules refuse. Those
// rules bound the *write*: exactly one ref may be updated, and a pattern cannot name it. This refspec
// bounds a *read* of the one repository the authorization already named, and the objects it brings in
// are the material the local determinations need — the authorized new commit has to exist somewhere in
// the upstream before this process may publish it, and the branch that carries it is not necessarily
// the branch being updated. The write target is never derived from this refspec: it is the signed ref
// and nothing else.
//
// Tags are excluded by `--no-tags` and by the refspec's own prefix, so a tag object cannot arrive and
// cannot be published; the fetch is also told not to write `FETCH_HEAD`, so no later Git command in
// this repository can be influenced by a file this one left behind.
const pushFetchRefspec = "+refs/heads/*:refs/heads/*"

// validatePushTarget refuses an address this path will not dial.
//
// The address is the one field of a push job that is not compared against the signed authorization —
// the authorization binds the canonical *identity* of the repository, which is a different string — so
// it is checked on its own terms: it must be an https URL this process can hand to Git, with no
// whitespace that could split the argument. The same rule the mirror path applies to its target, for
// the same reason.
func validatePushTarget(address string) error {
	if strings.ContainsAny(address, " \t\n\r") {
		return errors.New("the push target must not contain whitespace")
	}
	parsed, err := url.Parse(address)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return errors.New("the push target is not an https repository")
	}
	return nil
}

// pushFetchArguments is the complete Git command that materializes the authorized upstream.
//
// The constraints are `gitConstraintArguments`, shared with the clone and mirror paths so the three
// cannot drift; what this function adds is only the fetch's own shape. `--` separates the options from
// the address, so an address beginning with `-` cannot be read as another option.
func pushFetchArguments(helper, address string, transport gitTransport) ([]string, error) {
	if err := validatePushTarget(address); err != nil {
		return nil, err
	}
	arguments, err := gitConstraintArguments(helper, address, transport)
	if err != nil {
		return nil, err
	}
	return append(arguments,
		"fetch", "--quiet", "--no-tags", "--no-write-fetch-head", "--prune",
		"--no-recurse-submodules", "--", address, pushFetchRefspec,
	), nil
}

// pushUpdateArguments is the complete Git command that performs the authorized write.
//
// This is the only command in this package that changes anything outside the executor, and every
// restriction the push decision fixes is on this one line:
//
//   - The refspec is `<local ref>:<signed ref>` — one source, one target, both fully qualified. There is
//     no pattern, no multi-ref list, no `+` prefix and no empty source, so a deletion and a
//     "push everything" are not expressible. The rules that refuse those spellings at admission are
//     repeated by `cloneauth.ValidateRef` on the value that arrives here, so the write cannot be widened
//     by anything between Cloud and this call.
//   - `--force-with-lease=<ref>:<old>` is the compare-and-swap. The update is sent only if the remote's
//     ref holds exactly the commit the authorization named; a remote that has moved is refused by the
//     remote itself rather than overwritten. It is stated with the expected value rather than relying on
//     the local ref, because a lease that is checked against this process's own copy would be checked
//     against a value this process wrote — that is, against nothing.
//   - `--no-verify` and `-c push.followTags=false` are defense in depth on a write path: hooks are
//     already disabled by `core.hooksPath=/dev/null`, and an explicit refspec cannot carry a tag, but a
//     write is the one operation where a second, unreviewed program or ref must not be able to ride
//     along.
//
// `--porcelain` is what makes the outcome readable: the plain output is prose meant for a person and
// would have to be parsed by matching English.
func pushUpdateArguments(helper, address, ref, oldCommit, source string, transport gitTransport) ([]string, error) {
	if err := validatePushTarget(address); err != nil {
		return nil, err
	}
	if err := cloneauth.ValidateRef(ref); err != nil {
		return nil, err
	}
	if err := cloneauth.ValidateCommit(oldCommit); err != nil {
		return nil, fmt.Errorf("old commit: %w", err)
	}
	arguments, err := gitConstraintArguments(helper, address, transport)
	if err != nil {
		return nil, err
	}
	arguments = append(arguments, "-c", "push.followTags=false")
	return append(arguments,
		"push", "--porcelain", "--no-verify",
		"--force-with-lease="+ref+":"+oldCommit,
		"--", address, source+":"+ref,
	), nil
}

// pushRemoteRefArguments is the complete Git command that reads one ref of the authorized upstream
// without changing anything.
//
// It is the reconciliation half of unit 4: after an outcome that cannot be established from the write
// attempt alone, this is how the executor answers "what does the remote hold now" — by asking the
// remote, under the same credential the attempt already spent, with no new authorization and no second
// write. `--refs` drops the peeled `^{}` lines a tag would add; an annotated tag cannot be the target
// of a push refspec here, but a repository's tags are still listed by a plain `ls-remote` and reading
// one would only be a second value to explain.
func pushRemoteRefArguments(helper, address, ref string, transport gitTransport) ([]string, error) {
	if err := validatePushTarget(address); err != nil {
		return nil, err
	}
	if err := cloneauth.ValidateRef(ref); err != nil {
		return nil, err
	}
	arguments, err := gitConstraintArguments(helper, address, transport)
	if err != nil {
		return nil, err
	}
	return append(arguments, "ls-remote", "--refs", "--", address, ref), nil
}

// pushReport is what one `git push --porcelain` run said about the one ref it was asked to update.
//
// The flag is the whole answer: an update that was sent is the evidence that the compare-and-swap held
// and that this attempt is what moved the ref, and a rejection is the evidence that nothing was sent.
// Everything else Git could print is prose.
type pushReport struct {
	// Flag is the porcelain status letter: ' ' for a fast-forward, '+' for a forced update, '*' for a
	// new ref, '=' for "the remote already holds this", '!' for a rejection, '-' for a deletion.
	Flag byte
	// Summary is the trailing human-readable field, kept for the executor's log only.
	Summary string
}

// parsePushReport reads the porcelain line for one ref out of a push's standard output.
//
// The line is `"<flag>\t<from>:<to>\t<summary>"`, with a fast-forward spelled as a single space. The
// target half of the refspec is compared against the authorized ref, so a report about any other ref —
// which this path cannot ask for, but which a Git that behaved differently could print — is refused
// rather than read as this write's outcome. A second matching line, or no matching line at all, is
// likewise not a report: the answer is then "this attempt's result could not be read", which the caller
// turns into the unknown state rather than into a guess.
func parsePushReport(stdout, ref string) (pushReport, bool) {
	var found pushReport
	matches := 0
	for _, line := range strings.Split(stdout, "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) < 2 {
			// "To <address>" and "Done" carry no tab-separated refspec and are not reports.
			continue
		}
		flag := byte(' ')
		if fields[0] != "" {
			flag = fields[0][0]
		}
		from, to, present := strings.Cut(fields[1], ":")
		if !present || from == "" || to != ref {
			continue
		}
		matches++
		if matches > 1 {
			return pushReport{}, false
		}
		found = pushReport{Flag: flag}
		if len(fields) > 2 {
			found.Summary = strings.TrimSpace(fields[2])
		}
	}
	return found, matches == 1
}

// parseRemoteRef reads the commit one `git ls-remote --refs` run reported for one ref.
//
// An empty answer is a fact rather than an error: `git ls-remote` prints nothing and exits zero for a
// ref the remote does not have (measured against a local upstream), so "the ref is gone" has to be
// readable as its own answer — a third value distinct from both the old and the new commit, which is
// what makes a vanished ref a conflict rather than a failure. `ok` therefore means "the command
// produced one usable answer, which may be `no such ref`", and false means it produced something this
// function cannot read, which the caller turns into the unknown state.
func parseRemoteRef(stdout, ref string) (commit string, ok bool) {
	lines := 0
	for _, raw := range strings.Split(stdout, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		lines++
		fields := strings.Split(line, "\t")
		if len(fields) != 2 || fields[1] != ref || cloneauth.ValidateCommit(fields[0]) != nil {
			return "", false
		}
		commit = fields[0]
	}
	if lines > 1 {
		return "", false
	}
	return commit, true
}
