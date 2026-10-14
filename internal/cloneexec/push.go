package cloneexec

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/wanglongan587/cloud/internal/cloneauth"
	"github.com/wanglongan587/cloud/internal/credentialmaterial"
)

// PushJob is one controlled push: Cloud's request that this executor update exactly one ref of exactly
// one upstream repository from one commit to another, under one single-use authorization.
//
// It is a type of its own on a route of its own, for the reason the mirror job is: the three subjects
// are different — a clone is one execution's checkout on a Node, a mirror preparation is one attempt of
// one Project Operation, a push is one person's approval of one write — and a single job shape would
// put the decision about which identity fields are meaningful inside C instead of in the signature.
//
// The four values that describe the write — repository, ref, old commit, new commit — are all signed by
// the authorization, and every one of them is compared against it before anything is spent. Nothing
// here is a hint.
type PushJob struct {
	// OperationID is the push request's own identity: the approval Cloud recorded. The authorization
	// binds it, and it names the flow a later reader can look up.
	OperationID string `json:"operationId"`
	// ProjectID and WorkspaceID scope the write to the Project whose repository it is and the Workspace
	// the approval was made in.
	ProjectID   string `json:"projectId"`
	WorkspaceID string `json:"workspaceId"`
	// Repository is the upstream to write to, canonicalized here and compared against the authorization.
	Repository string `json:"repository"`
	// Ref is the one fully qualified ref this write may update, for example `refs/heads/main`.
	Ref string `json:"ref"`
	// OldCommit and NewCommit are the write's object: the value the ref was observed at and the single
	// commit it may become.
	OldCommit string `json:"oldCommit"`
	NewCommit string `json:"newCommit"`
	// CredentialID and CredentialVersion name the material the authorization is for.
	CredentialID      string `json:"credentialId"`
	CredentialVersion int64  `json:"credentialVersion"`
	// Authorization is the Cloud-signed single-use authorization. It is a secret.
	Authorization string `json:"authorization"`
}

// PushState is what one push attempt established about the remote.
//
// It is one closed vocabulary rather than a pair of booleans, because the two questions a push asks —
// does the ref hold the authorized commit, and is that this write's doing — can disagree in ways a pair
// of flags cannot express honestly: "the ref holds the new commit and nothing says this attempt put it
// there" and "the ref holds a commit nobody authorized" are different facts about the world, and a
// settlement that could not tell them apart would record one of them as the other.
//
// The five members are mutually exclusive by construction, and `unknown` is one of them rather than the
// absence of an answer: a push whose outcome could not be established must be recordable as exactly
// that, and never as an empty value a reader might read as "nothing happened".
type PushState string

const (
	// PushConfirmed is the only success: the remote now holds the authorized new commit at the
	// authorized ref, and this attempt is what put it there. It is established by the compare-and-swap
	// itself — the update was sent, which is only possible if the ref still held the authorized old
	// commit at the moment the remote applied it.
	PushConfirmed PushState = "confirmed"
	// PushTargetSatisfied is the execution authority's "写入者不可归因": the ref holds the authorized
	// commit, and nothing proves this attempt is what moved it. It is reached when Git reports the ref was
	// already up to date, or when the attempt was refused but a read-only reconciliation finds the ref at
	// the new commit anyway. The target state is reached; this approval is not credited with reaching it.
	PushTargetSatisfied PushState = "target_satisfied"
	// PushNotApplied is a confirmed failure: the remote explicitly refused this update and the ref does
	// not hold the authorized commit. Nothing was written and nothing may be retried under this
	// authorization.
	PushNotApplied PushState = "not_applied"
	// PushTargetConflict is the third value: the ref holds neither the authorized old commit nor the
	// authorized new one, so another writer moved it — or this write landed and was then superseded, which
	// a single observation cannot distinguish. It is recorded as a conflict rather than as either success
	// or failure because neither claim is supportable.
	PushTargetConflict PushState = "target_conflict"
	// PushUnknown is an attempt whose outcome could not be established at all: a transport failure, a
	// timeout, a killed process, or a reconciliation that could not be read. It is never inferred to be a
	// success or a failure, because both would be claims this executor cannot support.
	PushUnknown PushState = "unknown"
)

// known reports whether the state is one this build defines.
//
// It exists for the boundary where a state arrives from another process. A client that read a state it
// does not recognize must not pass it on as though it were an answer, and a caller that has to decide
// what an unrecognized state means will eventually decide it means "nothing happened" — the one reading
// that is never safe about a write. The check names the members rather than testing a prefix or a
// length, so a member added here is one every boundary accepts and nothing else is.
func (s PushState) known() bool {
	switch s {
	case PushConfirmed, PushTargetSatisfied, PushNotApplied, PushTargetConflict, PushUnknown:
		return true
	default:
		return false
	}
}

// PushResult is what one push attempt reports.
//
// It is returned for every attempt that got as far as the write, including the ones that failed and the
// ones whose outcome is unknown, because the state is the answer and only the state distinguishes them.
// An error from Push means the attempt never reached the write: a refusal this executor decided, with a
// stable code, or a failure of the credential path.
type PushResult struct {
	// State is what the attempt established about the remote.
	State PushState `json:"state"`
	// RemoteCommit is what the authorized ref held when the executor read it back, when it read it back
	// at all. It is empty only when no read was made or the read produced nothing, and it is never used to
	// change the state — the reconciliation is evidence for the operator and for Cloud's audit, not a
	// second chance to call the write a success.
	RemoteCommit string `json:"remoteCommit,omitempty"`
	// CredentialVersion is the material version the write actually used.
	CredentialVersion int64 `json:"credentialVersion,omitempty"`
}

// Reason codes this executor reports for a push. They are the executor's own vocabulary, as the clone
// and mirror paths' are, and they name the two pre-write determinations a push makes that the other two
// paths have no equivalent of.
const (
	// ReasonCodePushFetchFailed reports that the authorized upstream could not be read. The write never
	// started: this determination happens before it, so a fetch that fails is a confirmed failure of this
	// attempt and not an unknown outcome.
	ReasonCodePushFetchFailed = "credential_executor_push_fetch_failed"
	// ReasonCodePushNotApplicable reports that the authorized write is not one this executor may perform:
	// the authorized new commit does not exist in the authorized upstream, or the authorized old commit is
	// not an ancestor of it. Both are local determinations made before any network write, so both are
	// confirmed failures. They are reported as refusals rather than as `not_applied` because the write was
	// never attempted at all, and an operator needs to tell "the remote said no" from "the job named a
	// commit that is not there".
	ReasonCodePushNotApplicable = "credential_executor_push_not_applicable"
)

// DefaultPushTimeout bounds one push when the deployment configures none.
//
// A push is a fetch plus one small update, so it is bounded like a clone rather than like a mirror
// preparation: the fetch is of the same branch set a clone would take, and the update transfers only the
// objects the remote is missing. The authorization's own expiry bounds it further, and that is the
// binding constraint in practice — a push that outlived its authorization would be one whose CAS could
// no longer be verified by the party that signed it.
const DefaultPushTimeout = 5 * time.Minute

// PushConfig wires the push executor's deployment-fixed inputs.
//
// It repeats the other two paths' Git inputs rather than sharing one Config, for the reason MirrorConfig
// gives: the capabilities are separable in a deployment, and a process that runs only one of them must be
// able to say so. What they must not differ in is the deployment they point at, which is what
// `gitDeployment` is for — and, for a push, a scratch root of its own, because a push materializes an
// upstream repository and a clone's checkout root is handed to a workload.
type PushConfig struct {
	// MaterialRoot is the directory credential material is staged under. It is the same directory the
	// other two paths use: one C identity owns one materialization root, and a second root would be a
	// second place a token could outlive its job.
	MaterialRoot string
	// ScratchRoot is the directory a push's scratch repository is created under. It holds a copy of the
	// authorized upstream in plaintext under whatever credential fetched it, so it is a directory only
	// this identity may traverse, and it is swept at startup like every other place work is abandoned.
	ScratchRoot string
	// HomeDirectory is the executor's own empty directory, used as Git's HOME.
	HomeDirectory string
	// HelperPath is the absolute path of the deployment-fixed credential helper.
	HelperPath string
	// GitPath is the absolute path of the Git binary.
	GitPath string
	// SSLCAInfo is the optional absolute path of the certificate authority file Git verifies the
	// upstream's TLS certificate against. Empty means the system bundle.
	SSLCAInfo string
	// Egress is the deployment's optional, default-off proxy for Git's own egress, scoped to the hosts
	// it names. The zero value is the approved default and routes nothing through a proxy; with it
	// unset the command line is byte-for-byte the one that clears every proxy (D17's boundary is
	// unchanged, and see GitEgress for the recorded deviation).
	Egress GitEgress
	// VerificationKeys is the deployment's authorization verification key set. A deployment that cannot
	// verify has no push executor at all.
	VerificationKeys []ed25519.PublicKey
	// Store redeems push authorizations. It is required for the same reason the other paths' is: an
	// executor that cannot obtain a credential cannot write to a private repository.
	Store PushRedemptionClient
	// Timeout bounds one push.
	Timeout time.Duration
	// Now supplies the clock. It is injected so the local expiry check is deterministic in tests; the
	// authoritative expiry decision belongs to the Store either way.
	Now func() time.Time
	// Log receives one line per push. It never receives a credential or an authorization.
	Log *zap.Logger
}

// PushExecutor performs controlled pushes. It holds no per-job state.
type PushExecutor struct {
	config PushConfig
	log    *zap.Logger
}

// NewPushExecutor validates the configuration and returns a ready executor.
//
// Every check is a deployment boundary and a refusal rather than a default. The two that are specific to
// this path are the scratch root's existence as its own directory and its separation from the material
// root: a push writes an upstream repository to disk, and a deployment that pointed that at the
// directory holding staged tokens would be putting the two in one place for no reason a reviewer could
// accept.
//
//nolint:gocritic // By value on purpose: the deployment descriptor is copied into the executor, so a caller that keeps its Config and edits it cannot move a running executor's boundary afterwards.
func NewPushExecutor(cfg PushConfig) (*PushExecutor, error) {
	if err := cfg.gitDeployment().validate(); err != nil {
		return nil, err
	}
	if err := validateExecutablePath(cfg.ScratchRoot); err != nil {
		return nil, fmt.Errorf("push scratch root: %w", err)
	}
	if cfg.ScratchRoot == cfg.MaterialRoot {
		// One directory cannot be both the place a token is staged and the place a repository fetched
		// with it is written. A deployment that set both to one path would have made the sweep that
		// removes abandoned repositories and the sweep that removes abandoned tokens one operation.
		return nil, errors.New("push scratch root must not be the credential materialization root")
	}
	if len(cfg.VerificationKeys) == 0 {
		return nil, errors.New("push executor requires an authorization verification key")
	}
	for _, key := range cfg.VerificationKeys {
		if len(key) != ed25519.PublicKeySize {
			return nil, errors.New("push executor verification key is not an Ed25519 public key")
		}
	}
	if cfg.Store == nil {
		return nil, errors.New("push executor requires a credential service client")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultPushTimeout
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	log := cfg.Log
	if log == nil {
		log = zap.NewNop()
	}
	if err := os.MkdirAll(cfg.ScratchRoot, publishRootMode); err != nil {
		return nil, fmt.Errorf("create the push scratch root: %w", err)
	}
	if err := os.Chmod(cfg.ScratchRoot, publishRootMode); err != nil {
		return nil, fmt.Errorf("fix the push scratch root mode: %w", err)
	}
	return &PushExecutor{config: cfg, log: log}, nil
}

// gitDeployment is the shared Git and credential-materialization environment this executor runs with.
//
//nolint:gocritic // Value receiver on purpose, for the reason Config's has one: the environment checked is the deployment snapshot `NewPushExecutor` was handed.
func (c PushConfig) gitDeployment() gitDeployment {
	return gitDeployment{
		materialRoot:  c.MaterialRoot,
		homeDirectory: c.HomeDirectory,
		helperPath:    c.HelperPath,
		gitPath:       c.GitPath,
		transport:     gitTransport{sslCAInfo: c.SSLCAInfo, egress: c.Egress},
	}
}

// Sweep removes scratch repositories left by a previous run.
//
// A scratch repository has no life of its own: it is created, filled, and removed inside one call, and
// nothing outside that call names it. Anything under the scratch root at startup is therefore debris
// from a process that died, and removing it is the whole repair — it is also a repair that matters more
// here than on the mirror path, because this directory holds a plaintext copy of a private upstream.
func (p *PushExecutor) Sweep() (int, error) {
	entries, err := os.ReadDir(p.config.ScratchRoot)
	if err != nil {
		return 0, fmt.Errorf("read the push scratch root: %w", err)
	}
	removed := 0
	for _, entry := range entries {
		if err := os.RemoveAll(filepath.Join(p.config.ScratchRoot, entry.Name())); err != nil {
			return removed, fmt.Errorf("remove the abandoned push scratch %s: %w", entry.Name(), err)
		}
		removed++
	}
	return removed, nil
}

// Push performs one controlled push.
//
// The order is what may be spent and when, and it differs from the mirror path's in one place that the
// push decision makes load bearing: the local determinations — does the authorized new commit exist in
// the authorized upstream, and is the authorized old commit an ancestor of it — can only be made after
// the credential has been redeemed, because obtaining the objects is what needs the credential. So they
// happen *after* the Store has spent the authorization and *before* any network write, which means a job
// naming a commit the upstream does not have is refused, and refused at the cost of one authorization.
// That is stated here rather than engineered around: the alternative would be a second content channel
// for the objects, and a second channel is a second thing to authorize.
//
// From the fetch onward the steps are: read the authorized upstream, prove the write is expressible
// locally, perform the one compare-and-swap update, and report what the remote holds now. Nothing here
// retries and nothing here is reusable — a retry is a new approval with a new authorization.
//
//nolint:gocritic // By value on purpose: the job carries the bearer authorization, so the copy this method validates is the copy it spends and no caller can change the job mid-flight.
func (p *PushExecutor) Push(ctx context.Context, job PushJob) (result PushResult, err error) {
	repository, err := cloneauth.CanonicalRepository(job.Repository)
	if err != nil {
		return PushResult{}, p.refuse(ReasonCodeInvalidJob, err)
	}
	if !credentialmaterial.ValidExecutionID(job.OperationID) {
		return PushResult{}, p.refuse(ReasonCodeInvalidJob, errors.New("the job names no operation"))
	}
	if strings.TrimSpace(job.ProjectID) == "" || strings.TrimSpace(job.WorkspaceID) == "" {
		return PushResult{}, p.refuse(ReasonCodeInvalidJob, errors.New("the job names no project or workspace"))
	}
	if !validCredentialID(job.CredentialID) {
		return PushResult{}, p.refuse(ReasonCodeInvalidJob, errors.New("the job names no credential"))
	}
	if strings.TrimSpace(job.Authorization) == "" {
		return PushResult{}, p.refuse(ReasonCodeInvalidJob, errors.New("the job carries no authorization"))
	}
	// The three values that make up the write are validated with the issuer's own rules rather than a
	// second opinion about ref names and object ids: the value the job carries is the value the
	// authorization signed, so a value these refuse is one no authorization from this build could have
	// carried, and the job is malformed rather than mismatched. `ValidateRef` refuses the wildcard, the
	// force prefix, the multi-ref spelling and the traversal forms; `ValidateCommit` refuses an
	// abbreviation, an upper-case spelling and the all-zero object id, which is how a delete and a
	// create-from-nothing are spelled.
	if err := cloneauth.ValidateRef(job.Ref); err != nil {
		return PushResult{}, p.refuse(ReasonCodeInvalidJob, err)
	}
	if err := cloneauth.ValidateCommit(job.OldCommit); err != nil {
		return PushResult{}, p.refuse(ReasonCodeInvalidJob, fmt.Errorf("old commit: %w", err))
	}
	if err := cloneauth.ValidateCommit(job.NewCommit); err != nil {
		return PushResult{}, p.refuse(ReasonCodeInvalidJob, fmt.Errorf("new commit: %w", err))
	}
	if job.OldCommit == job.NewCommit {
		return PushResult{}, p.refuse(ReasonCodeInvalidJob, errors.New("the write moves the ref to the commit it already holds"))
	}
	// The local verification is not a substitute for the Store's, which decides on its own whether to
	// deliver a secret. It exists so that a job whose authority is already known to be unusable does not
	// reach the network, and so the purpose and every binding are checked before a credential is anywhere
	// near this call — including the purpose check that refuses a `clone` or `mirror_prepare`
	// authorization presented on the push route.
	binding, err := cloneauth.Verify(p.config.VerificationKeys, job.Authorization, p.config.Now().UTC())
	if err != nil {
		return PushResult{}, p.refuse(ReasonCodeAuthorizationRejected, err)
	}
	if err := matchPushBinding(&binding, &job, repository); err != nil {
		return PushResult{}, p.refuse(ReasonCodeBindingMismatch, err)
	}
	timeout := p.config.Timeout
	// The authorization's own expiry bounds the push, measured against the same clock `Verify` judged it
	// by. An expiry that has already passed yields an already-cancelled context, which is the intended
	// outcome: nothing below may spend an authorization this executor has just judged expired.
	if remaining := binding.ExpiresAt.Sub(p.config.Now()); remaining < timeout {
		timeout = remaining
	}
	pushContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// The material is staged under the authorization's identifier rather than the operation's. An
	// authorization is spent exactly once, so two attempts of one approval can never stage material under
	// the same name even if Cloud has already recorded a retry while this call was still running.
	identity, err := pushMaterialIdentity(binding.AuthorizationID)
	if err != nil {
		return PushResult{}, p.refuse(ReasonCodeInvalidJob, err)
	}
	credential, err := p.config.Store.ResolvePush(pushContext, job.CredentialID, job.Authorization)
	if err != nil {
		return PushResult{}, p.refuse(ReasonCodeRedemptionFailed, err)
	}
	// The authorization is spent from here on. Every failure below is reported as a failure and nothing
	// below retries, because a retry would need a new authorization rather than this one.
	token := credential.Token
	defer func() { token = "" }()
	if err := p.stage(identity, token, repository); err != nil {
		return PushResult{}, p.refuse(ReasonCodeUnavailable, err)
	}
	// From this line the material exists, so its destruction is registered immediately and its outcome is
	// folded into the result rather than logged and forgotten. A push whose credential may still be on
	// disk is not a completed push, whatever the remote did.
	defer func() {
		if destroyErr := credentialmaterial.Destroy(p.config.MaterialRoot, identity); destroyErr != nil {
			result = PushResult{}
			err = p.refuse(ReasonCodeCleanupFailed, destroyErr)
		}
	}()
	scratch, err := os.MkdirTemp(p.config.ScratchRoot, job.OperationID+".")
	if err != nil {
		return PushResult{}, p.refuse(ReasonCodeUnavailable, fmt.Errorf("create the push scratch directory: %w", err))
	}
	defer func() {
		if removeErr := os.RemoveAll(scratch); removeErr != nil {
			// The startup sweep removes whatever survives here, and a push that succeeded is not made a
			// failure by its own scratch directory outliving it.
			p.log.Warn("a push scratch directory could not be removed",
				zap.String("operationId", job.OperationID), zap.Error(removeErr))
		}
	}()
	// The address dialed is the deployment's own spelling of the repository, not the canonical identity
	// the authorization binds — the same distinction the mirror path makes, and for the same measured
	// reason: a Git service need not answer both spellings, and dialing the canonical form would turn a
	// correct binding into a request for a path that does not exist.
	address := strings.TrimSpace(job.Repository)
	if err := p.materialize(pushContext, address, identity, scratch, token); err != nil {
		return PushResult{}, p.refuse(ReasonCodePushFetchFailed, err)
	}
	if err := p.verifyWriteIsExpressible(pushContext, scratch, &job); err != nil {
		return PushResult{}, p.refuse(ReasonCodePushNotApplicable, err)
	}
	state, remoteCommit := p.update(pushContext, &job, address, identity, scratch, token)
	p.log.Info("push attempted",
		zap.String("operationId", job.OperationID),
		zap.String("projectId", job.ProjectID),
		zap.String("repository", repository),
		zap.String("ref", job.Ref),
		zap.String("state", string(state)),
		zap.Int64("credentialVersion", credential.Version),
	)
	return PushResult{State: state, RemoteCommit: remoteCommit, CredentialVersion: credential.Version}, nil
}

// materialize fetches the authorized upstream into the scratch repository.
//
// The scratch repository is bare and created here rather than configured, so the fetch has somewhere to
// land that nothing else names. The fetch is a read of the one repository the authorization already
// named, under the same closed protocol set the other two paths use, and it cannot write: `--no-tags`
// and the refspec's `refs/heads/` prefix keep it to branch heads, and `--no-write-fetch-head` keeps it
// from leaving a file a later Git command in this repository could be influenced by.
func (p *PushExecutor) materialize(ctx context.Context, address, identity, scratch, token string) error {
	helper, err := helperArgument(p.config.HelperPath, p.config.MaterialRoot, identity)
	if err != nil {
		return err
	}
	arguments, err := pushFetchArguments(helper, address, p.config.gitDeployment().transport)
	if err != nil {
		return err
	}
	if _, stderr, err := runGit(ctx, p.config.GitPath, p.config.HomeDirectory,
		[]string{"init", "--quiet", "--bare", "--", scratch}, scratch); err != nil {
		return redactFailure(stderr, token)
	}
	if _, stderr, err := runGit(ctx, p.config.GitPath, p.config.HomeDirectory, arguments, scratch); err != nil {
		return redactFailure(stderr, token)
	}
	return nil
}

// verifyWriteIsExpressible makes the two local determinations the push decision requires before any
// network write.
//
// The first is that the authorized new commit exists in the authorized upstream. It is what makes "the
// new commit really exists locally" mean something: the objects came from the repository being written
// to, so an approval can only publish content that repository already carries. A commit that arrived
// from somewhere else — a workspace, a build, a person's laptop — has no path to the remote through this
// executor, and that is deliberate: the only content channel this path has is the upstream itself.
//
// The second is that the authorized old commit is an ancestor of the new one. It is checked here, against
// objects this process holds, rather than inferred from the lease: the lease answers "is the remote still
// where I looked", and this answers "is the change I am authorized to make a fast-forward of it" — two
// different questions about two different repositories. A non-fast-forward rewrite is therefore refused
// even when the lease would have held, and the refusal is local, before the remote is told anything.
func (p *PushExecutor) verifyWriteIsExpressible(ctx context.Context, scratch string, job *PushJob) error {
	if _, stderr, err := runGit(ctx, p.config.GitPath, p.config.HomeDirectory,
		pushFactArguments("cat-file", "-e", job.NewCommit+"^{commit}"), scratch); err != nil {
		return fmt.Errorf("the authorized new commit is not in the authorized upstream: %s", strings.TrimSpace(stderr))
	}
	if _, stderr, err := runGit(ctx, p.config.GitPath, p.config.HomeDirectory,
		pushFactArguments("merge-base", "--is-ancestor", job.OldCommit, job.NewCommit), scratch); err != nil {
		return fmt.Errorf("the authorized old commit is not an ancestor of the new commit: %s", strings.TrimSpace(stderr))
	}
	if _, stderr, err := runGit(ctx, p.config.GitPath, p.config.HomeDirectory,
		pushFactArguments("update-ref", pushLocalRef, job.NewCommit), scratch); err != nil {
		return fmt.Errorf("the authorized commit could not be named locally: %s", strings.TrimSpace(stderr))
	}
	return nil
}

// update performs the one write and reports what it established about the remote.
//
// The classification is the execution authority's D7, read off Git's own porcelain report of what the
// remote answered:
//
//   - an update that was *sent* (' ' fast-forward, '+' forced update) is the only thing that proves this
//     attempt moved the ref. The compare-and-swap is what makes that a proof rather than a hope: the
//     remote applied the update only because the ref still held the authorized old commit.
//   - '=' means Git found the ref already holding the authorized commit and sent nothing. The target
//     state is reached and this attempt did not reach it — the "写入者不可归因" cell.
//   - '!' means the remote refused: nothing was sent. That is an explicit refusal, which D7 makes a
//     confirmed failure, and the read-only reconciliation below says what the ref holds now so that a
//     refusal caused by another writer's move to the *same* target is recorded as the target state
//     rather than as a failure.
//   - anything else — including a transport failure, a timeout, or output this package cannot read — is
//     `unknown`. It is deliberately not reconciled: a single read after a write whose fate is unknown
//     cannot distinguish "my request never arrived" from "my request arrived and a later writer moved the
//     ref", so a read would produce a fact that does not settle the question and would tempt a reader to
//     treat it as if it did.
func (p *PushExecutor) update(ctx context.Context, job *PushJob, address, identity, scratch, token string) (state PushState, observed string) {
	helper, err := helperArgument(p.config.HelperPath, p.config.MaterialRoot, identity)
	if err != nil {
		return PushUnknown, ""
	}
	arguments, err := pushUpdateArguments(helper, address, job.Ref, job.OldCommit, pushLocalRef, p.config.gitDeployment().transport)
	if err != nil {
		return PushUnknown, ""
	}
	// The exit status is deliberately not consulted: the report Git wrote is the outcome, and a Git that
	// failed before writing one is exactly the unreported case below, whose handling does not depend on
	// why it failed.
	stdout, stderr, _ := runGit(ctx, p.config.GitPath, p.config.HomeDirectory, arguments, scratch)
	report, reported := parsePushReport(stdout, job.Ref)
	if !reported {
		// A Git that failed before it could report, or that reported something this package does not
		// recognize, leaves the attempt's outcome unestablished. The remote's own error text goes to the
		// executor's log with the staged secret redacted, and never to the caller: it can carry the
		// upstream's words, and the caller's action is the same whatever it said.
		p.log.Warn("the push attempt produced no readable report",
			zap.String("operationId", job.OperationID),
			zap.String("ref", job.Ref),
			zap.Error(redactFailure(stderr, token)),
		)
		return PushUnknown, ""
	}
	switch report.Flag {
	case ' ', '+':
		return PushConfirmed, job.NewCommit
	case '=':
		return PushTargetSatisfied, job.NewCommit
	case '!':
		p.log.Warn("the remote refused the update",
			zap.String("operationId", job.OperationID),
			zap.String("ref", job.Ref),
			zap.String("summary", report.Summary),
		)
		return p.reconcile(ctx, job, address, identity, scratch, token)
	default:
		// '*' (a new ref) and '-' (a deletion) are spellings this path never asks for — the null object
		// id is refused before signing, so a create-from-nothing cannot be authorized, and the refspec
		// has no empty source. A remote that reports one is a remote this executor will not claim to
		// understand.
		p.log.Warn("the push attempt reported a status this path does not perform",
			zap.String("operationId", job.OperationID),
			zap.String("ref", job.Ref),
			zap.String("status", string(report.Flag)),
		)
		return PushUnknown, ""
	}
}

// reconcile reads the authorized ref once, without writing anything, and names the state its value
// implies.
//
// This is the whole of the read-only reconciliation: one `ls-remote` under the credential the attempt
// already spent, against the one ref the authorization names. It creates no write authorization, mints
// nothing, and performs no second push — and it cannot re-spend anything either, because the
// authorization is already consumed and the Store's record of that is what the credential was delivered
// against.
//
// Its answer distinguishes the three values D7 names. The old commit means no target update was observed:
// the ref is where this attempt found it, and a blind retry would be a second attempt at a write whose
// first attempt was already refused. The new commit means the target state is reached by someone — not
// necessarily by this approval, which is why it is reported as the unattributable cell and not as
// success. Anything else is a conflict. A read that fails, or that this package cannot parse, leaves the
// outcome exactly where the refusal left it: not established.
func (p *PushExecutor) reconcile(ctx context.Context, job *PushJob, address, identity, scratch, token string) (state PushState, observed string) {
	helper, err := helperArgument(p.config.HelperPath, p.config.MaterialRoot, identity)
	if err != nil {
		return PushUnknown, ""
	}
	arguments, err := pushRemoteRefArguments(helper, address, job.Ref, p.config.gitDeployment().transport)
	if err != nil {
		return PushUnknown, ""
	}
	stdout, stderr, err := runGit(ctx, p.config.GitPath, p.config.HomeDirectory, arguments, scratch)
	if err != nil {
		p.log.Warn("the authorized ref could not be read back after a refusal",
			zap.String("operationId", job.OperationID),
			zap.String("ref", job.Ref),
			zap.Error(redactFailure(stderr, token)),
		)
		return PushUnknown, ""
	}
	commit, ok := parseRemoteRef(stdout, job.Ref)
	if !ok {
		return PushUnknown, ""
	}
	switch commit {
	case job.NewCommit:
		return PushTargetSatisfied, commit
	case job.OldCommit:
		return PushNotApplied, commit
	default:
		// An empty value lands here too: a ref that no longer exists is neither the commit this attempt
		// found nor the one it was authorized to publish, so it is a third value and is recorded as such.
		return PushTargetConflict, commit
	}
}

// stage writes the token where the helper will read it, scoped to the repository's host and path.
//
// It is the clone path's staging with the push path's identity, and the scope is derived the same way:
// from the canonical repository the authorization named, never from anything a caller restated. The
// scope covers a read of the same repository as well as the write, which is correct rather than wide:
// the fetch this path performs is part of executing the authorized write, and a credential that could
// only write and not read could not perform it.
func (p *PushExecutor) stage(identity, token, repository string) error {
	host, path, err := repositoryScope(repository)
	if err != nil {
		return err
	}
	return credentialmaterial.Stage(p.config.MaterialRoot, identity, token, host, path)
}

// pushFactArguments is the closed configuration for one local Git command that only reads or writes this
// process's own scratch repository.
//
// It is narrower than `gitConstraintArguments` on purpose: a local determination never touches the
// network, so the protocol set is closed entirely and the credential machinery is not configured at all
// — the helper is not named, because a command that cannot dial a repository has nothing to
// authenticate to.
func pushFactArguments(arguments ...string) []string {
	return append([]string{
		"-c", "protocol.allow=never",
		"-c", "core.hooksPath=/dev/null",
		"-c", "core.askPass=",
	}, arguments...)
}

// matchPushBinding checks the authorization against the job before anything is spent.
//
// The purpose is checked first and in the direction that matters: this is the check that refuses a
// `clone` or a `mirror_prepare` authorization on the push route. It compares the signed operation rather
// than the route the request arrived on, so a dispatcher that sent another purpose's job here gets a
// refusal instead of a credential and instead of a write.
//
// The capability set is deliberately not compared, for the reason `matchMirrorBinding` gives:
// `Binding.validate` refuses any operation whose capability set is not exactly the one
// `permittedCapabilities` returns for it, and it runs on verification, so the set this function would
// compare is the set it already read.
func matchPushBinding(binding *cloneauth.Binding, job *PushJob, repository string) error {
	if binding.Operation != cloneauth.OperationPush {
		return fmt.Errorf("authorization permits %q, not a push", binding.Operation)
	}
	if binding.OperationID != job.OperationID {
		return errors.New("authorization is for a different operation")
	}
	if binding.ProjectID != job.ProjectID {
		return errors.New("authorization is for a different project")
	}
	if binding.WorkspaceID != job.WorkspaceID {
		return errors.New("authorization is for a different workspace")
	}
	if binding.Repository != repository {
		return errors.New("authorization is for a different repository")
	}
	if binding.Ref != job.Ref {
		return errors.New("authorization is for a different ref")
	}
	if binding.OldCommit != job.OldCommit || binding.NewCommit != job.NewCommit {
		return errors.New("authorization is for a different commit pair")
	}
	if binding.CredentialID != job.CredentialID || binding.CredentialVersion != job.CredentialVersion {
		return errors.New("authorization is for a different credential version")
	}
	return nil
}

// pushMaterialIdentity is the identity one push redemption's credential is staged under.
//
// It is the authorization's identifier, for the reason the mirror path uses the same one: an
// authorization is spent exactly once, so two attempts of one approval can never collide on one
// directory. The value must be a canonical UUID because the Store mints it as one and turns it into a
// file name of its own; the check is `credentialmaterial`'s, which fixes the form, because the value
// becomes a path segment here too.
func pushMaterialIdentity(authorizationID string) (string, error) {
	if !credentialmaterial.ValidExecutionID(authorizationID) {
		return "", errors.New("the authorization is not identified by a canonical uuid")
	}
	return authorizationID, nil
}

// refuse builds one coded failure so it is reported through the same Failure type the other two paths
// use. The three share the type because a caller has one place to read a coded refusal from and one
// mapping from code to HTTP status; only the codes differ.
func (p *PushExecutor) refuse(code string, cause error) error {
	return &Failure{Code: code, cause: cause}
}
