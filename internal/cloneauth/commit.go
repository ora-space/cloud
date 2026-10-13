package cloneauth

import "fmt"

// commitIDLength is the length of a SHA-1 Git object id, which is the object format every repository
// this deployment reads or writes uses.
//
// The length is fixed rather than inferred from the repository the commit came from, because the two
// sides of a write are compared as bytes: the executor reads the commit back out of its own
// checkout (`internal/cloneexec.headCommit`, which applies the same rule) and the remote reports the
// ref's value in the same format, so a value of another length is not a commit this path produced and
// cannot be one it wrote. A repository whose object format is SHA-256 would be a new decision, not a
// second accepted length here — accepting both would make "these two refs name the same commit"
// depend on a length comparison the caller could satisfy with a spelling neither side produced.
const commitIDLength = 40

// ValidateCommit refuses a value that is not a full Git commit id.
//
// The rules are Git's own spelling: hexadecimal, lowercase, and the whole id rather than an
// abbreviation. Lowercase is required rather than normalized, for the same reason `ValidateRef`
// requires the qualified form of a ref — an abbreviation and a differently cased spelling both name
// the same object while being different bytes, and a signed value a consumer has to interpret is a
// signed value two consumers can interpret differently. Requiring the full lowercase id means the old
// and new commits an authorization binds can be compared against what the remote and the local
// repository report with `==` and nothing else.
//
// An empty value is refused here rather than being left to the presence rules, so every caller gets
// one answer for "this is not a commit id" instead of an empty string that reads as absent in one
// place and as a malformed id in another.
func ValidateCommit(commit string) error {
	if len(commit) != commitIDLength {
		return fmt.Errorf("%w: %q is not a %d-character commit id", ErrIncomplete, commit, commitIDLength)
	}
	for i := 0; i < len(commit); i++ {
		c := commit[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return fmt.Errorf("%w: %q is not a lowercase hexadecimal commit id", ErrIncomplete, commit)
		}
	}
	return nil
}
