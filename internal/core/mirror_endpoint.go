package core

import (
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/wanglongan587/cloud/internal/gitserver"
	"github.com/wanglongan587/cloud/internal/mirror"
)

// MirrorEndpoint is the address a sandbox reads one published mirror at.
//
// It exists because the mirror's address is deliberately not stored anywhere. Cloud derives it from
// the mirror's key — the Project, the Project's canonical upstream repository, and the commit the
// mirror pins — so a clone input can never be a stale column that outlived the facts it was built
// from, and the address Cloud writes is the same one the Git Server recomputes when it authorizes the
// read (`git_read.go`, `mirrorKey`).
//
// The base URL is the deployment's and it names the *sandbox-facing* listener, not the one the
// deployment's own tooling queries: the read authorization resolves the caller's container from the
// connection's peer address, so an address that reached the server through a proxy or a load
// balancer would make every caller look like the proxy and turn every read into the same refusal.
//
// It is installed once at startup and never per request, which is why it is a value with no
// dependencies to substitute rather than an interface: there is nothing here to fake.
type MirrorEndpoint struct{ base *url.URL }

// SetMirrorEndpoint installs the internal Git Server's sandbox-facing address, once, at startup.
//
// A deployment that installs none leaves a credentialed Workspace behind the `mirror_not_ready` gate
// on the `node` -> `clone` transition, even once its mirror is published. That is the fail-closed
// direction and not a fallback to the upstream: the Node holds no credential for the upstream, and
// the decision makes the published mirror the one channel a Workspace's content arrives through, so
// "the mirror is ready but nobody named where to read it" is a deployment that must not clone rather
// than a deployment that clones from somewhere else.
func (s *Store) SetMirrorEndpoint(endpoint *MirrorEndpoint) { s.mirrorEndpoint = endpoint }

// NewMirrorEndpoint validates the base URL a sandbox reaches the Git Server at.
//
// It refuses anything but an `https` origin with no user info, path, query or fragment, so the value
// Cloud writes into a durable clone input cannot be a plaintext address, cannot smuggle a credential,
// and cannot name a path the server does not serve. A wrong value here is refused at startup rather
// than written into an Operation that then fails to clone.
func NewMirrorEndpoint(baseURL string) (*MirrorEndpoint, error) {
	raw := strings.TrimSpace(baseURL)
	if raw == "" {
		return nil, errors.New("the Git Server base URL is required")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("the Git Server base URL is not a URL: %w", err)
	}
	if parsed.Scheme != "https" {
		return nil, errors.New("the Git Server base URL must be https")
	}
	if parsed.Host == "" {
		return nil, errors.New("the Git Server base URL names no host")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("the Git Server base URL must not carry user info, a query, or a fragment")
	}
	if strings.Trim(parsed.Path, "/") != "" {
		return nil, errors.New("the Git Server base URL must not carry a path")
	}
	return &MirrorEndpoint{base: parsed}, nil
}

// CloneAddress is the URL one mirror is cloned from.
//
// The key is validated rather than trusted: an address built from a key that names no mirror would be
// an address the Git Server cannot serve, and an Operation's recorded clone input is far too late a
// place to discover that. The path comes from the server itself, so the address Cloud records is one
// that server answers on.
func (e *MirrorEndpoint) CloneAddress(key mirror.Key) (string, error) {
	if err := key.Validate(); err != nil {
		return "", err
	}
	return e.base.JoinPath(gitserver.MirrorPath(key)).String(), nil
}
