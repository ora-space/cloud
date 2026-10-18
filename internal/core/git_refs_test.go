package core

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestParseGitRefAcceptsOnlyContractNamespaces(t *testing.T) {
	id := uuid.NewString()
	oid := strings.Repeat("ab", 20)
	for _, c := range []struct {
		ref  string
		want GitRefName
		ok   bool
	}{
		{GitDeliveryRefPrefix + id, GitRefName{Namespace: GitDelivery, Owner: id}, true},
		{GitSnapshotRef(id, oid), GitRefName{Namespace: GitUpstreamSnapshot, Owner: id, Name: oid}, true},
		{GitUpstreamHeadPrefix(id) + "release/1.x", GitRefName{Namespace: GitUpstreamHead, Owner: id, Name: "release/1.x"}, true},
		{GitWorkspaceRefPrefix(id) + "main", GitRefName{Namespace: GitWorkspaceBranch, Owner: id, Name: "main"}, true},
		{GitDeliveryRefPrefix + strings.ToUpper(id), GitRefName{}, false},
		{GitSnapshotRef(id, strings.ToUpper(oid)), GitRefName{}, false},
		{GitSnapshotRef(id, strings.Repeat("0", 40)), GitRefName{}, false},
		{GitSnapshotRef(id, oid) + "/x", GitRefName{}, false},
		{GitWorkspaceRefPrefix(id) + "a..b", GitRefName{}, false},
		{GitWorkspaceRefPrefix(id) + "x.lock", GitRefName{}, false},
		{GitWorkspaceRefPrefix(id) + ".hidden", GitRefName{}, false},
		{GitWorkspaceRefPrefix(id) + "a b", GitRefName{}, false},
		{GitWorkspaceRefPrefix(id), GitRefName{}, false},
		{GitWorkspaceRefPrefix("x") + "main", GitRefName{}, false},
		{"refs/ora/upstreams/" + id + "/other/x", GitRefName{}, false},
		{"refs/heads/main", GitRefName{}, false},
		{"refs/ora/revisions/" + id, GitRefName{}, false},
	} {
		got, ok := ParseGitRef(c.ref)
		if ok != c.ok || ok && got != c.want {
			t.Errorf("ParseGitRef(%q) = %+v, %v; want %+v, %v", c.ref, got, ok, c.want, c.ok)
		}
		if ok && got.Namespace.Rule() == "" {
			t.Errorf("%q: namespace without a rule", c.ref)
		}
	}
}

func TestValidGitRefPrefixStaysInsideOneNamespace(t *testing.T) {
	id := uuid.NewString()
	for prefix, want := range map[string]bool{
		GitDeliveryRefPrefix:               true,
		"refs/ora/upstreams/" + id + "/":   true,
		GitSnapshotRefPrefix(id):           true,
		GitUpstreamHeadPrefix(id):          true,
		GitWorkspaceRefPrefix(id):          true,
		"refs/ora/upstreams/":              false,
		"refs/heads/workspaces/":           false,
		"refs/heads/":                      false,
		GitWorkspaceRefPrefix(id) + "a/":   false,
		"refs/ora/upstreams/" + id + "/x/": false,
		"refs/ora/upstreams/" + id:         false,
		"":                                 false,
	} {
		if got := ValidGitRefPrefix(prefix); got != want {
			t.Errorf("ValidGitRefPrefix(%q) = %v, want %v", prefix, got, want)
		}
	}
}
