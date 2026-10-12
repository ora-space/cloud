package gitserver

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/wanglongan587/cloud/internal/gitread"
)

// originPath is the one route the origin resolver serves. It is a constant rather than
// configuration: a deployment that could point this somewhere else could point identity resolution
// at something that is not the resolver.
const originPath = "/origin/v1/resolve"

// originBodyLimit bounds the resolver's response. It is a small JSON object; a body larger than this
// is not the resolver answering.
const originBodyLimit = 64 << 10

// originIdentity is the resolver's wire shape. It is declared here rather than imported from
// sandbox-server: the two are separate programs in separate repositories, and the Git Server should
// fail to compile against a shape it does not recognize rather than silently accept a renamed field
// as an empty string.
type originIdentity struct {
	ContainerID   string `json:"containerId"`
	ContainerName string `json:"containerName"`
	Atespace      string `json:"atespace"`
	SandboxID     string `json:"sandboxId"`
	EffectID      string `json:"effectId"`
	WorkspaceID   string `json:"workspaceId"`
	Address       string `json:"address"`
}

// OriginClient resolves a connection's peer address to the sandbox container that holds it.
//
// It calls the resolver over mutual TLS with the Git Server's own identity and pins the resolver's
// certificate, so neither end can be impersonated by another service the same CA has issued a
// certificate to. It answers only what the resolver answered: an address the resolver refuses is a
// refusal, and a resolver that could not be reached is unavailability. The two are never merged,
// because turning "I could not tell" into "you may not" hides an outage behind a decision.
type OriginClient struct {
	endpoint *url.URL
	client   *http.Client
	log      *slog.Logger
}

// NewOriginClient builds the resolver client. baseURL is the resolver's origin, which must be HTTPS:
// a plaintext resolver call would state the peer address to anyone on the path.
// keeps its configuration and edits it afterwards cannot move a running client's trust anchor.
//
//nolint:gocritic // By value on purpose: the identity is a construction snapshot, so a caller that
func NewOriginClient(baseURL string, identity OriginIdentity, timeout time.Duration, log *slog.Logger) (*OriginClient, error) {
	endpoint, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return nil, fmt.Errorf("parse the origin resolver URL: %w", err)
	}
	if endpoint.Scheme != "https" || endpoint.Host == "" {
		return nil, fmt.Errorf("the origin resolver URL %q must be an absolute https URL", baseURL)
	}
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	tlsConfig, err := identity.TLS()
	if err != nil {
		return nil, err
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	transport := &http.Transport{
		TLSClientConfig: tlsConfig,
		// One connection per resolution: the resolver queries the Engine on every call, and a
		// pooled connection would keep an identity that a rotation or a restart has retired.
		DisableKeepAlives:   true,
		MaxIdleConnsPerHost: 1,
	}
	return &OriginClient{
		endpoint: endpoint,
		client:   &http.Client{Transport: transport, Timeout: timeout},
		log:      log,
	}, nil
}

// Resolve answers which sandbox container holds peer.
func (c *OriginClient) Resolve(ctx context.Context, peer netip.Addr) (gitread.Origin, error) {
	if !peer.IsValid() {
		return gitread.Origin{}, fmt.Errorf("%w: %q is not an address", ErrOriginUnresolvable, peer)
	}
	target := *c.endpoint
	target.Path = originPath
	query := url.Values{}
	query.Set("address", peer.Unmap().String())
	target.RawQuery = query.Encode()

	//nolint:gosec // G704: the URL is the deployment-configured resolver endpoint, and the only
	// variable part is an address query parameter that must already be a valid address literal.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), http.NoBody)
	if err != nil {
		return gitread.Origin{}, fmt.Errorf("build the origin request: %w", err)
	}
	resp, err := c.client.Do(req) //nolint:gosec // G704: same request as above; the URL is fixed config.
	if err != nil {
		return gitread.Origin{}, fmt.Errorf("%w: %v", ErrOriginUnavailable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusForbidden:
		// The resolver decided this address belongs to no running sandbox container.
		return gitread.Origin{}, fmt.Errorf("%w: %s refused %s", ErrOriginUnresolvable, c.endpoint.Host, peer)
	default:
		// Every other status is the resolver failing to answer — including a 400, which would mean
		// this process built a request the resolver could not read, and is not a fact about the
		// caller.
		return gitread.Origin{}, fmt.Errorf("%w: %s answered %d", ErrOriginUnavailable, c.endpoint.Host, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, originBodyLimit))
	if err != nil {
		return gitread.Origin{}, fmt.Errorf("%w: read the origin answer: %v", ErrOriginUnavailable, err)
	}
	var identity originIdentity
	if err := json.Unmarshal(body, &identity); err != nil {
		return gitread.Origin{}, fmt.Errorf("%w: parse the origin answer: %v", ErrOriginUnavailable, err)
	}
	// Only an explicit 403 above is a refusal. Everything below is a 200 whose body is not an
	// identity, which — because the connection is pinned to the resolver's own certificate — can
	// only mean the resolver malfunctioned. That is unavailability, not a decision about the
	// caller, and it stays unavailability all the way out: the read is refused either way, and the
	// operator seeing 503 instead of 403 is the difference between "your resolver is broken" and
	// "this sandbox may not read that".
	// The identity that binds is the sandbox instance id from the container's own label, not the
	// Docker container id the resolver also reports. Cloud's row for a live sandbox is keyed by the
	// instance id (`sandbox_instances.id`), and the label is written with that same value when the
	// container is created; the Docker id is the substrate's identifier, recorded separately in
	// `substrate_sandbox_id`. Binding to the Docker id here would compare it against an instance id
	// and refuse every read, so the label is not an alternative spelling of the same fact — it is the
	// only one of the two that answers the question the policy asks.
	origin := gitread.Origin{
		WorkspaceID:   identity.WorkspaceID,
		ContainerID:   identity.SandboxID,
		ContainerName: identity.ContainerName,
	}
	if err := origin.Validate(); err != nil {
		return gitread.Origin{}, fmt.Errorf("%w: %s answered 200 without an identity: %v", ErrOriginUnavailable, c.endpoint.Host, err)
	}
	if identity.Address != "" && identity.Address != peer.Unmap().String() {
		// The resolver is supposed to have answered for the address we asked about. If it echoes a
		// different one, this process and the resolver disagree about the connection, and a
		// disagreement about the connection is not something to resolve by picking a side.
		return gitread.Origin{}, fmt.Errorf("%w: %s answered for %s, not %s", ErrOriginUnavailable, c.endpoint.Host, identity.Address, peer)
	}
	return origin, nil
}
