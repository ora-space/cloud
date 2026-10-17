// Package repoexec is the Repo Executor: the only Ora component that talks to external Git
// repositories. Cloud dispatches one task per sync, publish or probe over mTLS together with a
// one-time authorization; the executor verifies the authorization against the task field by field,
// redeems the credential once, runs Git in a private temporary bare repository and reports only what
// it observed. Sync results reach the Git Server through its service repository listener; publish
// reads its source from there and pushes to the external repository with a lease. The executor keeps
// no state between tasks.
package repoexec

import (
	"time"

	"github.com/wanglongan587/cloud/internal/cloneauth"
)

// DispatcherIdentity is the only client identity the task listener serves: Cloud's Operation worker.
const DispatcherIdentity = "spiffe://ora.local/cloud/repo-dispatcher"

// maxTaskBody bounds one task request. The largest field is the authorization, itself bounded by
// cloneauth; anything bigger is a malformed caller rather than a large task.
const maxTaskBody = 64 << 10

// Task is one unit of work Cloud asks for. Every field except Authorization and TimeoutSeconds must
// equal the same field of the verified authorization (empty equals empty); the duplicate exists so the
// request is readable and auditable on its own and so a mismatch is caught before anything is spent.
type Task struct {
	// Authorization is the signed one-time grant (cloneauth token). It is a bearer secret: never
	// logged and never echoed.
	Authorization       string            `json:"authorization"`
	Purpose             cloneauth.Purpose `json:"purpose"`
	OperationID         string            `json:"operationId,omitempty"`
	VerificationID      string            `json:"verificationId,omitempty"`
	TenantID            string            `json:"tenantId,omitempty"`
	ProjectID           string            `json:"projectId,omitempty"`
	BindingID           string            `json:"bindingId,omitempty"`
	Repository          string            `json:"repository,omitempty"`
	Branch              string            `json:"branch,omitempty"`
	ExpectedTrackingOID string            `json:"expectedTrackingOid,omitempty"`
	SourceRef           string            `json:"sourceRef,omitempty"`
	SourceCommit        string            `json:"sourceCommit,omitempty"`
	TargetRef           string            `json:"targetRef,omitempty"`
	ExpectedOldOID      string            `json:"expectedOldOid,omitempty"`
	Service             string            `json:"service,omitempty"`
	CredentialID        string            `json:"credentialId,omitempty"`
	CredentialVersion   int64             `json:"credentialVersion,omitempty"`
	// TimeoutSeconds is the task's total budget. It is independent of the authorization's lifetime
	// (which only bounds starting and redeeming) and may not exceed the deployment's maximum for the
	// purpose.
	TimeoutSeconds int `json:"timeoutSeconds"`
}

// Timeout is the task's total budget.
func (t *Task) Timeout() time.Duration { return time.Duration(t.TimeoutSeconds) * time.Second }

// Classification is the one fact class a task reports (ADR D8). Cloud alone decides the Operation
// state from it.
type Classification string

// The classes of ADR D8.
const (
	// Applied: the write is confirmed (sync push succeeded or was a no-op replay; publish push
	// reported ok or verification found the source commit). A probe that read its advertisement also
	// reports Applied.
	Applied Classification = "applied"
	// Superseded: the sync lease failed with stale info; someone else moved the tracking ref.
	Superseded Classification = "superseded"
	// Conflict: the external ref did not equal expectedOldOid (pre-check or lease).
	Conflict Classification = "conflict"
	// Rejected: the external platform refused the update and verification shows it unchanged.
	Rejected Classification = "rejected"
	// NotFastForward: expectedOldOid is not an ancestor of the source commit; nothing external was
	// written to.
	NotFastForward Classification = "not_fast_forward"
	// SourceUnavailable: the source commit is not reachable from the source ref on the Git Server.
	SourceUnavailable Classification = "source_unavailable"
	// Failed: certainly nothing was written.
	Failed Classification = "failed"
	// Uncertain: a publish push may have been sent and neither its report nor verification settles
	// whether it took effect. The executor never retries it.
	Uncertain Classification = "uncertain"
)

// Error codes reported with a classification. They are stable, safe to show and never carry input.
const (
	CodeInvalidTask           = "invalid_task"
	CodeAuthorizationInvalid  = "authorization_invalid"
	CodeAuthorizationExpired  = "authorization_expired"
	CodeAuthorizationMismatch = "authorization_mismatch"
	CodeRepositoryURLInvalid  = "repository_url_invalid"
	CodeEgressDenied          = "egress_denied"
	CodeRedemptionFailed      = "redemption_failed"
	CodeUpstreamBranchMissing = "upstream_branch_missing"
	CodeRequiresCredential    = "repository_requires_credential"
	CodeCredentialRejected    = "credential_rejected"
	CodeRepositoryTooLarge    = "repository_too_large"
	CodeTimeout               = "timeout"
	CodeFetchFailed           = "fetch_failed"
	CodeGitServerRejected     = "git_server_rejected"
	CodeGitServerUnavailable  = "git_server_unavailable"
	CodeSourceUnavailable     = "source_unavailable"
	CodeNotFastForward        = "not_fast_forward"
	CodeExternalRejected      = "external_rejected"
	CodePushDenied            = "push_denied"
	CodeVerificationFailed    = "verification_failed"
	CodeInternal              = "internal"
	// CodeExecutorBusy is the HTTP 429 refusal when every slot is taken; the task did not start.
	CodeExecutorBusy = "executor_busy"
)

// Result is what the executor observed. It is always returned with HTTP 200 once a task was
// accepted; Classification is always set.
type Result struct {
	Classification Classification `json:"classification"`
	ErrorCode      string         `json:"errorCode,omitempty"`
	// Detail is a diagnostic excerpt: secret and URL userinfo redacted first, then cut to 2 KiB.
	Detail string `json:"detail,omitempty"`

	// Sync.
	CommitOID   string `json:"commitOid,omitempty"`
	SnapshotRef string `json:"snapshotRef,omitempty"`
	TrackingRef string `json:"trackingRef,omitempty"`
	Forced      bool   `json:"forced,omitempty"`

	// ObservedOID is, for a superseded sync, the tracking value seen; for a publish, the external
	// value the classification rests on (post-check when read, otherwise pre-check).
	ObservedOID string `json:"observedOid,omitempty"`

	// Publish.
	PreObservedOID          string `json:"preObservedOid,omitempty"`
	PostObservedOID         string `json:"postObservedOid,omitempty"`
	ConfirmedByVerification bool   `json:"confirmedByVerification,omitempty"`

	// Probe.
	Refs       map[string]string `json:"refs,omitempty"`
	Advertised bool              `json:"advertised,omitempty"`
	HTTPStatus int               `json:"httpStatus,omitempty"`

	// CleanupFailed reports that the task directory or the credential material could not be
	// verified gone.
	CleanupFailed bool `json:"cleanupFailed"`
}

func failed(code, detail string) Result {
	return Result{Classification: Failed, ErrorCode: code, Detail: detail}
}
