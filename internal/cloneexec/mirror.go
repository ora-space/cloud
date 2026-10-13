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
	"github.com/wanglongan587/cloud/internal/mirror"
)

// MirrorJob is one mirror preparation: Cloud's request that this executor read exactly one ref of
// exactly one upstream repository and publish it as one internal mirror.
//
// It is a type of its own rather than a `kind` on Job, and it is served on a route of its own, because
// the two are different subjects with different identities. A clone is one execution's checkout on one
// Node and is authorized by `clone`; a mirror preparation is one attempt of one Project Operation and
// is authorized by `mirror_prepare`. Carrying both in one shape would mean a single decoder deciding
// which identity fields are meaningful, and the operation boundary would be a branch inside C instead
// of a signature Cloud cannot forge (the 2026-10-13 ruling).
type MirrorJob struct {
	// OperationID names the Project Operation this preparation is a step of. It is the identity the
	// authorization binds, and it is also the name the fetch directory is derived from.
	OperationID string `json:"operationId"`
	// PreparationGeneration is the attempt Cloud is asking for. Only the attempt that is still current
	// may publish, so the number travels inside the signed authorization and is compared here rather
	// than trusted from the job.
	PreparationGeneration int64 `json:"preparationGeneration"`
	// ProjectID is the Project whose mirror this is. It is carried because a mirror's on-disk identity
	// includes it: two tenants may name the same upstream repository and the same commit, and the
	// project is what keeps their mirrors apart. It is compared against the authorization, so a job
	// dispatched under the wrong Project is a mismatch rather than a mirror published under a name
	// nobody signed for.
	ProjectID string `json:"projectId"`
	// Repository is the upstream repository to fetch, canonicalized here and compared against the
	// authorization.
	Repository string `json:"repository"`
	// Ref is the fully qualified ref to fetch, for example `refs/heads/main`. It is signed: the
	// published mirror is keyed by the commit this ref resolved to, and a reader compares that commit
	// against Cloud's rows, so the ref may not be chosen by whoever dispatches the job.
	Ref string `json:"ref"`
	// CredentialID and CredentialVersion name the material the authorization is for.
	CredentialID      string `json:"credentialId"`
	CredentialVersion int64  `json:"credentialVersion"`
	// RuntimeGeneration and ControlEpoch are the fencing values the preparation was admitted under.
	// They are the two halves of the clone's runtime binding that a mirror preparation has: it runs on
	// no Node, so there is no instance or incarnation to name, but it still must not be executed under
	// a runtime generation Cloud has moved past.
	RuntimeGeneration int64 `json:"runtimeGeneration"`
	ControlEpoch      int64 `json:"controlEpoch"`
	// Authorization is the Cloud-signed single-use authorization. It is a secret.
	Authorization string `json:"authorization"`
}

// MirrorResult is what a successful preparation produced.
//
// Every field is a fact Cloud cannot derive from what it sent, which is why all four are reported: the
// commit and the generation are the evidence a `ready` preparation has to carry, and the repository
// and ref are repeated so Cloud compares the published artifact against what it authorized instead of
// against what it asked for.
type MirrorResult struct {
	// Commit is the commit the pinned ref resolved to, read back out of the fetched repository.
	Commit string `json:"commit"`
	// Ref is the single ref the published mirror exposes.
	Ref string `json:"ref"`
	// Repository is the canonical upstream identity the mirror was fetched from.
	Repository string `json:"repository"`
	// Generation is the mirror store's generation for this key, counting its publications. It is what
	// lets Cloud tell the mirror it approved from a later republication under the same key.
	Generation int64 `json:"generation"`
	// CredentialVersion is the material version the fetch actually used.
	CredentialVersion int64 `json:"credentialVersion"`
}

// Reason codes this executor reports for a mirror preparation. They are the executor's own vocabulary,
// as the clone path's are, and they are separate from it because the two describe different failures:
// a caller has to tell "this deployment cannot prepare mirrors" from "this mirror could not be
// fetched" from "this mirror could not be published".
const (
	// ReasonCodeFetchFailed reports that Git could not materialize the authorized ref. A ref that no
	// longer resolves, an upstream that refused the credential, and a transport failure all arrive
	// here; the cause distinguishes them for the executor's log and the code does not, because Cloud's
	// action is the same for all three.
	ReasonCodeFetchFailed = "credential_executor_fetch_failed"
	// ReasonCodePublishFailed reports that the fetched content could not be published as a mirror. The
	// store's own cause travels with it: a size or volume limit, a withdrawal that makes the key
	// unpublishable, and an I/O failure are all failures of this attempt and not of the fetch.
	ReasonCodePublishFailed = "credential_executor_publish_failed"
)

// DefaultMirrorTimeout bounds one mirror preparation when the deployment configures none.
//
// It is longer than a clone's default because the work is larger: a clone may take a single branch's
// checkout from a cached upstream, while a preparation fetches the branch's history into a bare
// repository with nothing to reuse. The authorization's own expiry bounds it further, and the
// deployment's value is what a real upstream is measured against.
const DefaultMirrorTimeout = 10 * time.Minute

// MirrorConfig wires the mirror executor's deployment-fixed inputs.
//
// It repeats the clone executor's Git inputs rather than sharing one Config, because the two
// capabilities are separable in a deployment: a Node that only clones runs the clone executor alone,
// and one that only fills the internal Git Server runs this. What they must not differ in is the
// deployment they point at, which is what gitDeployment checks.
type MirrorConfig struct {
	// MaterialRoot is the directory credential material is staged under, under the mirror path's own
	// identities. It is the same directory the clone path uses: one C identity owns one materialization
	// root, and a second root would be a second place a token could outlive its job.
	MaterialRoot string
	// HomeDirectory is the executor's own empty directory, used as Git's HOME.
	HomeDirectory string
	// HelperPath is the absolute path of the deployment-fixed credential helper.
	HelperPath string
	// GitPath is the absolute path of the Git binary.
	GitPath string
	// SSLCAInfo is the optional absolute path of the certificate authority file Git verifies the
	// upstream's TLS certificate against. Empty means the system bundle.
	SSLCAInfo string
	// VerificationKeys is the deployment's authorization verification key set. A deployment that cannot
	// verify has no mirror executor at all.
	VerificationKeys []ed25519.PublicKey
	// Mirrors is the store the prepared mirror is published into. It is required: this executor's whole
	// product is a publication, and an executor without a store would fetch repositories it could do
	// nothing with.
	Mirrors *mirror.Store
	// Store redeems mirror authorizations. It is required for the same reason the clone path's is.
	Store MirrorRedemptionClient
	// Timeout bounds one preparation.
	Timeout time.Duration
	// Now supplies the clock. It is injected so the local expiry check is deterministic in tests; the
	// authoritative expiry decision belongs to the Store either way.
	Now func() time.Time
	// Log receives one line per preparation. It never receives a credential or an authorization.
	Log *zap.Logger
}

// MirrorExecutor prepares internal mirrors. It holds no per-job state.
type MirrorExecutor struct {
	config MirrorConfig
	// fetchRoot is where a fetch is staged before the key that will name it is known. It is derived
	// from the mirror store's root rather than configured, so the move into the store's staging
	// directory is a rename within one filesystem by construction rather than by a deployment
	// remembering to say so.
	fetchRoot string
	log       *zap.Logger
}

// NewMirrorExecutor validates the configuration and returns a ready executor.
//
// Every check is a deployment boundary and a refusal rather than a default, for the same reason the
// clone executor's are: this process redeems a credential and writes a volume another process reads
// from, and a deployment that established half of that boundary should not start.
//
//nolint:gocritic // By value on purpose: the deployment descriptor is copied into the executor, so a caller that keeps its Config and edits it cannot move a running executor's boundary afterwards.
func NewMirrorExecutor(cfg MirrorConfig) (*MirrorExecutor, error) {
	if err := cfg.gitDeployment().validate(); err != nil {
		return nil, err
	}
	if len(cfg.VerificationKeys) == 0 {
		return nil, errors.New("mirror executor requires an authorization verification key")
	}
	for _, key := range cfg.VerificationKeys {
		if len(key) != ed25519.PublicKeySize {
			return nil, errors.New("mirror executor verification key is not an Ed25519 public key")
		}
	}
	if cfg.Store == nil {
		return nil, errors.New("mirror executor requires a credential service client")
	}
	if cfg.Mirrors == nil {
		return nil, errors.New("mirror executor requires a mirror store")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultMirrorTimeout
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	log := cfg.Log
	if log == nil {
		log = zap.NewNop()
	}
	fetchRoot := filepath.Join(cfg.Mirrors.Root(), "fetch")
	if err := os.MkdirAll(fetchRoot, publishRootMode); err != nil {
		return nil, fmt.Errorf("create the mirror fetch root: %w", err)
	}
	if err := os.Chmod(fetchRoot, publishRootMode); err != nil {
		return nil, fmt.Errorf("fix the mirror fetch root mode: %w", err)
	}
	return &MirrorExecutor{config: cfg, fetchRoot: fetchRoot, log: log}, nil
}

// gitDeployment is the shared Git and credential-materialization environment this executor runs with.
//
//nolint:gocritic // Value receiver on purpose, for the reason Config's has one: the environment checked is the deployment snapshot `NewMirrorExecutor` was handed.
func (c MirrorConfig) gitDeployment() gitDeployment {
	return gitDeployment{
		materialRoot:  c.MaterialRoot,
		homeDirectory: c.HomeDirectory,
		helperPath:    c.HelperPath,
		gitPath:       c.GitPath,
		sslCAInfo:     c.SSLCAInfo,
	}
}

// publishRootMode is the mode of the directory a fetch is staged under, and of the staging directory
// itself.
//
// It is the same mode the mirror store creates its own directories with: a fetch holds private
// repository content only until it is published, but it holds it in plaintext under whatever
// credential fetched it, and a directory any other identity could traverse would be a second way to
// read an upstream repository this process was trusted with.
const publishRootMode os.FileMode = 0o700

// Sweep removes fetch directories left by a previous run.
//
// A fetch directory has no life of its own: it is created, filled, and either moved into the mirror
// store's staging area or discarded, all inside one call, and nothing outside that call names it. So
// anything under the fetch root at startup is debris from a process that died, and removing it is the
// whole repair. It runs before the first job, like the clone executor's material sweep, because it is
// the same failure: a crash leaves work on disk with no process left to finish or clean it up.
//
// It does not touch the mirror store itself. An abandoned *staging* directory there is reclaimed by
// the store's own sweep, which is the only code that can take the key lock that proves a writer is
// gone.
func (m *MirrorExecutor) Sweep() (int, error) {
	entries, err := os.ReadDir(m.fetchRoot)
	if err != nil {
		return 0, fmt.Errorf("read the mirror fetch root: %w", err)
	}
	removed := 0
	for _, entry := range entries {
		if err := os.RemoveAll(filepath.Join(m.fetchRoot, entry.Name())); err != nil {
			return removed, fmt.Errorf("remove the abandoned fetch %s: %w", entry.Name(), err)
		}
		removed++
	}
	return removed, nil
}

// Prepare performs one mirror preparation.
//
// The order is fixed by what may be spent and when, and the mirror purpose makes the first step load
// bearing in a way the clone's check is not: the operation is checked against the signed authorization
// *before* the Store is asked for anything, so a `clone` authorization presented here never reaches a
// credential and is never consumed. A clone authorization that arrived at this route and was spent
// would be a clone the Node's job then could not perform, and the two operations exist precisely so
// that neither can happen.
//
// After that the steps are the clone path's, with a repository instead of a checkout at the end:
// redeem the authorization as the `broker-resolver` identity, materialize the token where only this
// identity can read it, run one constrained fetch of the one signed ref, read the commit back out of
// what was fetched, publish it into the mirror store, and destroy the material on every way out.
//
//nolint:gocritic // By value on purpose: the job carries the bearer authorization, so the copy this method validates is the copy it spends and no caller can change the job mid-flight.
func (m *MirrorExecutor) Prepare(ctx context.Context, job MirrorJob) (result MirrorResult, err error) {
	repository, err := cloneauth.CanonicalRepository(job.Repository)
	if err != nil {
		return MirrorResult{}, m.refuse(ReasonCodeInvalidJob, err)
	}
	if !credentialmaterial.ValidExecutionID(job.OperationID) {
		return MirrorResult{}, m.refuse(ReasonCodeInvalidJob, errors.New("the job names no operation"))
	}
	if job.PreparationGeneration < 1 {
		return MirrorResult{}, m.refuse(ReasonCodeInvalidJob, errors.New("the job names no preparation attempt"))
	}
	if strings.TrimSpace(job.ProjectID) == "" {
		return MirrorResult{}, m.refuse(ReasonCodeInvalidJob, errors.New("the job names no project"))
	}
	if strings.TrimSpace(job.Authorization) == "" {
		return MirrorResult{}, m.refuse(ReasonCodeInvalidJob, errors.New("the job carries no authorization"))
	}
	if !validCredentialID(job.CredentialID) {
		return MirrorResult{}, m.refuse(ReasonCodeInvalidJob, errors.New("the job names no credential"))
	}
	// The ref is validated with the issuer's own rule rather than a second opinion about ref names: the
	// value the job carries is the value the authorization signed, so a ref this refuses is one no
	// authorization from this build could have carried, and the job is malformed rather than mismatched.
	if err := cloneauth.ValidateRef(job.Ref); err != nil {
		return MirrorResult{}, m.refuse(ReasonCodeInvalidJob, err)
	}
	// The local verification is not a substitute for the Store's, which decides on its own whether to
	// deliver a secret. It exists so that a job whose authority is already known to be unusable does not
	// reach the network, and so the purpose and the binding are checked before a credential is anywhere
	// near this call.
	binding, err := cloneauth.Verify(m.config.VerificationKeys, job.Authorization, m.config.Now().UTC())
	if err != nil {
		return MirrorResult{}, m.refuse(ReasonCodeAuthorizationRejected, err)
	}
	if err := matchMirrorBinding(&binding, &job, repository); err != nil {
		return MirrorResult{}, m.refuse(ReasonCodeBindingMismatch, err)
	}
	timeout := m.config.Timeout
	// The authorization's own expiry bounds the preparation, measured against the same clock `Verify`
	// judged it by. An expiry that has already passed yields an already-cancelled context, which is the
	// intended outcome: nothing below may spend an authorization this executor has just judged expired.
	if remaining := binding.ExpiresAt.Sub(m.config.Now()); remaining < timeout {
		timeout = remaining
	}
	prepareContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// The material is staged under the authorization's identifier rather than the operation's. An
	// authorization is spent exactly once, so two attempts of one operation can never stage material
	// under the same name even if Cloud has already retried while this call was still running.
	identity, err := mirrorMaterialIdentity(binding.AuthorizationID)
	if err != nil {
		return MirrorResult{}, m.refuse(ReasonCodeInvalidJob, err)
	}
	credential, err := m.config.Store.ResolveMirror(prepareContext, job.CredentialID, job.Authorization)
	if err != nil {
		return MirrorResult{}, m.refuse(ReasonCodeRedemptionFailed, err)
	}
	// The authorization is spent from here on. Every failure below is reported as a failure and nothing
	// below retries, because a retry would need a new authorization rather than this one.
	token := credential.Token
	defer func() { token = "" }()
	if err := m.stage(identity, token, repository); err != nil {
		return MirrorResult{}, m.refuse(ReasonCodeUnavailable, err)
	}
	// From this line the material exists, so its destruction is registered immediately and its outcome
	// is folded into the result rather than logged and forgotten. A preparation whose credential may
	// still be on disk is not a successful preparation.
	defer func() {
		if destroyErr := credentialmaterial.Destroy(m.config.MaterialRoot, identity); destroyErr != nil {
			result = MirrorResult{}
			err = m.refuse(ReasonCodeCleanupFailed, destroyErr)
		}
	}()
	// The fetch directory is created before the fetch and removed after it on every path. The deferred
	// removal is registered before the fetch rather than inside it so that a fetch which fails after
	// creating part of a repository cannot leave it behind.
	fetchDirectory, err := os.MkdirTemp(m.fetchRoot, job.OperationID+".")
	if err != nil {
		return MirrorResult{}, m.refuse(ReasonCodeUnavailable, fmt.Errorf("create the fetch directory: %w", err))
	}
	defer func() {
		if removeErr := os.RemoveAll(fetchDirectory); removeErr != nil {
			// The startup sweep removes whatever survives here, and a preparation that succeeded is not
			// made a failure by its empty scratch directory outliving it.
			m.log.Warn("a fetch directory could not be removed",
				zap.String("operationId", job.OperationID), zap.Error(removeErr))
		}
	}()
	// The address the fetch dials is the deployment's own spelling of the repository, not the canonical
	// identity the authorization binds. The two differ exactly in the decorations `CanonicalRepository`
	// removes — a trailing `.git`, a trailing slash — and a Git service is free to serve only one of
	// them: `git http-backend` answers `/repo.git` and 404s `/repo`. Dialing the canonical form would
	// therefore turn a correct binding into a fetch of a path that does not exist, which is what the
	// private acceptance upstream reported: `remote: 404 page not found`. The canonical form stays the
	// identity — the binding, the credential's scope and the mirror's key all use it — so the two
	// spellings are one repository with one identity and one address, and only the address is dialed.
	address := strings.TrimSpace(job.Repository)
	verification, err := m.fetch(prepareContext, &job, address, identity, fetchDirectory, token)
	if err != nil {
		return MirrorResult{}, m.refuse(ReasonCodeFetchFailed, err)
	}
	record, err := m.publish(prepareContext, &binding, verification, fetchDirectory)
	if err != nil {
		return MirrorResult{}, m.refuse(ReasonCodePublishFailed, err)
	}
	m.log.Info("mirror prepared",
		zap.String("operationId", job.OperationID),
		zap.String("projectId", job.ProjectID),
		zap.String("repository", repository),
		zap.String("commit", verification.Commit),
		zap.Int64("generation", record.Generation),
		zap.Int64("credentialVersion", credential.Version),
	)
	return MirrorResult{
		Commit:            verification.Commit,
		Ref:               verification.Ref,
		Repository:        repository,
		Generation:        record.Generation,
		CredentialVersion: credential.Version,
	}, nil
}

// stage writes the token where the helper will read it, scoped to the repository's host and path.
//
// It is the clone path's staging with the mirror path's identity, and the scope is derived the same
// way: from the canonical repository the authorization named, never from anything a caller restated.
func (m *MirrorExecutor) stage(identity, token, repository string) error {
	host, path, err := repositoryScope(repository)
	if err != nil {
		return err
	}
	return credentialmaterial.Stage(m.config.MaterialRoot, identity, token, host, path)
}

// matchMirrorBinding checks the authorization against the job before anything is spent.
//
// The purpose is checked first and in the direction that matters: this is the check that refuses a
// `clone` authorization on the mirror route. It is a comparison of the signed operation rather than of
// the route the request arrived on, so a dispatcher that sent a clone's job to this endpoint gets a
// refusal instead of a credential.
func matchMirrorBinding(binding *cloneauth.Binding, job *MirrorJob, repository string) error {
	if binding.Operation != cloneauth.OperationMirrorPrepare {
		return fmt.Errorf("authorization permits %q, not a mirror preparation", binding.Operation)
	}
	if binding.OperationID != job.OperationID {
		return errors.New("authorization is for a different operation")
	}
	if binding.PreparationGeneration != job.PreparationGeneration {
		return errors.New("authorization is for a different preparation attempt")
	}
	if binding.ProjectID != job.ProjectID {
		return errors.New("authorization is for a different project")
	}
	if binding.Repository != repository {
		return errors.New("authorization is for a different repository")
	}
	if binding.Ref != job.Ref {
		return errors.New("authorization is for a different ref")
	}
	if binding.CredentialID != job.CredentialID || binding.CredentialVersion != job.CredentialVersion {
		return errors.New("authorization is for a different credential version")
	}
	if binding.RuntimeGeneration != job.RuntimeGeneration || binding.ControlEpoch != job.ControlEpoch {
		return errors.New("authorization is for a different runtime generation")
	}
	// The capability set is deliberately not compared here, unlike the clone path's. A clone compares
	// it because the execution plan fixes the capability the execution relies on, and a job that asked
	// for one thing while the authorization permitted another would be a plan the executor did not
	// follow. A mirror preparation has no such second axis: `Binding.validate` refuses any operation
	// whose capability set is not exactly the one `permittedCapabilities` returns for it, and it runs on
	// verification, so the set this function would compare is the set it already read. A field whose
	// only reachable value is the one just enforced is not a check; it is a second place to change when
	// the vocabulary grows.
	return nil
}

// mirrorMaterialIdentity is the identity one mirror redemption's credential is staged under.
//
// It is the authorization's identifier, and the reason is the same one that makes it a good file name:
// an authorization is spent exactly once, so two attempts of one operation — including an attempt
// Cloud has already retried while this one was still running — can never collide on one directory. The
// value must be a canonical UUID because the Store mints it as one and turns it into a file name of
// its own; the check is `credentialmaterial`'s, which fixes the form, because the value becomes a path
// segment here too.
func mirrorMaterialIdentity(authorizationID string) (string, error) {
	if !credentialmaterial.ValidExecutionID(authorizationID) {
		return "", errors.New("the authorization is not identified by a canonical uuid")
	}
	return authorizationID, nil
}

// refuse builds one coded failure so it is reported through the same Failure type the clone path uses.
// The two paths share the type because a caller has one place to read a coded refusal from and one
// mapping from code to HTTP status; only the codes differ.
func (m *MirrorExecutor) refuse(code string, cause error) error {
	return &Failure{Code: code, cause: cause}
}
