package config

import (
	"crypto/ed25519"
	"crypto/tls"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/viper"

	"github.com/wanglongan587/cloud/internal/cloneauth"
	"github.com/wanglongan587/cloud/internal/cloneexec"
	"github.com/wanglongan587/cloud/internal/core"
	"github.com/wanglongan587/cloud/internal/logger"
	"github.com/wanglongan587/cloud/internal/mirror"
)

// CloneExecutorServiceConfig is the credentialed-clone executor's own configuration.
//
// It is loaded by `cmd/clone-executor` from its own file with its own `CLONE_EXECUTOR_` environment
// prefix, because the executor is a separate deployment unit with a separate identity, a separate
// volume, and a separate trust root. Nothing here is read by the Cloud server, and Cloud cannot
// inherit the executor's materialization root or helper path by configuring itself.
//
// The section is deliberately not optional. A deployment that runs this process at all has declared
// that it executes credentialed clones, and the decisions require such a deployment to establish the
// whole boundary or not start (D17): a partially configured executor would accept jobs and fail them
// one at a time, which reads as a flaky clone rather than as a misconfigured deployment.
type CloneExecutorServiceConfig struct {
	// Listen is the address the executor binds, for example `:8444`.
	Listen string `mapstructure:"listen"`
	// MaterialRoot is the directory credential material is staged under. The deployment creates it and
	// owns it as the credential-executing identity; the workload identity must not be able to traverse
	// it (D11).
	MaterialRoot string `mapstructure:"material_root"`
	// CheckoutRoot is where clones are written, and the handoff point to the workload. It must be a
	// different directory from MaterialRoot, and it is the one place the two identities are meant to
	// meet.
	CheckoutRoot string `mapstructure:"checkout_root"`
	// HomeDirectory is the executor's own empty directory, used as Git's HOME so Git finds no
	// configuration. The executor creates it if absent.
	HomeDirectory string `mapstructure:"home_directory"`
	// HelperPath is the absolute path of the deployment-fixed credential helper. It must be a program
	// the credential-executing identity can execute and the workload identity cannot write (D11).
	HelperPath string `mapstructure:"helper_path"`
	// GitPath is the absolute path of the Git binary the executor runs.
	GitPath string `mapstructure:"git_path"`
	// SSLCAInfo is the optional certificate authority file Git verifies the repository's TLS
	// certificate against. A deployment that serves Git from its own authority names it here; an empty
	// value means the system bundle.
	SSLCAInfo string `mapstructure:"ssl_ca_info"`
	// AuthorizationPublicKeyFile is the deployment-delivered Ed25519 public key Cloud signs
	// authorizations with. It is the public half only: an executor whose volume and process are both
	// compromised still cannot mint an authorization. The file may hold several keys, which is how the
	// deployment rotates Cloud's signing key without an outage.
	AuthorizationPublicKeyFile string `mapstructure:"authorization_public_key_file"`
	// CertificateFile, PrivateKeyFile and ClientCAFile are the executor's own server identity and the
	// authority the dispatcher's certificate must chain to.
	CertificateFile string `mapstructure:"certificate_file"`
	PrivateKeyFile  string `mapstructure:"private_key_file"`
	ClientCAFile    string `mapstructure:"client_ca_file"`
	// DispatcherIdentity is the single SPIFFE identity allowed to dispatch jobs. There is exactly one,
	// because the ability to dispatch is the ability to spend an authorization.
	DispatcherIdentity string `mapstructure:"dispatcher_identity"`
	// StoreEndpoint is the Credential Service's HTTPS base URL.
	StoreEndpoint string `mapstructure:"store_endpoint"`
	// StoreCertificateFile and StorePrivateKeyFile hold the executor's `broker-resolver` certificate.
	// It is the only identity the executor holds toward the Store, and the Store grants it `resolve`
	// alone.
	StoreCertificateFile string `mapstructure:"store_certificate_file"`
	StorePrivateKeyFile  string `mapstructure:"store_private_key_file"`
	// StoreCAFile is the authority that signs the Credential Service's server certificate.
	StoreCAFile string `mapstructure:"store_ca_file"`
	// MirrorRoot is the directory holding the mirror store this deployment publishes into — the same
	// volume the internal Git Server reads from. It is optional, and empty is the approved default: a
	// deployment that prepares no mirrors has no mirror endpoint at all, and the clone path is
	// unaffected either way.
	//
	// It is deliberately not defaulted to anything derived from the other roots. The mirror store
	// contains published content another process serves, and the materialization root contains only
	// secrets, so a deployment that let one be inferred from the other would be choosing which of the
	// two guarantees to lose without saying so; see the containment checks in MirrorExecutor.
	MirrorRoot string `mapstructure:"mirror_root"`
	// MirrorMaxBytes caps one published mirror. It is enforced at publication, by this process: the
	// writer is the only party that publishes, so this is the value that decides what may be stored.
	// The internal Git Server is configured with its own copy for its own accounting, and a deployment
	// that set them differently would be describing one volume with two budgets.
	MirrorMaxBytes int64 `mapstructure:"mirror_max_bytes"`
	// MirrorVolumeBytes caps the total published size across all mirrors.
	MirrorVolumeBytes int64 `mapstructure:"mirror_volume_bytes"`
	// MirrorGrace is how long a mirror stays after Cloud's references stop naming it. This process does
	// not sweep the store today; the value is carried because the store is opened here with the
	// deployment's own policy, so a later reconciliation pass wired into this command inherits the
	// configured grace rather than a zero that would reclaim immediately.
	MirrorGrace time.Duration `mapstructure:"mirror_grace"`
	// MirrorTimeout bounds one mirror preparation. Zero selects the executor's default.
	MirrorTimeout time.Duration `mapstructure:"mirror_timeout"`
	// PushRoot is the directory the push executor materializes an authorized upstream under. It is
	// optional, and empty is the approved default: a deployment that performs no controlled push has no
	// push endpoint at all, and the clone and mirror paths are unaffected either way.
	//
	// It is a directory of its own for a reason the other roots do not share. What lands here is a
	// plaintext copy of a private upstream, fetched with a credential the Store delivered, and the only
	// thing that removes it is the sweep — which removes every entry beneath the root it is given. A push
	// root nested inside the materialization root would therefore have its contents deleted on the first
	// restart, and the reverse nesting would put staged tokens inside the directory a private upstream is
	// written to. See PushExecutor for the containment checks.
	PushRoot string `mapstructure:"push_root"`
	// PushTimeout bounds one push. Zero selects the executor's default.
	//
	// It is bounded by the authorization's own lifetime rather than the other way round, which is why the
	// deployment should keep this at or below `push_authorization_lifetime` on Cloud's side: an
	// authorization that expires mid-push cuts off a write that was otherwise permitted, and the two
	// values live in different configuration files, so neither process can check the other's.
	PushTimeout time.Duration `mapstructure:"push_timeout"`
	// Timeout bounds one clone. Zero selects the executor's default.
	Timeout time.Duration `mapstructure:"timeout"`
	// Logger is the executor's own log destination.
	Logger logger.Config `mapstructure:"logger"`
}

// LoadCloneExecutorConfig reads the executor's own configuration file.
//
// Like the Credential Service, the executor is a separate deployment unit and gets its own loader and
// its own environment prefix: `CLONE_EXECUTOR_*` cannot be set by anything that configures Cloud.
func LoadCloneExecutorConfig(path string) (*CloneExecutorServiceConfig, error) {
	v := viper.New()
	if path != "" {
		v.SetConfigFile(path)
	} else {
		v.SetConfigName("clone-executor")
		v.SetConfigType("yaml")
		v.AddConfigPath("./configs")
		v.AddConfigPath("../configs")
		v.AddConfigPath(".")
	}
	v.SetEnvPrefix("CLONE_EXECUTOR")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()
	if err := BindEnvKeys(v, CloneExecutorServiceConfig{}); err != nil {
		return nil, err
	}
	// A missing file is not an error: every value may come from the environment, which is how a
	// container deployment supplies the paths it mounted.
	if err := v.ReadInConfig(); err != nil {
		var notFound viper.ConfigFileNotFoundError
		if !errors.As(err, &notFound) && path != "" {
			return nil, fmt.Errorf("clone executor configuration is unreadable")
		}
	}
	var cfg CloneExecutorServiceConfig
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("clone executor configuration is invalid: %w", err)
	}
	return &cfg, nil
}

// Executor builds the clone executor, including its redemption client toward the Credential Service.
//
// The order matters: the verification key and the Store client are both built here, so a deployment
// that mounted no verification key or cannot reach a well-formed Store endpoint refuses at startup
// rather than at the first job. That is the D17 rule — a deployment that declares credentialed clone
// establishes the boundary or does not start.
func (c *CloneExecutorServiceConfig) Executor() (*cloneexec.Executor, error) {
	if strings.TrimSpace(c.Listen) == "" {
		return nil, errors.New("clone executor listen address is required")
	}
	keys, err := cloneauth.LoadVerificationKeys(c.AuthorizationPublicKeyFile)
	if err != nil {
		return nil, fmt.Errorf("clone executor: %w", err)
	}
	storeClient, err := c.StoreClient()
	if err != nil {
		return nil, err
	}
	executor, err := cloneexec.New(cloneexec.Config{
		MaterialRoot:     c.MaterialRoot,
		CheckoutRoot:     c.CheckoutRoot,
		HomeDirectory:    c.HomeDirectory,
		HelperPath:       c.HelperPath,
		GitPath:          c.GitPath,
		SSLCAInfo:        c.SSLCAInfo,
		VerificationKeys: keys,
		Store:            storeClient,
		Timeout:          c.Timeout,
	})
	if err != nil {
		return nil, fmt.Errorf("clone executor: %w", err)
	}
	return executor, nil
}

// MirrorExecutor builds the mirror-preparation executor over the configured mirror store.
//
// An empty `mirror_root` yields `(nil, nil)`: the capability is simply absent, and the executor's
// mirror route answers `credential_executor_unavailable` rather than the deployment serving a store it
// was never given. That is the approved default, and it is the same shape CloneDispatchConfig uses —
// an absent capability is not an error, a half-declared one is.
//
// The containment checks are the reason this cannot be a plain field assignment. `credentialmaterial.Sweep`
// removes *every* entry under the materialization root, and the clone executor runs it at startup; a
// mirror root nested inside it would therefore be deleted on the first restart, published content and
// all, by a process that was only cleaning up after itself. The reverse nesting puts credential material
// inside the volume the internal Git Server serves. Both are refused here, where the failure can be
// reported as the deployment error it is.
func (c *CloneExecutorServiceConfig) MirrorExecutor() (*cloneexec.MirrorExecutor, error) {
	root := strings.TrimSpace(c.MirrorRoot)
	if root == "" {
		return nil, nil
	}
	for _, other := range []struct{ name, path string }{
		{"credential materialization root", c.MaterialRoot},
		{"checkout root", c.CheckoutRoot},
	} {
		if err := separateRoots(root, other.path); err != nil {
			return nil, fmt.Errorf("clone executor mirror root: %w; it must be a directory of its own, not one containing or contained by the %s", err, other.name)
		}
	}
	keys, err := cloneauth.LoadVerificationKeys(c.AuthorizationPublicKeyFile)
	if err != nil {
		return nil, fmt.Errorf("clone executor mirror: %w", err)
	}
	storeClient, err := c.StoreClient()
	if err != nil {
		return nil, err
	}
	mirrors, err := mirror.New(mirror.Config{
		Root:           root,
		MaxMirrorBytes: c.MirrorMaxBytes,
		MaxVolumeBytes: c.MirrorVolumeBytes,
		Grace:          c.MirrorGrace,
		Clock:          time.Now,
	})
	if err != nil {
		return nil, fmt.Errorf("clone executor mirror store: %w", err)
	}
	executor, err := cloneexec.NewMirrorExecutor(cloneexec.MirrorConfig{
		MaterialRoot:     c.MaterialRoot,
		HomeDirectory:    c.HomeDirectory,
		HelperPath:       c.HelperPath,
		GitPath:          c.GitPath,
		SSLCAInfo:        c.SSLCAInfo,
		VerificationKeys: keys,
		Mirrors:          mirrors,
		Store:            storeClient,
		Timeout:          c.MirrorTimeout,
	})
	if err != nil {
		return nil, fmt.Errorf("clone executor mirror: %w", err)
	}
	return executor, nil
}

// PushExecutor builds the controlled-push executor over a scratch root of its own.
//
// An empty `push_root` yields `(nil, nil)`: the capability is absent, and C's push route answers
// `credential_executor_unavailable` rather than the deployment performing writes it was never given a
// root for. That is the approved default and the same shape `MirrorExecutor` has — an absent capability
// is not an error, a half-declared one is.
//
// The containment checks matter more here than they do for the mirror root. A push root holds a
// plaintext copy of a private upstream, and the push executor's own sweep removes every entry beneath
// it: a root nested inside the mirror store would delete published content on the first restart, and one
// nested inside the checkout root would have a Workspace's clone deleted out from under it. The
// materialization root is checked for the reason the mirror root is — staged tokens must not live inside
// a directory a repository is written to, and vice versa.
func (c *CloneExecutorServiceConfig) PushExecutor() (*cloneexec.PushExecutor, error) {
	root := strings.TrimSpace(c.PushRoot)
	if root == "" {
		return nil, nil
	}
	for _, other := range []struct{ name, path string }{
		{"credential materialization root", c.MaterialRoot},
		{"checkout root", c.CheckoutRoot},
		{"mirror root", c.MirrorRoot},
	} {
		if err := separateRoots(root, other.path); err != nil {
			return nil, fmt.Errorf("clone executor push root: %w; it must be a directory of its own, not one containing or contained by the %s", err, other.name)
		}
	}
	keys, err := cloneauth.LoadVerificationKeys(c.AuthorizationPublicKeyFile)
	if err != nil {
		return nil, fmt.Errorf("clone executor push: %w", err)
	}
	storeClient, err := c.StoreClient()
	if err != nil {
		return nil, err
	}
	executor, err := cloneexec.NewPushExecutor(cloneexec.PushConfig{
		MaterialRoot:     c.MaterialRoot,
		ScratchRoot:      root,
		HomeDirectory:    c.HomeDirectory,
		HelperPath:       c.HelperPath,
		GitPath:          c.GitPath,
		SSLCAInfo:        c.SSLCAInfo,
		VerificationKeys: keys,
		Store:            storeClient,
		Timeout:          c.PushTimeout,
	})
	if err != nil {
		return nil, fmt.Errorf("clone executor push: %w", err)
	}
	return executor, nil
}

// separateRoots refuses two configured directories that contain one another.
//
// It is a containment check and not an equality check because of how each directory is swept:
// `credentialmaterial.Sweep` and the mirror executor's own fetch sweep both remove every entry beneath
// their root, so nesting one inside the other means one capability's routine cleanup deletes the
// other's content. An empty path is skipped rather than refused, because a missing root is reported by
// the builder that needs it, with a message about the root that is actually missing.
func separateRoots(one, other string) error {
	one, other = strings.TrimSpace(one), strings.TrimSpace(other)
	if one == "" || other == "" {
		return nil
	}
	for _, pair := range [][2]string{{one, other}, {other, one}} {
		rel, err := filepath.Rel(pair[0], pair[1])
		if err != nil {
			// Paths on different volumes, or one absolute and one relative: neither can contain the other
			// in a way this check has to reason about, and the builders reject a non-absolute root on their
			// own.
			continue
		}
		if rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
			return fmt.Errorf("%q and %q are not separate directories", pair[0], pair[1])
		}
	}
	return nil
}

// StoreClient builds the executor's `broker-resolver` client.
//
// It is a client certificate rather than a token, and the deployment's capability table gives that
// certificate `resolve` and nothing else. The executor therefore cannot write, rotate, revoke, or
// enumerate a credential even if it is fully compromised.
func (c *CloneExecutorServiceConfig) StoreClient() (*cloneexec.StoreClient, error) {
	cfg, _, err := loadMutualTLS(c.StoreCertificateFile, c.StorePrivateKeyFile, c.StoreCAFile)
	if err != nil {
		return nil, fmt.Errorf("clone executor credential service: %w", err)
	}
	client, err := cloneexec.NewStoreClient(cloneexec.StoreClientConfig{
		Endpoint: c.StoreEndpoint,
		TLS:      cfg,
	})
	if err != nil {
		return nil, fmt.Errorf("clone executor credential service: %w", err)
	}
	return client, nil
}

// ServerTLS builds the executor's listener configuration. It always requires and verifies a client
// certificate: an anonymous caller must never reach the identity comparison at all.
func (c *CloneExecutorServiceConfig) ServerTLS() (*tls.Config, error) {
	cfg, clientCAs, err := loadMutualTLS(c.CertificateFile, c.PrivateKeyFile, c.ClientCAFile)
	if err != nil {
		return nil, fmt.Errorf("clone executor: %w", err)
	}
	// The authority in `client_ca_file` verifies the executor's *peers*, which is `ClientCAs` rather
	// than `RootCAs`. The executor does make outbound connections, but they use the Store's authority
	// and are configured separately, so leaving this one unset would mean the listener accepted
	// anonymous callers.
	cfg.ClientCAs = clientCAs
	cfg.ClientAuth = tls.RequireAndVerifyClientCert
	return cfg, nil
}

// ExecutorDispatchConfig is Cloud's half of the credentialed-executor boundary: the executor it
// dispatches jobs to, the identity it authenticates as, and the key it signs authorizations with.
//
// It covers three capabilities, and the configuration section is shared by all of them because they are
// one deployment fact rather than three: a single executor process, reached at one endpoint, behind one
// dispatcher identity, verifying one signing key. Splitting this into separate sections would let a
// deployment point clone dispatch, mirror dispatch and push dispatch at different hosts, and then the
// statement "the mirror Cloud published was prepared by the component Cloud authorized" would stop being
// true without anything refusing.
//
// What is separate is which capabilities are *switched on*: `enabled` for credentialed clones,
// `mirror_enabled` for mirror preparation, and `push_enabled` for controlled pushes. Any may stand
// alone. The approved fail-closed state is all three off, which is the default: a deployment that
// configures no executor admits no credentialed clone at all (`requireRepositoryAccess` refuses every
// Project that names a credential with `repository_credential_executor_unavailable`), prepares no
// mirror, and refuses every push with `push_executor_unavailable`. Enabling a capability is the
// deployment declaring it has established that capability's whole boundary, which is the shape D17 gives
// the missing-configuration table.
//
// The mapstructure key remains `clone_dispatch`, and the environment prefix remains
// `CLOUD_CLONE_DISPATCH_*`. The deployment files and the compose environment already spell it that
// way, and widening a section is not a reason to invalidate them; the Go type is renamed because the
// name is what a reader of this package sees, and `CloneDispatchConfig` holding the mirror capability
// would be a name that lies about its contents.
type ExecutorDispatchConfig struct {
	// Enabled admits credentialed clones.
	Enabled bool `mapstructure:"enabled"`
	// Endpoint is the executor's HTTPS base URL.
	Endpoint string `mapstructure:"endpoint"`
	// CertificateFile and PrivateKeyFile hold Cloud's `clone-dispatcher` certificate. They are file
	// references, never inline material.
	CertificateFile string `mapstructure:"certificate_file"`
	PrivateKeyFile  string `mapstructure:"private_key_file"`
	// CAFile is the certificate authority that signs the executor's server certificate.
	CAFile string `mapstructure:"ca_file"`
	// SigningKeyFile is Cloud's Ed25519 private key for clone authorizations. It is the ability to
	// authorize, and it belongs to Cloud alone: the Store and the executor hold only the public half,
	// so neither can mint one.
	SigningKeyFile string `mapstructure:"signing_key_file"`
	// AuthorizationLifetime is how long one authorization lives. It is bounded by the decision's hard
	// ceiling and must not exceed one clone's timeout, because an authorization that expires mid-clone
	// fails a clone that was otherwise permitted.
	AuthorizationLifetime time.Duration `mapstructure:"authorization_lifetime"`
	// MirrorEnabled admits mirror preparation. It is separate from `Enabled` because the two
	// capabilities have different consequences: mirror preparation installs a worker that claims
	// Operations and can settle them with a failure, while credentialed clone dispatch is reachable only
	// from an Operation that already reached the clone step. A deployment can therefore run the mirror
	// path with the clone path still refused, which is what an incremental rollout of this boundary
	// needs — and, before the clone path's own acceptance evidence exists, what keeps
	// `repository_credential_executor_unavailable` the answer for every credentialed clone.
	MirrorEnabled bool `mapstructure:"mirror_enabled"`
	// ExecutorIdentity is the SPIFFE identity of the credential-executing component as the Credential
	// Service sees it, meaning the URI in the certificate that component presents when it redeems.
	//
	// Cloud states it here because the mirror authorization has to name the party permitted to spend
	// it, and the only value the Store will accept is the one it resolves from the verified client
	// certificate. The two spellings have to agree; `validExecutorIdentity` checks the shape, and a
	// well-formed value that names the wrong workload fails closed at redemption.
	ExecutorIdentity string `mapstructure:"executor_identity"`
	// MirrorAuthorizationLifetime is how long one mirror authorization lives.
	//
	// It is bounded by `core.MirrorPreparationLease`, which is the ceiling this differs from the clone
	// path by and the reason the lease is exported: an authorization outliving its attempt's lease
	// would let the executor fetch with material the worker has already reclaimed, and the generation
	// fence would then refuse the result it publishes. Keep the executor's `mirror_timeout` at or below
	// this value too, so a fetch is not cut off by the authorization midway — that is the executor's own
	// file, in a different process, so it cannot be checked from here.
	MirrorAuthorizationLifetime time.Duration `mapstructure:"mirror_authorization_lifetime"`
	// PushEnabled admits controlled pushes. It is separate from `Enabled` and `MirrorEnabled` because
	// the three capabilities have different consequences, and this one's is the largest: a push is the
	// only operation in this component that changes something outside the platform, in a repository the
	// platform does not own. A deployment can therefore run clones and preparations while refusing every
	// push, which is the approved state and the default.
	//
	// Turning it on does not by itself make a push possible. The Project has to bind a reference that
	// permits one, the approval path has to admit it, and C's own `push_root` has to be configured; this
	// switch only decides whether Cloud signs and dispatches an authorization at all.
	PushEnabled bool `mapstructure:"push_enabled"`
	// PushAuthorizationLifetime is how long one push authorization lives.
	//
	// It is the bound on the window `20261012-push-execution-authority` D6 describes as irreducible: the
	// interval between Cloud's pre-dispatch re-check and the Credential Service's redemption, during
	// which a revocation landing in between is not visible to both parties at once. The ceiling is
	// therefore `core.PushDispatchLifetime`, which is Cloud's own documented bound on that window, and a
	// deployment may only shorten it. A configuration that could widen it would be a deployment choosing
	// a larger revocation window without the decision that set the bound being revisited.
	PushAuthorizationLifetime time.Duration `mapstructure:"push_authorization_lifetime"`
	// Timeout bounds one dispatch. Zero selects the client's default.
	Timeout time.Duration `mapstructure:"timeout"`
}

// DefaultAuthorizationLifetime is the lifetime used when the deployment configures none. It is well
// under `cloneauth.MaxLifetime`, so the ceiling the decision sets is a backstop rather than the
// operating value.
const DefaultAuthorizationLifetime = 10 * time.Minute

// DefaultMirrorAuthorizationLifetime is the mirror lifetime used when the deployment configures none.
// It leaves a minute of margin under `core.MirrorPreparationLease`, so the lease — not a clock
// difference between this process and the executor's — is what ends an attempt that has stopped.
const DefaultMirrorAuthorizationLifetime = 4 * time.Minute

// ExecutorDispatch is what a configured section yields: one transport, one signing key, and the
// capabilities the deployment switched on.
//
// The client and the key are shared rather than copied per capability on purpose, so a caller cannot
// hold a clone dispatcher and a mirror dispatcher that authorize differently. The capability fields
// are nil when the deployment did not enable them, and nil is the whole meaning: it is what a caller
// checks instead of a boolean, so "not enabled" cannot be confused with "enabled but misconfigured" —
// the latter never returns.
type ExecutorDispatch struct {
	// Client is the mutual-TLS transport to the executor.
	Client *cloneexec.Client
	// SigningKey is the Ed25519 key every authorization from this deployment is signed with.
	SigningKey ed25519.PrivateKey
	// Clone is non-nil exactly when `enabled` is set.
	Clone *CloneCapability
	// Mirror is non-nil exactly when `mirror_enabled` is set.
	Mirror *MirrorCapability
	// Push is non-nil exactly when `push_enabled` is set.
	Push *PushCapability
}

// CloneCapability is the credentialed-clone half.
type CloneCapability struct {
	// Lifetime is the authorization lifetime, resolved to its default.
	Lifetime time.Duration
}

// MirrorCapability is the mirror-preparation half.
type MirrorCapability struct {
	// ExecutorIdentity is the workload permitted to redeem a mirror authorization.
	ExecutorIdentity string
	// Lifetime is the authorization lifetime, resolved to its default.
	Lifetime time.Duration
}

// PushCapability is the controlled-push half.
//
// It carries the executor identity for the mirror's reason — a push authorization names the party
// permitted to spend it, and the Store compares that against the identity it resolves from the verified
// client certificate — and its lifetime is capped rather than merely defaulted, because the lifetime is
// the width of the revocation window D6 records as irreducible.
type PushCapability struct {
	// ExecutorIdentity is the workload permitted to redeem a push authorization.
	ExecutorIdentity string
	// Lifetime is the authorization lifetime, resolved to its default and never above the bound.
	Lifetime time.Duration
}

// Open builds Cloud's dispatch transport, signing key, and enabled capabilities.
//
// A section with every capability off yields an `ExecutorDispatch` with no capabilities and no
// error, and builds nothing: an unconfigured deployment must not load a signing key or a certificate
// it has no use for, because doing so would make an absent capability able to fail startup. An
// enabled capability whose shared half is incomplete or unusable is an error, because a deployment
// must refuse to start a capability it has declared but cannot establish.
func (c *ExecutorDispatchConfig) Open() (*ExecutorDispatch, error) {
	if !c.Enabled && !c.MirrorEnabled && !c.PushEnabled {
		return &ExecutorDispatch{}, nil
	}
	if strings.TrimSpace(c.SigningKeyFile) == "" {
		return nil, errors.New("executor dispatch: an authorization signing key is required")
	}
	key, err := cloneauth.LoadSigningKey(c.SigningKeyFile)
	if err != nil {
		return nil, fmt.Errorf("executor dispatch: %w", err)
	}
	cfg, _, err := loadMutualTLS(c.CertificateFile, c.PrivateKeyFile, c.CAFile)
	if err != nil {
		return nil, fmt.Errorf("executor dispatch: %w", err)
	}
	client, err := cloneexec.NewClient(cloneexec.ClientConfig{
		Endpoint: c.Endpoint,
		TLS:      cfg,
		Timeout:  c.Timeout,
	})
	if err != nil {
		return nil, fmt.Errorf("executor dispatch: %w", err)
	}
	dispatch := &ExecutorDispatch{Client: client, SigningKey: key}
	if c.Enabled {
		lifetime := c.AuthorizationLifetime
		if lifetime <= 0 {
			lifetime = DefaultAuthorizationLifetime
		}
		if lifetime > cloneauth.MaxLifetime {
			return nil, fmt.Errorf("clone dispatch: authorization lifetime exceeds %s", cloneauth.MaxLifetime)
		}
		dispatch.Clone = &CloneCapability{Lifetime: lifetime}
	}
	if c.MirrorEnabled {
		if err := validExecutorIdentity(c.ExecutorIdentity); err != nil {
			return nil, fmt.Errorf("mirror dispatch: executor_identity %w", err)
		}
		lifetime := c.MirrorAuthorizationLifetime
		if lifetime <= 0 {
			lifetime = DefaultMirrorAuthorizationLifetime
		}
		// The lease, not `cloneauth.MaxLifetime`, is what bounds this: the decision's ceiling is looser
		// than the attempt it authorizes, so the tighter of the two is the one worth checking, and the
		// looser one is enforced by `Issue` regardless.
		if lifetime > core.MirrorPreparationLease {
			return nil, fmt.Errorf("mirror dispatch: authorization lifetime exceeds the preparation lease of %s", core.MirrorPreparationLease)
		}
		dispatch.Mirror = &MirrorCapability{ExecutorIdentity: c.ExecutorIdentity, Lifetime: lifetime}
	}
	if c.PushEnabled {
		if err := validExecutorIdentity(c.ExecutorIdentity); err != nil {
			return nil, fmt.Errorf("push dispatch: executor_identity %w", err)
		}
		lifetime := c.PushAuthorizationLifetime
		if lifetime <= 0 {
			lifetime = core.PushDispatchLifetime
		}
		// The bound is Cloud's own constant rather than `cloneauth.MaxLifetime`, and the direction is
		// deliberate: the lifetime is the width of the revocation window D6 records as irreducible, so a
		// deployment may narrow it and may not widen it. `Issue` enforces the issuer's ceiling either way.
		if lifetime > core.PushDispatchLifetime {
			return nil, fmt.Errorf("push dispatch: authorization lifetime exceeds the %s bound on the revocation window", core.PushDispatchLifetime)
		}
		dispatch.Push = &PushCapability{ExecutorIdentity: c.ExecutorIdentity, Lifetime: lifetime}
	}
	return dispatch, nil
}

// validExecutorIdentity checks that an executor identity names exactly one SPIFFE workload.
//
// The shape is the one the control listener and the executor already enforce on their own peer
// identities — `<trust domain>/<kind>/<name>` and nothing else — restated here because the value is
// this deployment's to write and a typo in it is otherwise only discovered at the first redemption.
// It is deliberately a shape check and not a lookup: nothing here can know which certificates the
// deployment's credential service was configured to accept, so the authority on whether this identity
// exists is the refusal the Store issues when it does not.
func validExecutorIdentity(identity string) error {
	uri, err := url.Parse(strings.TrimSpace(identity))
	if err != nil {
		return errors.New("is not a valid URI")
	}
	if uri.Scheme != "spiffe" || uri.Host == "" {
		return errors.New("is not a spiffe:// URI with a trust domain")
	}
	if uri.RawQuery != "" || uri.Fragment != "" || uri.User != nil {
		return errors.New("must not carry a query, fragment, or user info")
	}
	segments := strings.Split(strings.TrimPrefix(uri.Path, "/"), "/")
	if len(segments) != 2 || segments[0] == "" || segments[1] == "" {
		return errors.New("must name exactly one workload as /<kind>/<name>")
	}
	return nil
}
