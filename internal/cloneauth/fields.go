package cloneauth

import (
	"strings"

	"github.com/google/uuid"
)

// ZeroOID40 and ZeroOID64 are the all-zero object IDs Git uses for "this ref does not exist" in SHA-1
// and SHA-256 repositories. They are valid expected values (create a ref) but never a source commit.
const (
	ZeroOID40 = "0000000000000000000000000000000000000000"
	ZeroOID64 = ZeroOID40 + "000000000000000000000000"
)

// validUUID reports whether s is a canonical lowercase UUID and not the nil UUID.
//
// The canonical spelling is required rather than parsed leniently because the same value is compared
// byte for byte across Cloud, the executor and the Credential Store, and becomes a file name in the
// Store: two spellings of one ID must not be two identities.
func validUUID(s string) bool {
	parsed, err := uuid.Parse(s)
	return err == nil && parsed != uuid.Nil && parsed.String() == s
}

// validOID reports whether s is a full lowercase hexadecimal SHA-1 or SHA-256 object ID. Abbreviated
// or uppercase IDs are refused because they are compared as strings with Git's own output.
func validOID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// isZeroOID reports whether s is one of the all-zero object IDs.
func isZeroOID(s string) bool { return s == ZeroOID40 || s == ZeroOID64 }

// validRefName reports whether name is a full ref name that `git check-ref-format` accepts and that
// starts with "refs/".
//
// The rules are checked here rather than by running Git so that the signer refuses an unusable ref
// before anything is dispatched; the executor still passes refs to Git only as data.
func validRefName(name string) bool {
	if !strings.HasPrefix(name, "refs/") || strings.HasSuffix(name, "/") || strings.HasSuffix(name, ".") {
		return false
	}
	if strings.Contains(name, "..") || strings.Contains(name, "@{") || strings.Contains(name, "//") {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c < 0x20 || c == 0x7f || strings.IndexByte(" ~^:?*[\\", c) >= 0 {
			return false
		}
	}
	for _, component := range strings.Split(name, "/") {
		if component == "" || strings.HasPrefix(component, ".") || strings.HasSuffix(component, ".lock") {
			return false
		}
	}
	return true
}

// validBranch reports whether branch is a branch short name Git accepts: refs/heads/<branch> is a
// valid ref, it does not start with "-" (it would read as an option) and it is not "HEAD".
func validBranch(branch string) bool {
	return branch != "" && branch != "HEAD" && !strings.HasPrefix(branch, "-") && validRefName("refs/heads/"+branch)
}

// validBranchRef reports whether ref is "refs/heads/<branch>" with a valid branch.
func validBranchRef(ref string) bool {
	branch, ok := strings.CutPrefix(ref, "refs/heads/")
	return ok && validBranch(branch)
}
