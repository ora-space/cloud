package config

import (
	"errors"
	"fmt"
	"strings"

	"github.com/wanglongan587/cloud/internal/core"
)

// GitServerConfig is where Cloud is told that this deployment serves Workspace content from the
// internal Git Server.
//
// It holds one value, and it holds it because Cloud has to write it into a clone input: the address a
// sandbox reaches the Git Server at. The address a *sandbox* reaches is not the one the deployment's
// own tooling queries, which is why this is configuration rather than a fact Cloud could derive — the
// server resolves the caller's container from the connection's peer address, so an address Cloud
// guessed wrong would make every read look like it came from somewhere else.
//
// Cloud stores no mirror address anywhere: the address is derived per clone from the mirror's key
// (Project, canonical upstream repository, pinned commit), and this section supplies only the base.
//
// An empty section is the approved default and means this deployment serves no mirror, with every
// credentialed Workspace kept behind the `mirror_not_ready` gate. A non-empty but unusable value is a
// startup error, not a runtime downgrade.
type GitServerConfig struct {
	// SandboxBaseURL is the internal Git Server's sandbox-facing origin, for example
	// `https://git-server:8080`. It must be an `https` origin with no user info, path, query or
	// fragment: the value is written into a durable clone input, so a plaintext address, a smuggled
	// credential or a path the server does not serve is refused at startup rather than recorded on an
	// Operation that then fails to clone.
	SandboxBaseURL string `mapstructure:"sandbox_base_url"`
}

// MirrorEndpoint builds the internal Git Server's address for this deployment, and refuses a
// deployment that declared a content channel it cannot serve.
//
// `dispatch` is the opened `clone_dispatch` section. It is passed in rather than read from the
// receiver so the two facts are checked together: whether a mirror is published at all, and whether
// anything is left to clone from. Both refusals exist because the alternative is a deployment that
// starts and then does not do what its configuration says:
//
//   - A Git Server *and* credentialed clone dispatch is two content channels, and the decision fixes
//     exactly one (D1). Cloud prefers the mirror when one is named, so `enabled` would be a capability
//     that is declared, signed for and never reached — a live-looking switch that dispatches nothing.
//   - A Git Server with mirror preparation off has no publisher, so no preparation row is ever
//     claimed, every credentialed Workspace's clone is refused `mirror_not_ready` forever, and the
//     deployment would look like a mirror that is merely late.
//
// Nothing is refused in the other direction. Mirror preparation without a Git Server is a coherent
// deployment — it is the pre-mirror channel with the preparation machinery exercised — and the
// approved fail-closed state is neither of them, which is what an unconfigured section yields.
func (c *Config) MirrorEndpoint(dispatch *ExecutorDispatch) (*core.MirrorEndpoint, error) {
	raw := strings.TrimSpace(c.GitServer.SandboxBaseURL)
	if raw == "" {
		return nil, nil
	}
	endpoint, err := core.NewMirrorEndpoint(raw)
	if err != nil {
		return nil, fmt.Errorf("git_server.sandbox_base_url: %w", err)
	}
	if dispatch.Clone != nil {
		return nil, errors.New("git_server.sandbox_base_url and clone_dispatch.enabled are two content channels and this deployment may serve only one: with the internal Git Server named, a credentialed Workspace's clone comes from the published mirror, so the credentialed-clone executor would never be dispatched to")
	}
	if dispatch.Mirror == nil {
		return nil, errors.New("git_server.sandbox_base_url names the internal Git Server but clone_dispatch.mirror_enabled is off, so nothing publishes a mirror and every credentialed Workspace would wait for one that never appears")
	}
	return endpoint, nil
}
