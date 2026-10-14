package cloneauth

import (
	"errors"
	"fmt"
	"strings"
)

// BranchRefPrefix is the namespace every ref this package will sign must live under.
//
// A workspace clones a branch, so the mirror purpose is limited to branches. Tags, notes and
// remote-tracking refs are outside what that purpose needs, and a ref naming something other than a
// branch would let a mirror preparation publish content no workspace clone was ever going to read.
// Widening this is a decision about what a mirror may contain, which is why the constant exists
// rather than the check being written inline at its one call site.
const BranchRefPrefix = "refs/heads/"

// ValidateRef refuses a ref the mirror purpose must not pin.
//
// The accepted value is a fully qualified ref name, not a short branch name, because the two
// spellings of one branch are not interchangeable: `main` and `refs/heads/main` can be read as
// different things by different Git commands, and a signed value a consumer has to interpret is a
// signed value a consumer can interpret differently. Requiring the qualified form means the executor
// hands the ref to `git fetch` unchanged and the remote decides nothing about what the name meant.
//
// The name rules are Git's `check-ref-format` restrictions, applied before any network access, so a
// malformed ref is refused at issuance or verification rather than becoming a confusing Git error
// after the authorization was already spent. They are the same rules the credentialed clone applies
// to its own job field, and they are deliberately a second copy rather than a shared helper: that
// field is a short branch name and this one is qualified, the two layers cannot import each other,
// and one shared function over both spellings is exactly how the spellings drift back together.
func ValidateRef(ref string) error {
	if !strings.HasPrefix(ref, BranchRefPrefix) {
		return fmt.Errorf("%w: ref %q is not under %q", ErrIncomplete, ref, BranchRefPrefix)
	}
	if err := validateRefName(strings.TrimPrefix(ref, BranchRefPrefix)); err != nil {
		return fmt.Errorf("%w: ref %q is not a valid ref name", ErrIncomplete, ref)
	}
	return nil
}

// validateRefName applies Git's ref rules to the part after the namespace prefix.
func validateRefName(name string) error {
	if name == "" {
		return errors.New("ref name is empty")
	}
	if strings.HasPrefix(name, "-") {
		return errors.New("ref name must not begin with a dash")
	}
	if strings.HasPrefix(name, "/") || strings.HasSuffix(name, "/") || strings.Contains(name, "..") {
		return errors.New("ref name is not a valid ref name")
	}
	if strings.ContainsAny(name, " \t\n\r~^:?*[\\") || strings.Contains(name, "//") || strings.Contains(name, "@{") {
		return errors.New("ref name is not a valid ref name")
	}
	if strings.HasSuffix(name, ".lock") || strings.HasSuffix(name, ".") {
		return errors.New("ref name is not a valid ref name")
	}
	return nil
}
