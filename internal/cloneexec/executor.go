// Package cloneexec is the credentialed-clone executor: the component that turns one Cloud-issued
// single-use authorization into one Git clone of a private repository, and then makes the credential
// it used impossible to recover.
//
// # Why this is a component of its own
//
// The approved decisions fix three identities on the machine that runs a clone — M for management, W
// for the Agent workload, and C for credential execution — and require that the token exist only
// under C, in a directory W cannot traverse, for no longer than the clone
// (`20261010-controlled-node-git-authentication-isolation` D1, D7, D11). Those decisions place C on
// the controlled Node, alongside the workload it is kept away from. This package is C, built as a
// process of the current product with its own identity, its own volume, and its own listener, so the
// boundary is an operating-system boundary between two processes rather than a rule inside one.
//
// The component runs no Agent code and holds no Agent state. Its whole job is the five steps below,
// and it holds nothing between jobs.
//
// # The order is the contract
//
//  1. Authenticate the job locally against the deployment's verification key, and check every binding
//     this executor can check without spending anything: the execution, the repository, the runtime.
//     A job that fails here has consumed nothing.
//  2. Redeem the authorization at the Credential Service as the `broker-resolver` identity. This is
//     where the authorization is spent, and it is spent exactly once, by the Store, durably.
//  3. Materialize the token under the materialization root: one directory named for the execution,
//     mode 0700, holding one file of mode 0600.
//  4. Run one constrained `git clone` whose only credential helper is the deployment-fixed program
//     that reads that one file.
//  5. Destroy the material and prove it gone, on every path out of step 3 or 4 — success, Git
//     failure, timeout, cancellation, or a panic.
//
// Steps 3 to 5 are ordered so that the material's lifetime is inside the job's: it is created only
// after the Store has decided to deliver, and destroyed before the result is returned. A crash is the
// one case this ordering cannot cover, which is what the startup sweep is for.
//
// # What this package does not do
//
// It does not decide whether a clone is allowed. Scope, ownership, freezing, and the fail-closed
// refusal for a deployment with no executor all belong to Cloud, and by the time a job arrives here
// Cloud has already answered them. It does not hold the token after the job: nothing in this package
// stores, caches, or logs a credential. And it does not run anything the repository supplies — no
// hooks, no LFS filters, no submodules, no symlinks — because each of those is a program or a fetch
// that the authorization does not cover.
package cloneexec

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/wanglongan587/cloud/internal/cloneauth"
	"github.com/wanglongan587/cloud/internal/credentialmaterial"
)

// checkoutRootMode is the mode of the directory clones are written into.
//
// It is group- and world-readable because the checkout is the handoff: the clone exists so the
// workload can read it, and the deployment places this root on a volume the workload identity can
// reach. That is the deliberate asymmetry with the materialization root, which nothing but the
// credential-executing identity may traverse. The clone holds no secret — Git writes the credential
// nowhere into it — so the two must not share a permission story.
const checkoutRootMode os.FileMode = 0o755

// homeDirectoryMode is the mode of the executor's own home directory.
//
// Git is pointed at this directory as its `HOME` so that it finds no configuration. That only holds if
// the directory exists and is empty and is not writable by anyone but the credential-executing
// identity: a home another account could write to is a `.gitconfig` someone else can plant, and
// `GIT_CONFIG_GLOBAL=/dev/null` neutralizes only the file Git is told to read, not a directory Git
// might fall back to. The deployment creates it (`cluster/compose/clone-executor-setup.sh`) and `New`
// refuses a deployment whose home is missing or has the wrong mode.
const homeDirectoryMode os.FileMode = 0o700

// Runtime identifies the runtime a clone was admitted under.
//
// The values come from the job Cloud dispatched and are echoed back to the Credential Service, which
// compares them against the authorization's binding. They fence the authorization to the generation
// it was issued for; they are not an identity claim, because the executor proves who it is with its
// certificate.
type Runtime struct {
	NodeID            string `json:"nodeId"`
	NodeInstanceID    string `json:"nodeInstanceId"`
	NodeIncarnationID string `json:"nodeIncarnationId"`
	RuntimeGeneration int64  `json:"runtimeGeneration"`
	ControlEpoch      int64  `json:"controlEpoch"`
}

// Job is one credentialed clone to perform.
//
// The authorization travels in the job because it is the job's authority: it names the credential,
// the version, the repository, and the runtime, and it is what the Store spends. It is a bearer
// secret, so it is never logged, never echoed in a response, and never written anywhere by this
// package.
type Job struct {
	// ExecutionID is Cloud's clone execution identifier. It is both the materialization directory's
	// name and the binding the authorization carries, so the two are compared rather than assumed.
	ExecutionID string `json:"executionId"`
	// Repository is the repository to clone. It is canonicalized here and compared against the
	// authorization, so a job cannot clone a repository the authorization did not name.
	Repository string `json:"repository"`
	// Branch is the branch to check out. Empty means the repository's default branch.
	Branch string `json:"branch"`
	// Capabilities is the set of repository operations the job asks for, in canonical order. It is
	// carried so the executor compares a capability the caller states against the one the signed
	// authorization binds, rather than inferring what the job may do from the operation's name: the
	// plan is what decides, and a job whose set and authorization disagree is a mismatch rather than
	// an execution.
	Capabilities []string `json:"capabilities"`
	// CredentialID and CredentialVersion name the material the authorization is for. They are carried
	// so the redemption addresses the same credential the authorization binds.
	CredentialID      string `json:"credentialId"`
	CredentialVersion int64  `json:"credentialVersion"`
	// Authorization is the Cloud-signed single-use authorization. It is a secret.
	Authorization string `json:"authorization"`
	// Runtime is the runtime the clone was admitted under.
	Runtime Runtime `json:"runtime"`
}

// Result is what a successful clone produced.
type Result struct {
	// Directory is the absolute path of the checkout, which the deployment hands to the workload.
	Directory string
	// Commit is the revision that was checked out.
	Commit string
	// CredentialVersion is the material version the clone actually used, so the caller's record names
	// the version rather than re-deriving it from what it asked for.
	CredentialVersion int64
}

// Failure is a refusal this executor decided, with a stable reason code.
//
// Cloud maps the code to a public outcome, so the code is the whole message and the wrapped cause is
// for the executor's own log. Nothing derived from the credential or the authorization may appear in
// either.
type Failure struct {
	// Code is a stable reason code.
	Code string
	// cause is the underlying error. It is never sent to the caller.
	cause error
}

// Error reports the refusal by code only. The cause is available through Unwrap so the executor's log
// can record it, and it is deliberately absent from the message a caller might print.
func (f *Failure) Error() string { return "clone executor refused the job with " + f.Code }

// Unwrap exposes the cause for errors.Is and errors.As without putting it in Error().
func (f *Failure) Unwrap() error { return f.cause }

// ReasonCode names the refusal in a form Cloud can map to its own contract.
func (f *Failure) ReasonCode() string { return f.Code }

// Reason codes this executor reports. They are the executor's own vocabulary rather than the Store's,
// because a caller has to tell "this deployment cannot execute credentialed clones" apart from "this
// particular clone was refused" and from "the clone itself failed".
const (
	// ReasonCodeUnavailable reports that the deployment did not establish the boundary this executor
	// needs: no materialization root it can use, no helper, no verification key, no Store. It is the
	// fail-closed answer, and it is a deployment fault rather than a decision about the clone.
	ReasonCodeUnavailable = "credential_executor_unavailable"
	// ReasonCodeInvalidJob reports a job that is not well formed.
	ReasonCodeInvalidJob = "credential_executor_invalid_job"
	// ReasonCodeBindingMismatch reports a job whose repository, execution, or runtime does not match
	// the authorization it carries.
	ReasonCodeBindingMismatch = "credential_executor_binding_mismatch"
	// ReasonCodeAuthorizationRejected reports an authorization this executor will not act on. The
	// cause distinguishes malformed, expired, and unverifiable for the log; the code does not, because
	// telling a caller which of the three it sent would help it iterate on a forgery.
	ReasonCodeAuthorizationRejected = "credential_executor_authorization_rejected"
	// ReasonCodeRedemptionFailed reports that the Credential Service refused to deliver the secret.
	// The Store's own code is preserved in the cause.
	ReasonCodeRedemptionFailed = "credential_executor_redemption_failed"
	// ReasonCodeCloneFailed reports that Git could not complete the clone.
	ReasonCodeCloneFailed = "credential_executor_clone_failed"
	// ReasonCodeCleanupFailed reports that the credential material could not be proven destroyed. On the
	// clone and mirror paths it is a refusal, because a checkout or a published mirror whose credential
	// may still be on disk is not work Cloud may act on as done. The push path reports it in
	// `PushResult.CleanupFailure` instead, beside the verdict rather than in place of it: see the
	// deferred destroy in `PushExecutor.Push` for why a write's conclusion may not be replaced by it.
	ReasonCodeCleanupFailed = "credential_executor_cleanup_failed"
)

// Config wires the executor's deployment-fixed inputs.
type Config struct {
	// MaterialRoot is the directory credential material is staged under. The deployment creates it,
	// owns it as the credential-executing identity, and makes it untraversable by the workload.
	MaterialRoot string
	// CheckoutRoot is the directory clones are written into, and the handoff point to the workload.
	CheckoutRoot string
	// HomeDirectory is the executor's own empty directory, used as Git's HOME.
	HomeDirectory string
	// HelperPath is the absolute path of the deployment-fixed credential helper. It must be a path
	// the executor can execute and the workload cannot write.
	HelperPath string
	// GitPath is the absolute path of the Git binary.
	GitPath string
	// SSLCAInfo is the optional absolute path of the certificate authority file Git verifies the
	// repository's TLS certificate against. Empty means the system bundle. It is a deployment-fixed
	// path like every other input here, so a repository cannot point the clone at a trust anchor of its
	// own choosing.
	SSLCAInfo string
	// Egress is the deployment's optional, default-off proxy for Git's own egress, scoped to the hosts
	// it names. The zero value is the approved default and routes nothing through a proxy; with it
	// unset the command line is byte-for-byte the one that clears every proxy (D17's boundary is
	// unchanged, and see GitEgress for the recorded deviation).
	Egress GitEgress
	// VerificationKeys is the deployment's authorization verification key set. It is the public half of
	// Cloud's signing key, so this executor can refuse a job before spending anything — and so a
	// deployment that cannot verify has no executor at all.
	VerificationKeys []ed25519.PublicKey
	// Store redeems authorizations. It is required: an executor that cannot obtain a credential cannot
	// clone a private repository, and pretending otherwise would be the silent fallback the decisions
	// forbid.
	Store RedemptionClient
	// Timeout bounds one clone. It is also the ceiling a caller should keep authorizations under, since
	// an authorization that expires mid-clone fails a clone that was otherwise permitted.
	Timeout time.Duration
	// Now supplies the clock. It is injected so the local expiry check is deterministic in tests; the
	// authoritative expiry decision belongs to the Store either way.
	Now func() time.Time
	// Log receives one line per job. It never receives a credential or an authorization.
	Log *zap.Logger
}

// DefaultCloneTimeout bounds one credentialed clone when the deployment configures none.
const DefaultCloneTimeout = 5 * time.Minute

// Executor performs credentialed clones. It holds no per-job state.
type Executor struct {
	config Config
	log    *zap.Logger
}

// New validates the configuration and returns a ready executor.
//
// Every check here is a deployment boundary rather than input validation, and each one is a refusal
// rather than a default. The decisions require a deployment that declares credentialed clone to
// establish the whole boundary or not start at all (D17): an executor missing its helper, its
// verification key, or its Store would still accept jobs and fail them one at a time, which looks
// like a flaky clone rather than a misconfigured deployment.
//
//nolint:gocritic // By value on purpose: the deployment descriptor is copied into the executor, so a caller that keeps its Config and edits it cannot move a running executor's boundary afterwards.
func New(cfg Config) (*Executor, error) {
	if err := cfg.gitDeployment().validate(); err != nil {
		return nil, err
	}
	if err := validateExecutablePath(cfg.CheckoutRoot); err != nil {
		return nil, fmt.Errorf("checkout root: %w", err)
	}
	if len(cfg.VerificationKeys) == 0 {
		return nil, errors.New("credential executor requires an authorization verification key")
	}
	for _, key := range cfg.VerificationKeys {
		if len(key) != ed25519.PublicKeySize {
			return nil, errors.New("credential executor verification key is not an Ed25519 public key")
		}
	}
	if cfg.Store == nil {
		return nil, errors.New("credential executor requires a credential service client")
	}
	if cfg.MaterialRoot == cfg.CheckoutRoot {
		// One directory cannot be both untraversable by the workload and the workload's handoff
		// point; a deployment that set both to one path would have chosen which of the two guarantees
		// to lose without saying so.
		return nil, errors.New("credential materialization root must not be the checkout root")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultCloneTimeout
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	log := cfg.Log
	if log == nil {
		log = zap.NewNop()
	}
	return &Executor{config: cfg, log: log}, nil
}

// gitDeployment is the shared Git and credential-materialization environment this executor runs with.
//
//nolint:gocritic // Value receiver on purpose: it is the same receiver `New` has, so the environment validated here is a snapshot of the deployment rather than a live view a caller could edit afterwards.
func (c Config) gitDeployment() gitDeployment {
	return gitDeployment{
		materialRoot:  c.MaterialRoot,
		homeDirectory: c.HomeDirectory,
		helperPath:    c.HelperPath,
		gitPath:       c.GitPath,
		transport:     gitTransport{sslCAInfo: c.SSLCAInfo, egress: c.Egress},
	}
}

// Sweep removes material left by a previous run.
//
// It is a separate call rather than something New does, because it is destructive and the command
// should be able to decide when it runs — but it must run before the first job, and the command does
// exactly that. The failure it repairs is the one the job's own cleanup cannot: a crash or a killed
// container leaves a token on disk with no process left to destroy it, and only the next start can.
func (e *Executor) Sweep() (int, error) {
	return credentialmaterial.Sweep(e.config.MaterialRoot)
}

// Execute performs one credentialed clone.
//
// The material's lifetime is tied to this call by a deferred destruction that runs on every path,
// including the panicking one: the deferred function is registered before the material exists and
// reports failure if it cannot prove the material gone. A clone that succeeded but whose credential
// could not be destroyed is reported as a failure, because the caller must not treat a job whose
// secret may still be readable as done.
//
//nolint:gocritic // By value on purpose: the job carries the bearer authorization, so the copy this method validates is the copy it spends and no caller can change the job mid-flight.
func (e *Executor) Execute(ctx context.Context, job Job) (result Result, err error) {
	repository, err := cloneauth.CanonicalRepository(job.Repository)
	if err != nil {
		return Result{}, e.refuse(ReasonCodeInvalidJob, err)
	}
	if !credentialmaterial.ValidExecutionID(job.ExecutionID) {
		return Result{}, e.refuse(ReasonCodeInvalidJob, credentialmaterial.ErrInvalidExecutionID)
	}
	if strings.TrimSpace(job.Authorization) == "" {
		return Result{}, e.refuse(ReasonCodeInvalidJob, errors.New("the job carries no authorization"))
	}
	if !validCredentialID(job.CredentialID) {
		return Result{}, e.refuse(ReasonCodeInvalidJob, errors.New("the job names no credential"))
	}
	// The capability set is checked for well-formedness before the authorization is even verified,
	// because it is the job's own claim about what it wants to do. A set that is empty, unknown,
	// repeated or out of order is not a request this executor can compare against anything, so it is
	// refused as a malformed job rather than as a binding mismatch.
	capabilities, err := cloneauth.CanonicalCapabilities(job.Capabilities)
	if err != nil {
		return Result{}, e.refuse(ReasonCodeInvalidJob, err)
	}
	// A credentialed clone reads one repository. Requesting anything else is refused here rather than
	// left to the binding comparison, so that a job asking for an authority this path does not
	// implement is a malformed job and not merely a mismatch with whatever the authorization happened
	// to carry. This is the executor's half of the plan: the authorization says what it permits, and
	// this says what the execution actually needs, and both have to name the same one thing.
	if !cloneauth.HasCapability(capabilities, cloneauth.CapabilityClone) {
		return Result{}, e.refuse(ReasonCodeInvalidJob, fmt.Errorf("a credentialed clone requires the %q capability", cloneauth.CapabilityClone))
	}
	// The local verification is not a substitute for the Store's. The Store verifies the authorization
	// itself, with its own clock, because it is the party that decides whether to deliver a secret
	// (the Store decision's D3). This check exists so that a job whose authority is already known to be
	// unusable does not reach the network, and so that the binding is checked against the job before a
	// credential is anywhere near it.
	binding, err := cloneauth.Verify(e.config.VerificationKeys, job.Authorization, e.config.Now().UTC())
	if err != nil {
		return Result{}, e.refuse(ReasonCodeAuthorizationRejected, err)
	}
	if err := matchBinding(&binding, &job, repository); err != nil {
		return Result{}, e.refuse(ReasonCodeBindingMismatch, err)
	}
	timeout := e.config.Timeout
	// The authorization's own expiry bounds the clone as well. Cloning past it would let a clone that
	// outlived its authority keep running on a credential the deployment has already stopped
	// considering valid.
	//
	// The remaining lifetime is measured against the executor's own clock, which is the same clock
	// `Verify` decided the authorization was still valid on, and not against the wall clock: two
	// readings of "now" in one decision could otherwise disagree, and a deployment that injects a
	// clock would bound the clone by a different instant than the one it authenticated the job with.
	// An expiry that has already passed yields a negative timeout, and `context.WithTimeout` then
	// hands back an already-cancelled context — which is the intended outcome, because nothing below
	// may spend an authorization the executor has just judged expired.
	if remaining := binding.ExpiresAt.Sub(e.config.Now()); remaining < timeout {
		timeout = remaining
	}
	cloneContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	credential, err := e.config.Store.Resolve(cloneContext, job.CredentialID, job.Authorization, job.Runtime)
	if err != nil {
		return Result{}, e.refuse(ReasonCodeRedemptionFailed, err)
	}
	// The authorization is spent from here on. Every failure below has to be reported as a failure and
	// cannot be retried with the same authorization, which is why nothing below retries.
	token := credential.Token
	defer func() { token = "" }()
	if err := e.stage(job.ExecutionID, token, repository); err != nil {
		return Result{}, e.refuse(ReasonCodeUnavailable, err)
	}
	// From this line the material exists, so the destruction is registered immediately and its outcome
	// is folded into the result rather than logged and forgotten.
	defer func() {
		if destroyErr := credentialmaterial.Destroy(e.config.MaterialRoot, job.ExecutionID); destroyErr != nil {
			result = Result{}
			err = e.refuse(ReasonCodeCleanupFailed, destroyErr)
		}
	}()
	directory, err := e.clone(cloneContext, &job, repository, token)
	if err != nil {
		return Result{}, err
	}
	commit, err := headCommit(cloneContext, e.config.GitPath, e.config.HomeDirectory, directory)
	if err != nil {
		e.log.Warn("credentialed clone produced no readable revision", zap.String("executionId", job.ExecutionID), zap.Error(err))
		return Result{}, e.refuse(ReasonCodeCloneFailed, err)
	}
	e.log.Info("credentialed clone completed",
		zap.String("executionId", job.ExecutionID),
		zap.String("repository", repository),
		zap.Int64("credentialVersion", credential.Version),
	)
	return Result{Directory: directory, Commit: commit, CredentialVersion: credential.Version}, nil
}

// stage writes the token where the helper will read it, scoped to the repository's host and path.
//
// The host and the path come from the canonical repository rather than from the job, so the scope is a
// function of the repository the authorization named and not of anything a caller could restate. The
// helper compares both against Git's credential request, which is what stops a token staged for one
// repository from answering for another on the same host.
func (e *Executor) stage(executionID, token, repository string) error {
	host, path, err := repositoryScope(repository)
	if err != nil {
		return err
	}
	return credentialmaterial.Stage(e.config.MaterialRoot, executionID, token, host, path)
}

// clone runs the constrained clone and returns the checkout directory.
//
// The target is derived from the execution id inside the checkout root, so a job cannot choose where
// its checkout lands. A target that already exists is refused rather than reused: a leftover
// directory is either a previous run's failure or another execution's clone, and neither is a safe
// place to write.
func (e *Executor) clone(ctx context.Context, job *Job, repository, token string) (string, error) {
	root, err := prepareCheckoutRoot(e.config.CheckoutRoot)
	if err != nil {
		return "", e.refuse(ReasonCodeUnavailable, err)
	}
	target := filepath.Join(root, job.ExecutionID)
	if _, err := os.Lstat(target); err == nil {
		return "", e.refuse(ReasonCodeInvalidJob, errors.New("the checkout target already exists"))
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", e.refuse(ReasonCodeUnavailable, err)
	}
	arguments, err := cloneArguments(job, e.config.HelperPath, e.config.MaterialRoot, target, e.config.gitDeployment().transport)
	if err != nil {
		return "", e.refuse(ReasonCodeUnavailable, err)
	}
	if _, stderr, err := runGit(ctx, e.config.GitPath, e.config.HomeDirectory, arguments, root); err != nil {
		// A failed clone may still have created the target. Removing it keeps the checkout root from
		// filling with partial trees, and it removes no credential: Git never writes the token into the
		// checkout.
		if removeErr := os.RemoveAll(target); removeErr != nil {
			e.log.Warn("failed clone left its target behind", zap.String("executionId", job.ExecutionID), zap.Error(removeErr))
		}
		// Git's own message is the only diagnosable part of a failed clone, and it is redacted against
		// the exact secret staged for this job before it becomes a failure the executor logs.
		return "", e.refuse(ReasonCodeCloneFailed, redactFailure(stderr, token))
	}
	return target, nil
}

// matchBinding checks the authorization against the job before anything is spent.
func matchBinding(binding *cloneauth.Binding, job *Job, repository string) error {
	if binding.Operation != cloneauth.OperationClone {
		return fmt.Errorf("authorization permits %q, not a clone", binding.Operation)
	}
	if binding.ExecutionID != job.ExecutionID {
		return errors.New("authorization is for a different execution")
	}
	if binding.Repository != repository {
		return errors.New("authorization is for a different repository")
	}
	if binding.CredentialID != job.CredentialID || binding.CredentialVersion != job.CredentialVersion {
		return errors.New("authorization is for a different credential version")
	}
	// The capability set is compared rather than assumed from the operation name. Both sides are
	// already canonical — the issuer canonicalizes before signing and the job was canonicalized in
	// Execute — so an inequality is a real difference in what was authorized, not a spelling
	// difference. `operation` alone would not catch it: an operation permitted by two capabilities
	// would let a job ask for one and be served under the other.
	//
	// Within the V1 vocabulary the two sides cannot differ: a clone permits exactly the `clone`
	// capability on both issue and verify, and a job that does not ask for it is refused as malformed
	// before it reaches here. The comparison is kept because it is what makes the binding meaningful
	// rather than decorative — the moment the vocabulary has a second member, this is the line that
	// decides — and it is deliberately not covered by a test with a case it cannot reach.
	if !slices.Equal(binding.Capabilities, job.Capabilities) {
		return errors.New("authorization permits different capabilities than the job asks for")
	}
	if binding.NodeID != job.Runtime.NodeID ||
		binding.NodeInstanceID != job.Runtime.NodeInstanceID ||
		binding.NodeIncarnationID != job.Runtime.NodeIncarnationID ||
		binding.RuntimeGeneration != job.Runtime.RuntimeGeneration ||
		binding.ControlEpoch != job.Runtime.ControlEpoch {
		return errors.New("authorization is for a different runtime")
	}
	return nil
}

// refuse builds one coded failure and records the cause for the executor's own log.
func (e *Executor) refuse(code string, cause error) error {
	return &Failure{Code: code, cause: cause}
}

// validCredentialID accepts only a canonical lowercase UUID, matching what the Store will accept as a
// path segment. Checking it here turns a malformed identifier into a refusal before a request is
// built from it.
func validCredentialID(id string) bool {
	if len(id) != 36 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
				return false
			}
		}
	}
	return true
}
