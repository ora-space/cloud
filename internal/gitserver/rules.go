package gitserver

import (
	"strings"

	"github.com/wanglongan587/cloud/internal/core"
)

// snapshotLookup reports whether the repository already holds the snapshot ref naming commit.
type snapshotLookup func(ref, commit string) (bool, error)

// admit applies a grant's rule set to every update of one push. One update that no rule admits
// rejects the whole push, so nothing is partially written. The rule a ref is written under is
// fixed by its namespace; a grant naming any other rule for it is malformed and admits nothing.
func admit(grant *core.GitGrant, updates []refUpdate, snapshots snapshotLookup) bool {
	if len(updates) == 0 {
		return false
	}
	for _, u := range updates {
		if !admitOne(grant, u, updates, snapshots) {
			return false
		}
	}
	return true
}

func admitOne(grant *core.GitGrant, u refUpdate, push []refUpdate, snapshots snapshotLookup) bool {
	// No ref is ever deleted, and every namespace names commits only.
	if !core.GitObjectID(u.Old) || !core.GitObjectID(u.New) || zeroID(u.New) || u.NewType != "commit" {
		return false
	}
	name, ok := core.ParseGitRef(u.Ref)
	if !ok {
		return false
	}
	entry, ok := writable(grant, u.Ref)
	if !ok || entry.Rule != name.Namespace.Rule() {
		return false
	}
	switch entry.Rule {
	case core.GitCreateOnly:
		// A snapshot ref's name is its proof of content: it must name the commit it points at.
		return zeroID(u.Old) && (name.Namespace != core.GitUpstreamSnapshot || name.Name == u.New)
	case core.GitFastForward:
		return zeroID(u.Old) || u.FastForward
	case core.GitTrackingCAS:
		if entry.ExpectedOld == "" && !zeroID(u.Old) || entry.ExpectedOld != "" && u.Old != entry.ExpectedOld {
			return false
		}
		// A tracking ref may only point at a commit the Binding has a snapshot of: one already in
		// the repository, or one this same push creates.
		snapshot := core.GitSnapshotRef(name.Owner, u.New)
		for _, other := range push {
			if other.Ref == snapshot && other.New == u.New {
				return true
			}
		}
		exists, err := snapshots(snapshot, u.New)
		return err == nil && exists
	}
	return false
}

// writable finds the grant entry covering ref: an exact ref, or a prefix the ref lies under.
func writable(grant *core.GitGrant, ref string) (core.GitWritableRef, bool) {
	for _, w := range grant.Writable {
		if w.Ref != "" && w.Ref == ref || w.Ref == "" && w.Prefix != "" && strings.HasPrefix(ref, w.Prefix) {
			return w, true
		}
	}
	return core.GitWritableRef{}, false
}
