package core

import (
	"encoding/hex"
	"strings"

	"github.com/google/uuid"
)

// GitRefRule is how one writable ref may change. Every rule forbids deletion; the rules differ
// only in which non-zero updates they admit.
type GitRefRule string

const (
	// GitCreateOnly admits only creating a ref that does not exist yet.
	GitCreateOnly GitRefRule = "create-only"
	// GitFastForward admits creating a ref or moving it to a descendant of its current commit.
	GitFastForward GitRefRule = "fast-forward"
	// GitTrackingCAS admits any move, including a non-fast-forward, whose old value equals the
	// expected old the grant records. It follows an external branch, which may be force-pushed.
	GitTrackingCAS GitRefRule = "tracking-cas"
)

// GitRefNamespace is one of the ref families a Project repository may contain. A ref outside all
// of them is never written by anyone and never answered by the ref query.
type GitRefNamespace int

const (
	// GitUpstreamSnapshot is refs/ora/upstreams/<bindingId>/snapshots/<commitOid>.
	GitUpstreamSnapshot GitRefNamespace = iota + 1
	// GitUpstreamHead is refs/ora/upstreams/<bindingId>/heads/<branch>.
	GitUpstreamHead
	// GitWorkspaceBranch is refs/heads/workspaces/<workspaceId>/<branch>.
	GitWorkspaceBranch
	// GitDelivery is refs/ora/deliveries/<deliveryWorkId>.
	GitDelivery
)

// Rule is the only rule a namespace's refs are ever written under. A grant naming another rule
// for the namespace is malformed, and the Git Server refuses the write.
func (n GitRefNamespace) Rule() GitRefRule {
	switch n {
	case GitUpstreamSnapshot, GitDelivery:
		return GitCreateOnly
	case GitUpstreamHead:
		return GitTrackingCAS
	case GitWorkspaceBranch:
		return GitFastForward
	}
	return ""
}

// Ref prefixes of the Project repository's namespaces.
const (
	// GitDeliveryRefPrefix names the remote refs delivery attempts create. Each delivery work item
	// owns exactly one ref under it, so a retry never collides with an earlier attempt.
	GitDeliveryRefPrefix  = "refs/ora/deliveries/"
	gitUpstreamRefPrefix  = "refs/ora/upstreams/"
	gitWorkspaceRefPrefix = "refs/heads/workspaces/"
)

// GitSnapshotRefPrefix is the prefix of one Binding's snapshot refs.
func GitSnapshotRefPrefix(bindingID string) string {
	return gitUpstreamRefPrefix + bindingID + "/snapshots/"
}

// GitSnapshotRef is the immutable ref naming one synchronized upstream commit. Naming it by the
// commit makes a replayed sync idempotent and keeps one ref per commit.
func GitSnapshotRef(bindingID, commit string) string { return GitSnapshotRefPrefix(bindingID) + commit }

// GitUpstreamHeadPrefix is the prefix of one Binding's tracking refs.
func GitUpstreamHeadPrefix(bindingID string) string {
	return gitUpstreamRefPrefix + bindingID + "/heads/"
}

// GitWorkspaceRefPrefix is the prefix of one Workspace's collaboration branches.
func GitWorkspaceRefPrefix(workspaceID string) string {
	return gitWorkspaceRefPrefix + workspaceID + "/"
}

// GitRefName is a parsed ref of a Project repository namespace.
type GitRefName struct {
	Namespace GitRefNamespace
	// Owner is the Binding, Workspace or delivery work item the ref belongs to.
	Owner string
	// Name is the snapshot commit or the branch short name; empty for a delivery ref.
	Name string
}

// ParseGitRef classifies a full ref name. It accepts only canonical owner IDs, lowercase object
// names and branch names Git itself would accept, so one logical ref has one spelling.
func ParseGitRef(ref string) (GitRefName, bool) {
	switch {
	case strings.HasPrefix(ref, GitDeliveryRefPrefix):
		owner := strings.TrimPrefix(ref, GitDeliveryRefPrefix)
		return GitRefName{Namespace: GitDelivery, Owner: owner}, canonicalUUID(owner)
	case strings.HasPrefix(ref, gitWorkspaceRefPrefix):
		owner, branch, ok := strings.Cut(strings.TrimPrefix(ref, gitWorkspaceRefPrefix), "/")
		return GitRefName{Namespace: GitWorkspaceBranch, Owner: owner, Name: branch}, ok && canonicalUUID(owner) && validBranchName(branch)
	case strings.HasPrefix(ref, gitUpstreamRefPrefix):
		owner, rest, ok := strings.Cut(strings.TrimPrefix(ref, gitUpstreamRefPrefix), "/")
		if !ok || !canonicalUUID(owner) {
			return GitRefName{}, false
		}
		if commit, ok := strings.CutPrefix(rest, "snapshots/"); ok {
			return GitRefName{Namespace: GitUpstreamSnapshot, Owner: owner, Name: commit}, GitObjectID(commit) && !gitZeroID(commit)
		}
		if branch, ok := strings.CutPrefix(rest, "heads/"); ok {
			return GitRefName{Namespace: GitUpstreamHead, Owner: owner, Name: branch}, validBranchName(branch)
		}
	}
	return GitRefName{}, false
}

// ValidGitRefPrefix reports whether prefix lists refs inside exactly one namespace instance: the
// deliveries, one Binding (all, snapshots or heads) or one Workspace. It must end with "/".
func ValidGitRefPrefix(prefix string) bool {
	if prefix == GitDeliveryRefPrefix {
		return true
	}
	if rest, ok := strings.CutPrefix(prefix, gitWorkspaceRefPrefix); ok {
		owner, tail, found := strings.Cut(rest, "/")
		return found && tail == "" && canonicalUUID(owner)
	}
	if rest, ok := strings.CutPrefix(prefix, gitUpstreamRefPrefix); ok {
		owner, tail, found := strings.Cut(rest, "/")
		return found && canonicalUUID(owner) && (tail == "" || tail == "snapshots/" || tail == "heads/")
	}
	return false
}

// GitObjectID accepts lowercase SHA-1 and SHA-256 hexadecimal object names.
func GitObjectID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil && strings.ToLower(s) == s
}

func gitZeroID(s string) bool { return strings.Trim(s, "0") == "" }

func canonicalUUID(s string) bool {
	id, err := uuid.Parse(s)
	return err == nil && id.String() == s
}

// validBranchName is the subset of git check-ref-format that a branch short name must satisfy,
// kept strict enough that no name needs quoting in a URL, a query or a shell argument.
func validBranchName(name string) bool {
	if name == "" || len(name) > 200 || strings.HasSuffix(name, "/") || strings.HasSuffix(name, ".") || strings.Contains(name, "..") || strings.Contains(name, "@{") || name == "@" {
		return false
	}
	for _, component := range strings.Split(name, "/") {
		if component == "" || strings.HasPrefix(component, ".") || strings.HasSuffix(component, ".lock") {
			return false
		}
	}
	for i := range len(name) {
		c := name[i]
		if c <= ' ' || c > '~' || strings.IndexByte(`~^:?*[\`, c) >= 0 {
			return false
		}
	}
	return true
}
