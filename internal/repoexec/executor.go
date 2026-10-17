package repoexec

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/wanglongan587/cloud/internal/cloneauth"
	"github.com/wanglongan587/cloud/internal/credentialmaterial"
)

// errTimeout is the cancellation cause when a task's own budget ends.
var errTimeout = errors.New("task timeout")

// Credential is a redeemed plaintext credential. String redacts it so an accidental log statement
// cannot print it.
type Credential struct {
	Username string
	Token    string
	Version  int64
}

// String redacts the credential.
func (c Credential) String() string {
	return fmt.Sprintf("repoexec.Credential{Version: %d, [redacted]}", c.Version)
}

// GoString redacts the credential for %#v.
func (c Credential) GoString() string { return c.String() }

// Redeemer exchanges an authorization for its credential exactly once (the Credential Store's
// redemption listener). The executor calls it only for a task that names a credential and only after
// the authorization verified and matched.
type Redeemer interface {
	Redeem(ctx context.Context, authorization string) (Credential, error)
}

// RedeemFunc adapts a function to Redeemer.
type RedeemFunc func(ctx context.Context, authorization string) (Credential, error)

// Redeem calls f.
func (f RedeemFunc) Redeem(ctx context.Context, authorization string) (Credential, error) {
	return f(ctx, authorization)
}

// Config wires one executor. Every path is deployment configuration, never task input.
type Config struct {
	// PublicKeys verify Cloud's execution authorizations (a set, for rotation).
	PublicKeys []ed25519.PublicKey
	Redeemer   Redeemer
	Egress     *Egress
	// GitServerURL is the Git Server's service repository listener, e.g. https://ora-git-service:8447.
	GitServerURL string
	// GitServerCAFile verifies the Git Server; CertFile/KeyFile are the executor's service
	// certificate, presented to the Git Server only.
	GitServerCAFile, CertFile, KeyFile string
	// ExternalCAFile, when set, replaces the system trust for external repositories. It exists for
	// acceptance fixtures with their own CA; production leaves it empty.
	ExternalCAFile string
	// WorkRoot holds task directories and staged credentials (tmpfs in deployment).
	WorkRoot string
	// HelperPath is the absolute git-credential-ora binary.
	HelperPath string
	// GitProgram is the git executable; it is resolved to an absolute path.
	GitProgram string
	// MaxTaskBytes bounds one task directory.
	MaxTaskBytes int64
	Limits       Limits
	// Now is the verification clock; nil means time.Now.
	Now    func() time.Time
	Logger *zap.Logger
}

// Executor runs tasks. It is safe for concurrent use; capacity is the Server's concern.
type Executor struct {
	keys      []ed25519.PublicKey
	redeemer  Redeemer
	egress    *Egress
	gitServer gitServerAccess
	external  string
	workRoot  string
	helper    string
	program   string
	maxBytes  int64
	limits    Limits
	now       func() time.Time
	log       *zap.Logger
	probe     *http.Client
}

// NewExecutor validates the configuration, prepares the work root and sweeps whatever a previous
// process left in it. It must run before the task listener accepts anything.
func NewExecutor(cfg *Config) (*Executor, error) {
	if cfg == nil || len(cfg.PublicKeys) == 0 || cfg.Redeemer == nil || cfg.Egress == nil {
		return nil, errors.New("repo executor needs authorization keys, a redeemer and an egress policy")
	}
	base, err := url.Parse(cfg.GitServerURL)
	if err != nil || base.Scheme != "https" || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" || strings.Trim(base.Path, "/") != "" {
		return nil, errors.New("git server URL must be an https origin")
	}
	for _, f := range []struct{ name, path string }{{"git server CA", cfg.GitServerCAFile}, {"service certificate", cfg.CertFile}, {"service key", cfg.KeyFile}} {
		if _, err := os.Stat(f.path); err != nil || !filepath.IsAbs(f.path) {
			return nil, fmt.Errorf("%s file must be an absolute path to an existing file", f.name)
		}
	}
	if cfg.MaxTaskBytes <= 0 || cfg.Limits.Sync <= 0 || cfg.Limits.Publish <= 0 || cfg.Limits.Probe <= 0 {
		return nil, errors.New("task size and timeout limits must be positive")
	}
	if !filepath.IsAbs(cfg.HelperPath) || !shellSafe(cfg.HelperPath) {
		return nil, errors.New("credential helper path must be absolute and shell-safe")
	}
	if info, err := os.Stat(cfg.HelperPath); err != nil || info.IsDir() || info.Mode().Perm()&0o111 == 0 {
		return nil, errors.New("credential helper is not an executable file")
	}
	program, err := exec.LookPath(cfg.GitProgram)
	if err != nil {
		return nil, errors.New("git program unavailable")
	}
	if program, err = filepath.Abs(program); err != nil {
		return nil, errors.New("git program unavailable")
	}
	external, err := externalTrust(cfg.ExternalCAFile)
	if err != nil {
		return nil, err
	}
	if err := prepareWorkRoot(cfg.WorkRoot, func(root string) error {
		_, err := credentialmaterial.Sweep(root)
		return err
	}); err != nil {
		return nil, err
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	log := cfg.Logger
	if log == nil {
		log = zap.NewNop()
	}
	e := &Executor{
		keys: cfg.PublicKeys, redeemer: cfg.Redeemer, egress: cfg.Egress,
		gitServer: gitServerAccess{base: strings.TrimSuffix(base.String(), "/") + "/", cert: cfg.CertFile, key: cfg.KeyFile, certificate: cfg.GitServerCAFile},
		external:  cfg.ExternalCAFile, workRoot: cfg.WorkRoot, helper: cfg.HelperPath, program: program,
		maxBytes: cfg.MaxTaskBytes, limits: cfg.Limits, now: now, log: log,
	}
	e.probe = &http.Client{
		Transport: &http.Transport{
			Proxy:             nil,
			DialContext:       cfg.Egress.DialContext,
			TLSClientConfig:   &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: external},
			DisableKeepAlives: true,
		},
		// A redirect could lead the probe, with its credential, to an address nobody checked.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return e, nil
}

func externalTrust(file string) (*x509.CertPool, error) {
	if file == "" {
		return nil, nil
	}
	if !filepath.IsAbs(file) {
		return nil, errors.New("external CA file must be an absolute path")
	}
	data, err := os.ReadFile(file) //nolint:gosec // G304: deployment configuration, validated above.
	if err != nil {
		return nil, errors.New("external CA file unavailable")
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(data) {
		return nil, errors.New("external CA file holds no certificate")
	}
	return pool, nil
}

// Run executes one task to completion and reports what it observed. It never returns without
// having removed the task directory and the staged credential (or reporting that it could not).
func (e *Executor) Run(ctx context.Context, task *Task) Result {
	started := e.now()
	result := e.run(ctx, task)
	e.log.Info("repo executor task finished",
		zap.String("purpose", string(task.Purpose)),
		zap.String("operationId", task.OperationID),
		zap.String("verificationId", task.VerificationID),
		zap.String("classification", string(result.Classification)),
		zap.String("errorCode", result.ErrorCode),
		zap.String("detail", result.Detail),
		zap.Bool("cleanupFailed", result.CleanupFailed),
		zap.Duration("duration", e.now().Sub(started)),
	)
	return result
}

func (e *Executor) run(ctx context.Context, task *Task) Result {
	auth, refusal := admit(task, e.keys, e.now())
	if refusal != nil {
		return *refusal
	}
	timeout, ok := e.limits.budget(task)
	if !ok {
		return failed(CodeInvalidTask, "timeoutSeconds is outside the range allowed for the purpose")
	}
	// Checked before anything is redeemed: a refused address must not spend the credential.
	if err := e.egress.Check(ctx, task.Repository); err != nil {
		return failed(CodeEgressDenied, "repository address is not allowed")
	}
	ctx, cancelTimeout := context.WithTimeoutCause(ctx, timeout, errTimeout)
	defer cancelTimeout()
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	return e.execute(ctx, cancel, task, &auth)
}

// execute owns the task directory and the credential material for the task's whole life; the
// deferred cleanup runs on every path, including a panic.
func (e *Executor) execute(ctx context.Context, cancel context.CancelCauseFunc, task *Task, auth *cloneauth.Authorization) (result Result) {
	dir, err := newTaskDirectory(e.workRoot)
	if err != nil {
		return failed(CodeInternal, "task directory unavailable")
	}
	var material *credentialmaterial.Material
	defer func() {
		if err := e.cleanup(dir, material); err != nil {
			result.CleanupFailed = true
			e.log.Error("repo executor cleanup failed", zap.String("operationId", task.OperationID), zap.Error(err))
		}
	}()
	stop := watchSize(ctx, dir, e.maxBytes, cancel)
	defer stop()

	t := &taskRun{e: e, task: task, auth: auth, dir: dir, cancel: cancel, anonymous: task.CredentialID == ""}
	if !t.anonymous {
		credential, err := e.redeemer.Redeem(ctx, task.Authorization)
		if err != nil {
			return failed(CodeRedemptionFailed, "credential could not be redeemed")
		}
		if credential.Version != task.CredentialVersion {
			return failed(CodeRedemptionFailed, "redeemed credential version differs from the authorization")
		}
		t.credential = credential
		t.secrets = basicAuthSecrets(credential.Username, credential.Token)
		scope, err := credentialmaterial.NewScope(task.Repository)
		if err != nil {
			return failed(CodeInternal, "credential scope unavailable")
		}
		material, err = credentialmaterial.Stage(filepath.Join(e.workRoot, credentialsDirectory), scope, credential.Username, credential.Token)
		if err != nil {
			return failed(CodeInternal, "credential material could not be staged")
		}
		if !shellSafe(material.Dir()) {
			return failed(CodeInternal, "credential material directory is not shell-safe")
		}
	}
	t.git = e.newGit(dir, task, material)
	if o := t.run(ctx, "init", "--bare", "--quiet", "repo.git"); !o.ok() {
		return t.fail(ctx, CodeInternal, &o)
	}
	t.git.dir = filepath.Join(dir, "repo.git")
	switch task.Purpose {
	case cloneauth.PurposeSync:
		return t.sync(ctx)
	case cloneauth.PurposePublish:
		return t.publish(ctx)
	case cloneauth.PurposeProbe:
		return t.probe(ctx)
	}
	return failed(CodeInvalidTask, "unknown purpose")
}

// cleanup destroys the credential first (it is the reason cleanup matters), then the task directory.
func (e *Executor) cleanup(dir string, material *credentialmaterial.Material) error {
	var err error
	if material != nil {
		err = material.Destroy()
	}
	return errors.Join(err, removeVerified(dir))
}

// newGit assembles the task's single Git configuration: the hardened list, the external trust
// anchor (fixtures only), the Git Server options scoped to its URL, and the credential helper.
func (e *Executor) newGit(dir string, task *Task, material *credentialmaterial.Material) *git {
	config := hardenedConfig()
	if e.external != "" {
		config = append(config, "http.sslCAInfo="+e.external)
	}
	if task.OperationID != "" {
		config = append(config, e.gitServer.scopedConfig(task.OperationID)...)
	}
	// Clear any helper first so the executor's is the only one.
	config = append(config, "credential.helper=")
	if material != nil {
		config = append(config, "credential.helper="+e.helper+" "+material.Dir(), "credential.useHttpPath=true")
	}
	return &git{program: e.program, dir: dir, env: gitEnvironment(dir), config: config}
}

// taskRun is the state of one running task.
type taskRun struct {
	e          *Executor
	task       *Task
	auth       *cloneauth.Authorization
	dir        string
	git        *git
	cancel     context.CancelCauseFunc
	anonymous  bool
	credential Credential
	secrets    []string
}

// run executes one Git command and enforces the size limit synchronously afterwards as well, so a
// transfer finishing between two polls cannot slip past it.
func (t *taskRun) run(ctx context.Context, args ...string) output {
	o := t.git.run(ctx, args...)
	if o.ok() && directorySize(t.dir) > t.e.maxBytes {
		t.cancel(errTooLarge)
		o.err, o.exit = errTooLarge, -1
	}
	return o
}

// fail reports a failed command. The task's own end (size, timeout, shutdown) outranks the code the
// command's output suggests, because it is why the command failed.
func (t *taskRun) fail(ctx context.Context, code string, o *output) Result {
	cause := context.Cause(ctx)
	switch {
	case errors.Is(o.err, errTooLarge), errors.Is(cause, errTooLarge):
		return failed(CodeRepositoryTooLarge, "task directory exceeded the size limit")
	case errors.Is(cause, errTimeout):
		return failed(CodeTimeout, "task exceeded its timeout")
	case ctx.Err() != nil:
		return failed(CodeInternal, "task was cancelled")
	}
	return failed(code, redact(o.stderr, t.secrets))
}

// externalRepository is the external URL handed to Git, after the egress check.
func (t *taskRun) externalRepository(ctx context.Context) (string, *Result) {
	if err := t.e.egress.Check(ctx, t.task.Repository); err != nil {
		r := failed(CodeEgressDenied, "repository address is not allowed")
		return "", &r
	}
	return t.task.Repository, nil
}
