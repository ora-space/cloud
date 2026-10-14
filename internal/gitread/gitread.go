// Package gitread is the read-authorization contract for the internal Git Server.
//
// It holds two things and nothing else: the vocabulary a read request and a read decision are
// expressed in, and the pure policy that turns Cloud's authoritative state for one request into an
// allow or a refusal. It opens no socket, runs no Git, reads no database and holds no credential, so
// the rule that decides whether a Workspace may read a mirror can be tested exhaustively, and the
// same rule is what production runs — there is no second implementation to drift.
//
// Two boundaries are the reason this package exists at all (D5, invariants 9 and 13):
//
//   - A request never carries its own authority. The caller's identity arrives as an Origin that the
//     origin resolver produced from deployment-owned container state; nothing else about the caller is
//     consulted. A path, a header, a query parameter or a claimed execution id can name *what* is
//     wanted and can never establish *who* is asking or *whether* they may have it.
//   - Resolution is not authorization. The origin resolver answering successfully says only which
//     container is calling. Every read is authorized again, per request, against Cloud's current
//     state: the workspace it belongs to, the project that workspace is bound to, the mirror key, the
//     mirror's generation, whether that mirror is ready, and whether the container is still the live
//     one for its workspace. Missing or inconsistent state is a refusal, never a default.
package gitread

import (
	"errors"
	"fmt"
	"strings"

	"github.com/wanglongan587/cloud/internal/mirror"
)

// Operation is the Git service a caller asked for. The vocabulary is closed: a service this package
// does not name is OperationUnknown, which no policy allows.
type Operation string

const (
	// OperationUploadPack is the only operation the internal Git Server serves: reading objects.
	OperationUploadPack Operation = "upload-pack"
	// OperationReceivePack is a write, which the internal Git Server does not implement at all. It is
	// named here so the policy can refuse it explicitly rather than falling through to "unknown".
	OperationReceivePack Operation = "receive-pack"
	// OperationUploadArchive asks for a tarball instead of a repository. It is a read, but a different
	// protocol with a different authorization surface, so it is refused until a decision covers it.
	OperationUploadArchive Operation = "upload-archive"
	// OperationUnknown is any other service, including the empty one.
	OperationUnknown Operation = "unknown"
)

// ParseService maps the `service` value of a smart-HTTP request onto an operation. Only the exact
// Git service names are recognized; anything else is OperationUnknown.
func ParseService(service string) Operation {
	switch service {
	case "git-upload-pack":
		return OperationUploadPack
	case "git-receive-pack":
		return OperationReceivePack
	case "git-upload-archive":
		return OperationUploadArchive
	default:
		return OperationUnknown
	}
}

// ParseRPCPath maps the trailing path component of a smart-HTTP request onto an operation.
func ParseRPCPath(path string) Operation {
	switch path {
	case "git-upload-pack":
		return OperationUploadPack
	case "git-receive-pack":
		return OperationReceivePack
	case "git-upload-archive":
		return OperationUploadArchive
	default:
		return OperationUnknown
	}
}

// ReadOnly reports whether an operation may be served. Only reading objects is served, so this is
// the single place that answers "may this be served at all", independent of who is asking.
func (o Operation) ReadOnly() bool { return o == OperationUploadPack }

// Origin is the caller's identity as the origin resolver observed it: the container that holds the
// address the connection came from, and the workspace that container belongs to.
//
// It carries no tenant and no runtime generation, and that is deliberate. Those are facts Cloud
// holds, not facts the container does: the resolver reads the control plane's own labels, and the
// labels name a workspace and a sandbox instance. Cloud is what turns a sandbox instance into a
// tenant, a generation and a project, so a caller cannot assert any of them and this type cannot
// carry a value a caller chose.
//
// Every field is deployment-owned. The type exists to be *complete*: an Origin missing any of these
// is not a weaker identity, it is not an identity, and Validate refuses it so no policy can compare
// against an empty string and accidentally match.
type Origin struct {
	// WorkspaceID is the workspace the calling container belongs to, from the container's own label.
	WorkspaceID string
	// ContainerID is the sandbox instance id, which is also the container's own sandbox label — the
	// value that ties a container to the row Cloud holds for it, and through that row to a tenant, a
	// runtime generation and a project.
	ContainerID string
	// ContainerName is the container's name. It is carried for logs only and decides nothing: a name
	// is not unique across time and is not what Cloud's rows are keyed by.
	ContainerName string
}

// ErrOrigin reports an origin that cannot be used as an identity.
var ErrOrigin = errors.New("origin identity is not usable")

// Validate reports whether the origin carries every field an authorization needs.
func (o Origin) Validate() error {
	if strings.TrimSpace(o.WorkspaceID) == "" {
		return fmt.Errorf("%w: it names no workspace", ErrOrigin)
	}
	if strings.TrimSpace(o.ContainerID) == "" {
		return fmt.Errorf("%w: it names no container", ErrOrigin)
	}
	return nil
}

// String renders an origin for logs. Identifiers are not secrets; credentials are, and this type
// never holds one.
func (o Origin) String() string {
	return fmt.Sprintf("workspace=%s container=%s", o.WorkspaceID, o.ContainerID)
}

// Request is one read request, reduced to what authorization may consider: who is asking (from
// resolution), what mirror they name (from the path), and what they want to do with it (from the
// protocol).
type Request struct {
	// Origin is the resolved caller. It is never derived from the request's own content.
	Origin Origin
	// MirrorDigest is the opaque mirror identifier from the request path. It names a mirror; it does
	// not grant one, which is why nothing decides anything from the digest alone.
	MirrorDigest string
	// Operation is the Git service the caller asked for.
	Operation Operation
}

// Snapshot is Cloud's authoritative state for one request, read at request time.
//
// The shape is deliberate: the fields needed to decide are exactly the fields present, so a decision
// cannot be made from a partial read. What this package does not do is fetch the snapshot — that is
// Cloud's reader, which must answer with the state *now*, because a cached or remembered binding is
// what invariant 3 forbids.
type Snapshot struct {
	// Found reports whether Cloud knows a mirror with this digest at all. When it is false every other
	// field is meaningless and the request is refused.
	Found bool
	// Key is the mirror key Cloud has for this digest: project, canonical upstream, commit.
	Key mirror.Key
	// MirrorGeneration is the generation Cloud observed for that mirror.
	MirrorGeneration int64
	// MirrorReady is whether that mirror was ready when the snapshot was taken.
	MirrorReady bool
	// MirrorRevoked is whether the mirror was withdrawn. It is separate from "not ready" because the
	// two are different facts, even though both refuse.
	MirrorRevoked bool
	// ContainerLive is whether the calling container is still the live sandbox instance of its
	// workspace. A stopped, terminated or replaced container is not, and neither is a container from
	// an earlier runtime generation: each generation has its own sandbox instance row, so binding the
	// instance is what binds the generation.
	ContainerLive bool
	// WorkspaceID and ContainerID are the binding Cloud currently holds for that live container. The
	// origin's values must equal these; the origin is what was observed, these are what is
	// authoritative, and a disagreement between them is a refusal in either direction.
	WorkspaceID string
	ContainerID string
	// RuntimeGeneration is the generation Cloud holds for that instance. It is reported rather than
	// compared: the caller cannot assert it, and the instance id already pins it.
	RuntimeGeneration int64
	// WorkspaceProjectID is the project the workspace is bound to. The mirror's project must be the
	// same one, which is what makes a cross-project read impossible even inside one tenant.
	WorkspaceProjectID string
	// WorkspaceTenantID and ProjectTenantID are the tenants Cloud's own join produced on each side.
	// They must agree. This is a tripwire for an inconsistent read rather than the barrier itself —
	// the barrier is that a container reaches a project only through its own workspace row — and a
	// tripwire that stays silent costs nothing while a join that goes wrong is otherwise invisible.
	WorkspaceTenantID string
	ProjectTenantID   string
}

// Refusal reasons. They are stable so they can be logged and counted, and they are never returned to
// the caller: every refusal is one indistinguishable response, because telling a caller *why* it was
// refused tells it what exists.
const (
	// ReasonOperationNotServed is a write, an archive request, or an unrecognized service.
	ReasonOperationNotServed = "operation_not_served"
	// ReasonOriginIncomplete is an origin that is not an identity.
	ReasonOriginIncomplete = "origin_incomplete"
	// ReasonMirrorUnknown is a digest Cloud has no mirror for.
	ReasonMirrorUnknown = "mirror_unknown"
	// ReasonMirrorNotReady is a mirror that is preparing, failed or destroyed.
	ReasonMirrorNotReady = "mirror_not_ready"
	// ReasonMirrorRevoked is a mirror that was withdrawn.
	ReasonMirrorRevoked = "mirror_revoked"
	// ReasonContainerNotLive is a container that is no longer the live one for its workspace.
	ReasonContainerNotLive = "container_not_live"
	// ReasonCrossTenant is an origin whose tenant is not the tenant Cloud binds the container to.
	ReasonCrossTenant = "cross_tenant"
	// ReasonCrossWorkspace is an origin whose workspace is not the one Cloud binds the container to.
	ReasonCrossWorkspace = "cross_workspace"
	// ReasonCrossProject is a workspace whose project is not the mirror's project.
	ReasonCrossProject = "cross_project"
	// ReasonDigestMismatch is a digest that does not name the key Cloud resolved.
	ReasonDigestMismatch = "digest_mismatch"
	// ReasonGenerationChanged is a mirror whose generation moved after authorization.
	ReasonGenerationChanged = "mirror_generation_changed"
)

// Verdict is the outcome of one authorization. Reason is for logs and counters only.
type Verdict struct {
	// Allow is the decision. Everything else in the verdict is only meaningful when it is true.
	Allow bool
	// Key is the mirror key Cloud resolved for this digest.
	Key mirror.Key
	// Generation is the mirror generation the authorization binds. A reader must serve that
	// generation and refuse if the mirror has moved on, so an authorization cannot silently follow a
	// republication.
	Generation int64
	// Reason is the stable code for a refusal, empty when Allow is true.
	Reason string
}

func refuse(reason string) Verdict { return Verdict{Reason: reason} }

// Evaluate decides one read request against Cloud's authoritative state for it.
//
// The order is the security order. The operation is checked first so a write is refused before any
// state is consulted; the origin is checked for completeness so an incomplete identity cannot match
// an empty authoritative field; liveness precedes the binding comparisons, because a container that
// is no longer live has no binding to compare; and the mirror's own state is checked last, after the
// caller is established as the right one, so a caller that is not entitled to a mirror never learns
// anything about it.
// about itself, so the policy stays a pure function of the request it was handed.
//
//nolint:gocritic // By value on purpose: a request is built at the call site and decides nothing
func (r Request) Evaluate(s Snapshot) Verdict {
	if !r.Operation.ReadOnly() {
		return refuse(ReasonOperationNotServed)
	}
	if err := r.Origin.Validate(); err != nil {
		return refuse(ReasonOriginIncomplete)
	}
	if strings.TrimSpace(r.MirrorDigest) == "" {
		return refuse(ReasonMirrorUnknown)
	}
	if !s.Found {
		return refuse(ReasonMirrorUnknown)
	}
	if err := s.Key.Validate(); err != nil {
		// Cloud answered with something that is not a mirror key. Treating that as a refusal rather
		// than an error keeps a malformed record from becoming an allow.
		return refuse(ReasonMirrorUnknown)
	}
	if s.Key.Digest() != r.MirrorDigest {
		return refuse(ReasonDigestMismatch)
	}
	if !s.ContainerLive {
		return refuse(ReasonContainerNotLive)
	}
	if r.Origin.WorkspaceID != s.WorkspaceID {
		return refuse(ReasonCrossWorkspace)
	}
	if r.Origin.ContainerID != s.ContainerID {
		return refuse(ReasonContainerNotLive)
	}
	if s.WorkspaceTenantID == "" || s.ProjectTenantID == "" {
		// A missing tenant is missing state, and missing state is a refusal rather than a default.
		return refuse(ReasonCrossTenant)
	}
	if s.WorkspaceTenantID != s.ProjectTenantID {
		return refuse(ReasonCrossTenant)
	}
	if s.WorkspaceProjectID != s.Key.ProjectID {
		return refuse(ReasonCrossProject)
	}
	if s.MirrorRevoked {
		return refuse(ReasonMirrorRevoked)
	}
	if !s.MirrorReady {
		return refuse(ReasonMirrorNotReady)
	}
	if s.MirrorGeneration <= 0 {
		return refuse(ReasonMirrorNotReady)
	}
	return Verdict{Allow: true, Key: s.Key, Generation: s.MirrorGeneration}
}
