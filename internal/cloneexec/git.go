package cloneexec

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/wanglongan587/cloud/internal/credentialmaterial"
)

// reapDelay bounds how long a Git invocation may spend on I/O after the process it started is gone.
//
// A deadline kills the process, but it does not close the process's pipes: a descendant that
// inherited them — `git fetch` starts helpers and index-pack — keeps the write ends open, and the
// wait for the command then waits for that descendant rather than for Git. Without this the
// executor's own Timeout would be advisory, which matters here more than in a read-only server: the
// cleanup that destroys the staged credential material is deferred, so a wait that never returns is
// also a credential that outlives the execution that was supposed to end it.
const reapDelay = 5 * time.Second

// gitDeployment is the part of C's deployment that every path running Git shares: where credential
// material is staged, which Git and which helper run, and what Git trusts.
//
// It exists so the two Git-running paths — the credentialed clone and the mirror preparation — cannot
// drift into two different Git configurations. They differ in what they ask Git to do, not in how Git
// is invoked or where the credential comes from, and a deployment that changed one helper without the
// other would otherwise be a silent asymmetry between the two ways a token reaches a repository.
type gitDeployment struct {
	materialRoot  string
	homeDirectory string
	helperPath    string
	gitPath       string
	sslCAInfo     string
}

// validate checks every fixed path once, at startup, and refuses rather than defaulting.
//
// Each of these is a deployment boundary rather than input validation: a missing or unusable path
// would otherwise be discovered per job, and a misconfigured deployment would look like an upstream
// that every repository fails to reach.
//
// Existence and mode of the home directory are checked rather than assumed, because `HOME` is one of
// the few inputs Git reads that the command line cannot override. A missing directory is not a
// security failure — Git simply finds nothing — but it means the deployment did not create what it
// declared, and a constructor is the place that says so rather than a job that behaves inexplicably
// later.
//
//nolint:gocritic // Value receiver on purpose: this reads a snapshot of the deployment's own environment, and there is nothing here a caller could write back through.
func (g gitDeployment) validate() error {
	for _, part := range []struct{ name, value string }{
		{"credential materialization root", g.materialRoot},
		{"executor home directory", g.homeDirectory},
		{"credential helper path", g.helperPath},
		{"git path", g.gitPath},
	} {
		if err := validateExecutablePath(part.value); err != nil {
			return fmt.Errorf("%s: %w", part.name, err)
		}
	}
	if g.sslCAInfo != "" {
		if err := validateExecutablePath(g.sslCAInfo); err != nil {
			return fmt.Errorf("git certificate authority file: %w", err)
		}
	}
	info, err := os.Stat(g.homeDirectory)
	if err != nil {
		return fmt.Errorf("executor home directory: %w", err)
	}
	if !info.IsDir() {
		return errors.New("executor home directory is not a directory")
	}
	if info.Mode().Perm() != homeDirectoryMode {
		return fmt.Errorf("executor home directory is %#o, not %#o", info.Mode().Perm(), homeDirectoryMode)
	}
	return nil
}

// gitEnvironment is the entire environment a credentialed clone runs with.
//
// It is built from nothing rather than inherited and filtered. Inheriting would mean every variable
// the executor's own container happened to carry — `HTTPS_PROXY`, `GIT_CONFIG_GLOBAL`, `HOME`,
// `GIT_ASKPASS`, `GIT_SSH_COMMAND` — silently becomes part of the clone's configuration, and the set
// of variables that can redirect a credential is long enough that a deny-list would be a standing
// invitation to miss one. An allow-list of seven variables is auditable at a glance.
//
// `GIT_CONFIG_NOSYSTEM=1` and `GIT_CONFIG_GLOBAL=/dev/null` are the load-bearing pair: together with
// putting every constraint on the command line, they close the gap D2 names. Git reads system and
// global configuration before the command line, so without these two a host-level
// `credential.helper`, `url.<x>.insteadOf`, or `http.proxy` could take effect in a window the
// executor never inspected. With them, the command line below is the whole configuration.
//
// `HOME` points at the executor's own directory so Git has somewhere to look and finds nothing: an
// unset `HOME` makes Git fall back to other sources, and a real home directory could hold a
// `.gitconfig`.
func gitEnvironment(home string, extra []string) []string {
	environment := []string{
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"HOME=" + home,
		"PATH=/usr/local/bin:/usr/bin:/bin",
		"LC_ALL=C",
		// Nothing may prompt. A prompt in a container with no terminal would hang until the job
		// timeout instead of failing, and an askpass program would be a second, unreviewed way for a
		// credential to reach Git.
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=",
		"SSH_ASKPASS=",
		// LFS is disabled at the filter level below; this stops the smudge filter being attempted at
		// all, so a repository carrying LFS pointers cannot make the clone fetch a second, unscoped
		// resource.
		"GIT_LFS_SKIP_SMUDGE=1",
		// The clone runs in its own directory and no other Git process shares it, so optional index
		// locking buys nothing and can only leave lock files behind after a killed clone.
		"GIT_OPTIONAL_LOCKS=0",
	}
	return append(environment, extra...)
}

// helperArgument is the value the executor hands Git as its credential helper.
//
// Git splits this value on whitespace and, because the program is named by an absolute path, executes
// it directly rather than resolving `git credential-<name>`. The materialization root and the
// execution id are therefore separate argv entries, which is why both are validated as shell-safe
// before they are joined: a root containing whitespace or a metacharacter would otherwise let a
// deployment path change what Git executes.
func helperArgument(helperPath, materialRoot, executionID string) (string, error) {
	for _, part := range []struct{ name, value string }{
		{"credential helper path", helperPath},
		{"credential materialization root", materialRoot},
	} {
		if err := validateExecutablePath(part.value); err != nil {
			return "", fmt.Errorf("%s: %w", part.name, err)
		}
	}
	if !credentialmaterial.ValidExecutionID(executionID) {
		return "", credentialmaterial.ErrInvalidExecutionID
	}
	return helperPath + " " + materialRoot + " " + executionID, nil
}

// validateExecutablePath refuses a path Git would not execute as one program with fixed arguments.
//
// The check is deliberately strict — absolute, clean, and made only of characters that cannot change
// a command's meaning — because this string is concatenated into something Git runs. A path is
// deployment configuration rather than caller input, so a refusal here is a misconfiguration that
// should stop the executor at startup, not a runtime condition.
func validateExecutablePath(path string) error {
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

// repositoryScope returns the host and the path a credential helper will be asked about for a
// canonical repository.
//
// Git's credential protocol reports the host with its port only when the port is not the protocol's
// default, so the default is stripped here too, and it reports the path without a leading slash. Both
// values are reduced further — and made equal to the request's spelling — by
// `credentialmaterial.NormalizeScope`, which is the one place the staged scope and Git's request are
// compared, so this function only has to derive them from the repository the authorization named
// rather than from anything a caller could restate.
func repositoryScope(repository string) (host, path string, err error) {
	parsed, err := url.Parse(repository)
	if err != nil || parsed.Host == "" {
		return "", "", errors.New("repository is not a valid URL")
	}
	host = strings.ToLower(parsed.Host)
	if parsed.Scheme == "https" {
		host = strings.TrimSuffix(host, ":443")
	}
	path = parsed.EscapedPath()
	if path == "" || path == "/" {
		return "", "", errors.New("repository names no path")
	}
	return host, path, nil
}

// cloneArguments is the complete Git configuration and command for one credentialed clone.
//
// Every constraint is a `-c` on this command line rather than a configuration file, which is what
// makes the validated configuration and the used configuration the same bytes. The constraints fall
// into four groups:
//
//   - Transport: `protocol.allow=never` with only `https` re-enabled. A repository URL is influenced
//     by whoever chose the repository, and `protocol.file.allow` or `protocol.ext.allow` would let a
//     clone reach the executor's own filesystem or run a program; `protocol.ssh.allow` would move the
//     clone off the credential path entirely, where the helper's repository scope means nothing.
//   - Credential: `credential.helper=` clears any inherited helper before the executor's own is
//     added, so there is exactly one. `credential.useHttpPath=true` is what makes the helper's scope
//     check meaningful: it is what makes Git include the repository path in the request, and the
//     deployment-fixed helper compares that path — together with the host — against the normalized
//     repository identity the executor staged (`credentialmaterial.NormalizeScope`). Without this
//     option Git would report only the host, and a repository on the same host could borrow the staged
//     scope.
//   - Egress: `http.followRedirects=false` stops a redirect from carrying the token to a host the
//     authorization never named, and `http.proxy=` clears any proxy that would see the request.
//     `http.sslVerify=true` is stated rather than assumed so a global `sslVerify=false` cannot turn
//     verification off.
//   - Content: hooks are disabled (`core.hooksPath=/dev/null`), because a hook is a program from the
//     repository running as the credential-executing identity. Symlinks are not created, so a
//     checkout cannot plant a link the workload later follows out of its own tree. Submodule
//     recursion and the LFS filters are off, because both fetch further content whose scope no
//     authorization covers.
//
// `--` separates the options from the repository, so a repository reference that began with `-`
// cannot be read as another option. The URL is passed exactly as canonicalized, and the target is the
// executor's own path, so neither is taken from Git's own parsing of the other.
func cloneArguments(job *Job, helperPath, materialRoot, target, sslCAInfo string) ([]string, error) {
	helper, err := helperArgument(helperPath, materialRoot, job.ExecutionID)
	if err != nil {
		return nil, err
	}
	repository, err := url.Parse(job.Repository)
	if err != nil || repository.Scheme != "https" {
		return nil, errors.New("clone target is not an https repository")
	}
	if err := validateExecutablePath(target); err != nil {
		return nil, fmt.Errorf("clone target: %w", err)
	}
	if sslCAInfo != "" {
		if err := validateExecutablePath(sslCAInfo); err != nil {
			return nil, fmt.Errorf("git certificate authority file: %w", err)
		}
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
		"-c", "submodule.recurse=false",
		"-c", "fetch.recurseSubmodules=false",
		"-c", "filter.lfs.smudge=",
		"-c", "filter.lfs.process=",
		"-c", "filter.lfs.required=false",
	}
	// A deployment that terminates its Git service with its own certificate authority names it here,
	// as a deployment-fixed path. It is stated as a command-line option rather than through
	// `GIT_SSL_CAINFO` so that the trust anchor is part of the same audited command line as
	// `http.sslVerify=true`, and so a repository can never influence it. An unset value means the
	// system bundle, which is what a deployment cloning from a public host wants.
	if sslCAInfo != "" {
		arguments = append(arguments, "-c", "http.sslCAInfo="+sslCAInfo)
	}
	arguments = append(arguments,
		"clone",
		"--single-branch",
		"--no-tags",
	)
	if branch := strings.TrimSpace(job.Branch); branch != "" {
		if err := validateBranch(branch); err != nil {
			return nil, err
		}
		arguments = append(arguments, "--branch", branch)
	}
	return append(arguments, "--", job.Repository, target), nil
}

// validateBranch refuses a branch name that could be read as an option or that Git's own ref rules
// forbid.
//
// `git clone --branch` takes its value as the next argv entry, so a leading `-` would be consumed as
// another option. The rest of the rules are Git's `check-ref-format` restrictions, applied here so a
// malformed branch is a refusal before any network access rather than a confusing Git error after
// the authorization was already spent.
func validateBranch(branch string) error {
	if strings.HasPrefix(branch, "-") {
		return errors.New("branch must not begin with a dash")
	}
	if strings.HasPrefix(branch, "/") || strings.HasSuffix(branch, "/") || strings.Contains(branch, "..") {
		return errors.New("branch is not a valid ref name")
	}
	if strings.ContainsAny(branch, " \t\n\r~^:?*[\\") || strings.Contains(branch, "//") || strings.Contains(branch, "@{") {
		return errors.New("branch is not a valid ref name")
	}
	if strings.HasSuffix(branch, ".lock") || strings.HasSuffix(branch, ".") {
		return errors.New("branch is not a valid ref name")
	}
	return nil
}

// runGit runs one constrained Git command and returns both of its output streams alongside the exit
// error.
//
// Git's stderr is captured rather than streamed: it is the only useful thing a failed command
// produces, and it is also the one place a credential could reach a log, because a remote can put
// arbitrary text in a response and Git may quote it. The caller therefore redacts it against the
// exact secret it staged before the text goes anywhere, which is a stronger guarantee than trusting
// the remote not to have echoed it. The text is never returned to the caller of the job in any case.
//
// Standard output is returned for the commands whose whole purpose is to print a fact back — the
// commit a fetch resolved to, the refs a repository exposes. It is bounded by being what Git was
// asked for and nothing else: neither command is given a revision or a ref list from outside this
// package.
func runGit(ctx context.Context, gitPath, home string, arguments []string, workingDirectory string) (stdout, stderr string, err error) {
	command := exec.CommandContext(ctx, gitPath, arguments...)
	command.Dir = workingDirectory
	command.Env = gitEnvironment(home, nil)
	command.WaitDelay = reapDelay
	// Git has no stdin. It would otherwise be able to read a credential or a confirmation from
	// whatever the executor's standard input happens to be attached to.
	command.Stdin = nil
	var output, errorOutput strings.Builder
	command.Stdout = &output
	command.Stderr = &errorOutput
	if err := command.Run(); err != nil {
		return "", errorOutput.String(), err
	}
	return output.String(), errorOutput.String(), nil
}

// redactFailure turns Git's own error output into a failure the executor may log.
//
// Two bounds are applied, and both are about the remote rather than about Git. The staged secret is
// removed, because a hostile or merely careless server can reflect a request — including its
// Authorization header — into a message Git prints, and the executor is the one party that knows
// exactly what to look for. The text is then truncated, so a server cannot use a clone failure to
// write an unbounded amount into the executor's log.
//
// What survives is the repository path and the protocol error, which is what makes a failed clone
// diagnosable. The result is a log line and never a response body: the executor reports its own stable
// reason code to Cloud.
func redactFailure(stderr, secret string) error {
	detail := stderr
	if secret != "" {
		detail = strings.ReplaceAll(detail, secret, "[redacted]")
	}
	detail = strings.TrimSpace(detail)
	const limit = 4096
	if len(detail) > limit {
		detail = detail[:limit] + "…"
	}
	if detail == "" {
		return errors.New("git reported no error output")
	}
	return errors.New("git reported: " + detail)
}

// headCommit reads the commit a finished clone checked out.
//
// It runs through the same constrained configuration as the clone, so a repository cannot influence
// how the revision is read: `rev-parse` is given a fixed revision name and no repository-supplied
// configuration is in force.
func headCommit(ctx context.Context, gitPath, home, repository string) (string, error) {
	command := exec.CommandContext(ctx, gitPath,
		"-c", "protocol.allow=never",
		"-c", "core.hooksPath=/dev/null",
		"rev-parse", "HEAD",
	)
	command.Dir = repository
	command.Env = gitEnvironment(home, nil)
	command.Stdin = nil
	command.WaitDelay = reapDelay
	output, err := command.Output()
	if err != nil {
		return "", fmt.Errorf("read the cloned revision: %w", err)
	}
	commit := strings.TrimSpace(string(output))
	if len(commit) != 40 {
		return "", errors.New("the cloned revision is not a commit id")
	}
	return commit, nil
}

// prepareCheckoutRoot creates the directory clones are written into and returns its absolute path.
//
// The root is created if absent and its mode is fixed rather than assumed, so a root that arrived
// with a wider mode is narrowed before anything is written under it.
func prepareCheckoutRoot(root string) (string, error) {
	if err := validateExecutablePath(root); err != nil {
		return "", fmt.Errorf("checkout root: %w", err)
	}
	if err := os.MkdirAll(root, checkoutRootMode); err != nil {
		return "", fmt.Errorf("create checkout root: %w", err)
	}
	if err := os.Chmod(root, checkoutRootMode); err != nil {
		return "", fmt.Errorf("fix checkout root mode: %w", err)
	}
	return root, nil
}
