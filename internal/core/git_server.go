package core

import (
	"context"
	"errors"
	"net/url"
	"strings"
)

// ErrGitRefNotFound is a definitive answer from the Git Server that a ref does not exist. It is
// negative evidence, unlike a transport failure, which proves nothing.
var ErrGitRefNotFound = errors.New("git ref not found")

// GitRefResolver is Cloud's read-only view of Project repositories on the Git Server: the commit an
// exact ref names. It returns ErrGitRefNotFound for an absent ref and any other error when the
// Git Server could not answer.
type GitRefResolver interface {
	ResolveGitRef(ctx context.Context, tenantID, projectID, ref string) (string, error)
}

// GitServer is the deployment's Git Server as Cloud uses it: the repository base URL sandboxes
// reach, which Cloud writes into fixed execution inputs, and the ref query Cloud verifies refs
// through. A Store without one cannot freeze clone inputs or deliver Revisions.
type GitServer struct {
	// RepositoryBaseURL is https://host[:port][/path], without userinfo, query, fragment or a
	// trailing slash. A Project's repository is <base>/<tenantId>/<projectId>.git.
	RepositoryBaseURL string
	Refs              GitRefResolver
}

// ValidGitRepositoryBaseURL accepts the sandbox-facing repository base: https only, with a host,
// and nothing a fixed input must not carry (userinfo, query, fragment, trailing slash).
func ValidGitRepositoryBaseURL(base string) bool {
	u, err := url.Parse(base)
	return err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil && u.RawQuery == "" && !u.ForceQuery &&
		u.Fragment == "" && !strings.HasSuffix(base, "/") && !strings.ContainsAny(base, " \t\r\n\\")
}

// gitRepositoryURL is the Git Server repository of one Project. Every input that names it is built
// here, so the clone, session and delivery inputs of one Project always spell it identically.
func (g *GitServer) gitRepositoryURL(tenantID, projectID string) string {
	return g.RepositoryBaseURL + "/" + tenantID + "/" + projectID + ".git"
}

// gitRepositoryURLNames reports whether a fixed input's repository URL addresses exactly the given
// Project repository. The Git Server process checks it without knowing the configured base: the
// path's last two segments are the repository identity, and nothing else may follow them.
func gitRepositoryURLNames(repositoryURL, tenantID, projectID string) bool {
	u, err := url.Parse(repositoryURL)
	return err == nil && u.Scheme == "https" && u.RawQuery == "" && u.Fragment == "" && strings.HasSuffix(u.Path, "/"+tenantID+"/"+projectID+".git")
}
