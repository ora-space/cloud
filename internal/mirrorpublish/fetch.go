package mirrorpublish

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/wanglongan587/cloud/internal/capture"
	"github.com/wanglongan587/cloud/internal/credentialmaterial"
	"github.com/wanglongan587/cloud/internal/mirror"
	"github.com/wanglongan587/cloud/internal/processgroup"
)

// MirrorBranch is the short name of the one branch a mirror exposes, and MirrorRef is that branch.
//
// A mirror carries exactly this ref and no other, which is what makes a clone of it deterministic
// (D7): there is no branch that could have moved, and a Workspace asking for one does not get
// whatever is at the tip today.
//
// It has to live under `refs/heads`, and the Node is why. The Node clones with
// `git clone --branch <name>` and its post-clone verification then requires `refs/heads/<name>` to
// exist locally, `HEAD` to be symbolic to it, and `refs/remotes/origin/<name>` to name the same
// commit (desktop `apps/ora-node/src/repository/inspection.rs`). Git's `--branch` resolves a name
// only as `refs/heads/<name>` or `refs/tags/<name>`, so a mirror whose single ref sat anywhere else
// would make every mirrored clone fail with "Remote branch not found" — measured against git 2.43,
// not assumed. The name is therefore fixed here and never taken from a caller.
//
// A fixed name is what the mirror's own identity requires. A mirror is keyed by
// `(project, repository, commit)` and not by a ref, so two Workspaces that asked for different refs
// resolving to the same commit share one mirror; naming the branch after whichever ref arrived first
// would make the second reader's checkout depend on the first reader's request, and re-pointing the
// ref for the second would republish content that is supposed to be immutable while readable.
//
// It is deliberately not the Workspace's own `requestedRef`: a client that checked this branch out
// cannot mistake it for the user's branch, and it cannot collide with one either, because the mirror
// repository holds nothing but the commit this branch points at.
const (
	// MirrorBranch is the name a clone of the mirror asks for.
	MirrorBranch = "ora-mirror"
	// MirrorRef is the single ref a mirror exposes, which points at the key's commit.
	MirrorRef = "refs/heads/" + MirrorBranch
)

// reapDelay bounds how long a fetch may spend on I/O after the Git process it started is gone.
//
// A deadline kills the process, but it does not close the process's pipes. This fetch starts the
// deployment-fixed credential helper, so a descendant that inherits the write ends is not
// hypothetical here: the wait for the command then waits for that descendant rather than for Git,
// and the declared Timeout becomes advisory — exactly the shape the clone executor was measured
// against, where a 180-second bound ended at Git's own 300-second connect timeout instead. The
// delay is what makes GitConfig.Timeout the thing that ends the fetch.
//
// It is deliberately the same five seconds the other two Git-running components use, and a second
// copy rather than a shared constant for the same reason: this package and those two do not import
// each other, and one value shared across them is how the three would drift back together.
const reapDelay = 5 * time.Second

// stallLimit and stallTime end a fetch that is not moving.
//
// GitConfig.Timeout bounds how long a fetch may take; it does not bound how long an upstream may hold
// the connection open while sending nothing, and a stalled transfer is the one shape that would
// otherwise spend the whole of that budget. Git aborts a transfer whose own throughput stays below
// `stallLimit` bytes per second for `stallTime`, so the bound is on the upstream's progress rather
// than on the clock. The values are copied rather than shared, for the reason `reapDelay` is: these
// are per-component deployment values, and one shared constant is how the three drift back together.
const (
	stallLimit = 1000
	stallTime  = 60 * time.Second
)

// captureLimit bounds each of the two streams one fetch may write into this process.
//
// The largest thing this package reads on standard output is a ref listing that must contain exactly
// one ref, and the diagnostics are cut to 2048 bytes before they become an error, so the bound is far
// above anything read here and far below what an upstream can print: a fetch forwards whatever the
// remote sends over the sideband, and the remote decides how much that is. It is a second copy of the
// executor's value rather than a shared constant, for the reason `reapDelay` is.
const captureLimit = 64 << 10

// Material is one credential the deployment-fixed helper can read for the duration of one fetch.
type Material struct {
	// Identity names the staged material to the helper. It is a canonical lowercase UUID for the same
	// reason the clone executor's execution identifiers are: the directory the helper reads is a
	// function of the identifier and not of the caller's spelling.
	Identity string
	// Remove destroys the material. It is called on every path, including a failed fetch, because the
	// material's whole lifetime is the fetch that needs it.
	Remove func() error
}

// MaterialSource stages the credential one fetch needs.
//
// It is an interface rather than a call into the credential store because the writer's fetch runs in
// the credential-executing identity C, which obtains a credential the same way a clone does: by
// redeeming a single-use authorization Cloud signed for one repository. Keeping it at the consuming
// boundary is what lets this package be tested without a store, and it is the seam a deployment
// wires to the real redemption.
type MaterialSource interface {
	Stage(ctx context.Context, reference *Reference) (Material, error)
}

// GitConfig fixes how the writer runs Git. Every field is deployment-fixed; none is derived from a
// reference, so a repository cannot influence the command line that reads it.
type GitConfig struct {
	// GitPath is the absolute path of the Git binary.
	GitPath string
	// Home is an existing directory Git uses as HOME, so it finds no global configuration.
	Home string
	// SSLCAInfo is the optional certificate authority file Git verifies the upstream's TLS
	// certificate against. Empty means the system bundle.
	SSLCAInfo string
	// HelperPath is the absolute path of the deployment-fixed credential helper (D11). It is
	// required: an internal channel that reaches upstream anonymously would be a second, unscoped way
	// to read a repository, which invariant 9 refuses.
	HelperPath string
	// MaterialRoot is the directory the helper reads staged material from.
	MaterialRoot string
	// Timeout bounds one fetch, including the network.
	Timeout time.Duration
}

// GitFetcher materializes a mirror by fetching one branch from the project's upstream repository.
//
// It is the only component in this package that touches the network, and it is written as the
// narrowest command that can produce the content the key names: one branch, into one ref, with the
// commit verified afterwards rather than trusted.
type GitFetcher struct {
	config   GitConfig
	material MaterialSource
}

// NewGitFetcher validates the deployment's Git configuration once, at startup.
//
// A missing or unusable path is refused here rather than discovered per fetch, for the same reason
// the executor refuses one: a misconfigured deployment would otherwise look like an upstream that
// every repository fails to reach.
// fetcher, so a caller that keeps its value and edits it cannot move a running fetcher's command
// line afterwards.
//
//nolint:gocritic // By value on purpose: the deployment's Git configuration is copied into the
func NewGitFetcher(config GitConfig, material MaterialSource) (*GitFetcher, error) {
	if material == nil {
		return nil, errors.New("the mirror fetcher needs a credential source")
	}
	if config.Timeout <= 0 {
		return nil, errors.New("the mirror fetcher needs a positive timeout")
	}
	for _, part := range []struct{ name, value string }{
		{"git path", config.GitPath},
		{"credential helper path", config.HelperPath},
		{"credential materialization root", config.MaterialRoot},
		{"home directory", config.Home},
	} {
		if err := validateFixedPath(part.value); err != nil {
			return nil, fmt.Errorf("%s: %w", part.name, err)
		}
	}
	if config.SSLCAInfo != "" {
		if err := validateFixedPath(config.SSLCAInfo); err != nil {
			return nil, fmt.Errorf("certificate authority file: %w", err)
		}
	}
	info, err := os.Stat(config.Home)
	if err != nil || !info.IsDir() {
		return nil, fmt.Errorf("home directory %s is not a readable directory", config.Home)
	}
	return &GitFetcher{config: config, material: material}, nil
}

// Fetch materializes reference into destination and reports the ref and commit that are there.
//
// The commit is read back out of the fetched repository instead of being taken from the key, so the
// verification in internal/mirror compares two independently obtained values. A branch whose tip is
// no longer the pinned commit therefore fails the fetch rather than publishing a mirror of the wrong
// content.
func (f *GitFetcher) Fetch(ctx context.Context, reference *Reference, destination string) (mirror.PublishVerification, error) {
	ctx, cancel := context.WithTimeout(ctx, f.config.Timeout)
	defer cancel()
	branch, err := upstreamBranch(reference.Upstream)
	if err != nil {
		return mirror.PublishVerification{}, err
	}
	staged, err := f.material.Stage(ctx, reference)
	if err != nil {
		return mirror.PublishVerification{}, fmt.Errorf("stage the upstream credential: %w", err)
	}
	defer func() {
		if staged.Remove != nil {
			_ = staged.Remove()
		}
	}()
	helper, err := helperArgument(f.config.HelperPath, f.config.MaterialRoot, staged.Identity)
	if err != nil {
		return mirror.PublishVerification{}, err
	}
	arguments, err := f.arguments(helper, reference.Key.Repository, branch)
	if err != nil {
		return mirror.PublishVerification{}, err
	}
	if err := os.MkdirAll(destination, 0o700); err != nil {
		return mirror.PublishVerification{}, fmt.Errorf("create the staging directory: %w", err)
	}
	// A bare repository is what the Git Server serves: it runs `git upload-pack` in the content
	// directory, and a bare repository is that directory rather than a working tree inside one.
	if _, err := f.run(ctx, destination, []string{"init", "--quiet", "--bare", "--", destination}); err != nil {
		return mirror.PublishVerification{}, fmt.Errorf("initialize the mirror: %w", err)
	}
	if _, err := f.run(ctx, destination, arguments); err != nil {
		return mirror.PublishVerification{}, fmt.Errorf("fetch %s from %s: %w", reference.Upstream, reference.Key.Repository, err)
	}
	commit, err := f.run(ctx, destination, []string{"rev-parse", "--verify", "--quiet", MirrorRef + "^{commit}"})
	if err != nil {
		return mirror.PublishVerification{}, fmt.Errorf("read back the fetched ref: %w", err)
	}
	refs, err := f.run(ctx, destination, []string{"for-each-ref", "--format=%(refname)"})
	if err != nil {
		return mirror.PublishVerification{}, fmt.Errorf("list the mirror's refs: %w", err)
	}
	if refs = strings.TrimSpace(refs); refs != MirrorRef {
		// The fetched repository must expose one ref and only one. A second ref would be content the
		// key does not name, and a client could clone it.
		return mirror.PublishVerification{}, fmt.Errorf("the mirror exposes %q; exactly %q is permitted", refs, MirrorRef)
	}
	// HEAD is set so a client that clones without naming a branch gets the mirrored commit rather
	// than an error about an unborn branch.
	if _, err := f.run(ctx, destination, []string{"symbolic-ref", "HEAD", MirrorRef}); err != nil {
		return mirror.PublishVerification{}, fmt.Errorf("point HEAD at the mirror: %w", err)
	}
	return mirror.PublishVerification{Commit: strings.TrimSpace(commit), Ref: MirrorRef}, nil
}

// arguments is the fetch command line.
//
// It is the same hardening the credentialed clone uses, applied to `fetch` instead of `clone`, and
// for the same reasons: the protocol set is closed, so a repository URL cannot select one this
// process did not review; redirects are refused, so an upstream cannot move the request to another
// host; the credential helper is cleared and then set to the one deployment-fixed program; and the
// environment (below) has no prompt and no global configuration for anything to be hidden in.
//
// `--no-tags` and `--no-write-fetch-head` are what keep the result to exactly one ref: a fetched tag
// would be content no key names, and FETCH_HEAD would be a file a later `git fetch` in this
// repository could be influenced by.
func (f *GitFetcher) arguments(helper, repository, branch string) ([]string, error) {
	if err := validateUpstreamRepository(repository); err != nil {
		return nil, err
	}
	arguments := []string{
		"-c", "protocol.allow=never",
		"-c", "protocol.https.allow=always",
		"-c", "protocol.ssh.allow=never",
		"-c", "protocol.file.allow=never",
		"-c", "protocol.ext.allow=never",
		"-c", "core.hooksPath=/dev/null",
		"-c", "core.askPass=",
		"-c", "core.symlinks=false",
		"-c", "credential.helper=",
		"-c", "credential.helper=" + helper,
		"-c", "credential.useHttpPath=true",
		"-c", "http.followRedirects=false",
		"-c", "http.proxy=",
		"-c", "http.sslVerify=true",
		"-c", "http.lowSpeedLimit=" + strconv.Itoa(stallLimit),
		"-c", "http.lowSpeedTime=" + strconv.Itoa(int(stallTime/time.Second)),
		"-c", "submodule.recurse=false",
		"-c", "fetch.recurseSubmodules=false",
		"-c", "filter.lfs.smudge=",
		"-c", "filter.lfs.process=",
		"-c", "filter.lfs.required=false",
	}
	if f.config.SSLCAInfo != "" {
		// The trust anchor is stated as a command-line option rather than through GIT_SSL_CAINFO so
		// that it is part of the same audited command line as `http.sslVerify=true`.
		arguments = append(arguments, "-c", "http.sslCAInfo="+f.config.SSLCAInfo)
	}
	return append(arguments,
		"fetch", "--quiet", "--no-tags", "--no-write-fetch-head", "--",
		repository, "+refs/heads/"+branch+":"+MirrorRef,
	), nil
}

// run executes one Git command and returns its standard output.
//
// Standard error is captured rather than streamed and is bounded before it becomes an error: a
// remote can put arbitrary text in a response and Git may quote it, so the one party that could leak
// a credential is the remote, and the executor's own redaction is applied to what it staged. Here
// the credential never reaches this process — the helper holds it — so what is bounded is the size.
// Both streams share that bound, because both are written by a party this process does not choose.
//
// What came back cut is treated as what it is: standard output that stopped at the bound is not a
// fact — a ref listing that was cut short is a prefix of what the upstream advertises, and a caller
// comparing a prefix against the one ref it requires would be comparing against a value Git never
// finished printing. Standard error carries no such role: it is reproduced with the bound's own
// marker in it, so a diagnosis that stopped early reads as one that stopped early. Withholding it
// instead would cost the first 2048 bytes of the diagnosis and hide nothing, because a remote that
// wanted to put something in those bytes can do so whatever the bound is.
func (f *GitFetcher) run(ctx context.Context, directory string, arguments []string) (string, error) {
	//nolint:gosec // The program is a deployment-fixed absolute path validated at construction and
	// every argument is a fixed list built above; the only values from a reference are a canonical
	// https URL and a ref name that was checked against Git's own ref rules.
	command := exec.CommandContext(ctx, f.config.GitPath, arguments...)
	// The timeout has to end the process tree and not only the process: this is a fetch, so the
	// descendants are the transport helper and the indexer, and they are what holds the pipes open.
	processgroup.Isolate(command)
	command.Dir = directory
	// Set before Start, because exec reads it when it builds the reaping path. Without it the wait
	// outlives the deadline whenever a descendant holds the pipes; see reapDelay.
	command.WaitDelay = reapDelay
	command.Env = []string{
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"HOME=" + f.config.Home,
		"PATH=/usr/local/bin:/usr/bin:/bin",
		"LC_ALL=C",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=",
		"SSH_ASKPASS=",
		"GIT_LFS_SKIP_SMUDGE=1",
		"GIT_OPTIONAL_LOCKS=0",
	}
	command.Stdin = nil
	output := &capture.Buffer{Limit: captureLimit}
	stderr := &capture.Buffer{Limit: captureLimit}
	command.Stdout = output
	command.Stderr = stderr
	if err := command.Run(); err != nil {
		return "", fmt.Errorf("%w: %s", err, truncate(stderr.String(), 2048))
	}
	if output.Truncated() {
		return "", errors.New("git printed more than the fetcher keeps, so what it printed is not a fact it can read")
	}
	return output.String(), nil
}

// helperArgument is the value Git is handed as its credential helper.
//
// Git splits this value on whitespace and, because the program is named by an absolute path, runs it
// directly rather than resolving `git credential-<name>`. The materialization root and the identity
// are therefore separate argv entries, which is why all three parts are validated first: a root
// containing a space would otherwise change what Git executes.
func helperArgument(helperPath, materialRoot, identity string) (string, error) {
	for _, part := range []struct{ name, value string }{
		{"credential helper path", helperPath},
		{"credential materialization root", materialRoot},
	} {
		if err := validateFixedPath(part.value); err != nil {
			return "", fmt.Errorf("%s: %w", part.name, err)
		}
	}
	if !credentialmaterial.ValidExecutionID(identity) {
		return "", fmt.Errorf("%w: the staged credential identity is not a canonical UUID", credentialmaterial.ErrInvalidExecutionID)
	}
	return helperPath + " " + materialRoot + " " + identity, nil
}

// upstreamBranch turns the branch Cloud recorded into the ref name to fetch.
//
// The stored value is a branch name, not a ref, so it is the one part of the fetch that could be
// read as an option or as a path. Git's own ref rules are applied here rather than after the fetch,
// so a malformed branch is a refusal before any network access happens.
func upstreamBranch(upstream string) (string, error) {
	branch := strings.TrimSpace(upstream)
	if branch == "" {
		return "", errors.New("the reference names no upstream branch, so no mirror of it is deterministic")
	}
	if strings.HasPrefix(branch, "-") || strings.HasPrefix(branch, "/") || strings.HasSuffix(branch, "/") {
		return "", fmt.Errorf("upstream branch %q is not a valid ref name", branch)
	}
	if strings.Contains(branch, "..") || strings.Contains(branch, "//") || strings.Contains(branch, "@{") {
		return "", fmt.Errorf("upstream branch %q is not a valid ref name", branch)
	}
	if strings.ContainsAny(branch, " \t\n\r~^:?*[\\") {
		return "", fmt.Errorf("upstream branch %q is not a valid ref name", branch)
	}
	if strings.HasSuffix(branch, ".lock") || strings.HasSuffix(branch, ".") {
		return "", fmt.Errorf("upstream branch %q is not a valid ref name", branch)
	}
	return branch, nil
}

// validateUpstreamRepository refuses a repository the fetch could not be built from.
//
// The value reaches Git as its own argv entry after `--`, so it cannot be read as an option; the
// check is the second line of defense rather than the only one. It requires the canonical form the
// key already promises — an https URL with a host and no whitespace — so a key that was somehow
// built from another spelling is refused here rather than fetched from.
func validateUpstreamRepository(repository string) error {
	if strings.HasPrefix(repository, "-") {
		return errors.New("upstream repository must not begin with a dash")
	}
	parsed, err := url.Parse(repository)
	if err != nil {
		return fmt.Errorf("upstream repository is not a URL: %w", err)
	}
	if parsed.Scheme != "https" || parsed.Host == "" {
		return errors.New("upstream repository must be an https URL naming a host")
	}
	if strings.ContainsAny(repository, " \t\n\r") {
		return errors.New("upstream repository must not contain whitespace")
	}
	return nil
}

// validateFixedPath refuses a deployment path Git would not run as one program with fixed arguments.
//
// It is deliberately strict — absolute, clean, and made only of characters that cannot change a
// command's meaning — because these values are concatenated into something Git executes. They are
// deployment configuration rather than caller input, so a refusal is a misconfiguration.
func validateFixedPath(path string) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("is not configured")
	}
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("must be an absolute, clean path")
	}
	if strings.ContainsAny(path, " \t\n\r\"'`$&|;<>()*?[]{}!~#\\") {
		return errors.New("must not contain whitespace or shell metacharacters")
	}
	return nil
}

// truncate bounds text that came from outside this process before it becomes an error.
func truncate(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	return text[:limit] + "…"
}
