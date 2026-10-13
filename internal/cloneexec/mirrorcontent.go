package cloneexec

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/wanglongan587/cloud/internal/cloneauth"
	"github.com/wanglongan587/cloud/internal/mirror"
	"github.com/wanglongan587/cloud/internal/mirrorpublish"
)

// mirrorArguments is the complete Git configuration and command for one mirror fetch.
//
// It is the mirror purpose's counterpart to `cloneArguments`, and the hardening is the same list for
// the same reasons: the protocol set is closed so a repository URL cannot select one this process did
// not review; redirects are refused so an upstream cannot move the request, and its credential, to
// another host; the credential helper is cleared and then set to the one deployment-fixed program, with
// `credential.useHttpPath=true` so the helper's repository-scope check is meaningful; hooks are
// disabled; and submodules, symlinks and the LFS filters are off, because each of those is a program
// or a fetch the authorization does not cover.
//
// It is written here rather than shared with internal/mirrorpublish's fetcher because the two reach
// Git through different lifecycles — one preparation under one authorization, versus a pass that
// reconciles Cloud's whole reference set — and because this copy has to run before the commit that
// names the resulting mirror is known. What the two must not differ in is the artifact they produce,
// which is why both target `mirrorpublish.MirrorRef` rather than each spelling the ref themselves.
//
// `--no-tags` and `--no-write-fetch-head` are what keep the result to exactly one ref: a fetched tag
// would be content no key names, and FETCH_HEAD would be a file a later Git command in this repository
// could be influenced by. `--` separates the options from the repository, so a repository reference
// that began with `-` cannot be read as another option.
func mirrorArguments(helper, repository, ref, sslCAInfo string) ([]string, error) {
	// The helper value is the composed `path root identity` triple, so it is deliberately not put
	// through `validateExecutablePath`: that rule describes one path, and the three components were each
	// validated by `helperArgument` before they were joined. What is checked here is that the value is
	// not blank, because `credential.helper=` with nothing after it is the directive that *clears* the
	// helper — a deployment that produced one would be fetching with no credential at all, and it would
	// look like an upstream that refused anonymous access rather than like a misconfiguration.
	if strings.TrimSpace(helper) == "" {
		return nil, errors.New("the credential helper is not configured")
	}
	parsed, err := url.Parse(repository)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return nil, errors.New("the mirror target is not an https repository")
	}
	if strings.ContainsAny(repository, " \t\n\r") {
		return nil, errors.New("the mirror target must not contain whitespace")
	}
	if err := validateMirrorRef(ref); err != nil {
		return nil, err
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
	if sslCAInfo != "" {
		arguments = append(arguments, "-c", "http.sslCAInfo="+sslCAInfo)
	}
	return append(arguments,
		"fetch", "--quiet", "--no-tags", "--no-write-fetch-head", "--",
		repository, "+"+ref+":"+mirrorpublish.MirrorRef,
	), nil
}

// validateMirrorRef refuses a ref the fetch could be built from that is not the one the mirror purpose
// signs.
//
// The rules are `cloneauth.ValidateRef`'s, and this is a second copy on purpose in the same sense the
// clone path's branch rules are: this layer cannot import the issuer's vocabulary as a *permission*
// without making the issuer's rule the only thing standing between a job and a fetch, and the check
// here is what makes the value safe to concatenate into a refspec. It is applied to the job's ref
// before the authorization is verified as well, so a malformed ref is a malformed job rather than a
// confusing Git error after an authorization has been spent.
func validateMirrorRef(ref string) error {
	if !strings.HasPrefix(ref, "refs/heads/") {
		return fmt.Errorf("ref %q is not a branch under refs/heads/", ref)
	}
	name := strings.TrimPrefix(ref, "refs/heads/")
	if name == "" {
		return errors.New("the mirror ref names no branch")
	}
	if strings.HasPrefix(name, "-") || strings.HasPrefix(name, "/") || strings.HasSuffix(name, "/") {
		return fmt.Errorf("ref %q is not a valid ref name", ref)
	}
	if strings.Contains(name, "..") || strings.Contains(name, "//") || strings.Contains(name, "@{") {
		return fmt.Errorf("ref %q is not a valid ref name", ref)
	}
	if strings.ContainsAny(name, " \t\n\r~^:?*[\\") {
		return fmt.Errorf("ref %q is not a valid ref name", ref)
	}
	if strings.HasSuffix(name, ".lock") || strings.HasSuffix(name, ".") {
		return fmt.Errorf("ref %q is not a valid ref name", ref)
	}
	return nil
}

// fetch materializes the authorized ref into destination and reports the ref and commit that arrived.
//
// The commit is read back out of the fetched repository rather than taken from the job, because the job
// does not carry one: a mirror preparation is the step that *discovers* which commit the pinned ref
// resolves to, and the mirror's identity is built from what actually arrived. Reading it back is also
// what makes publication's verification compare two independently obtained values — the writer's
// answer and the store's key — rather than one value written down twice.
//
// Nothing here retries and nothing here is reusable: the authorization was spent before this call, so a
// failure is the outcome of this attempt, and a new attempt needs a new authorization.
func (m *MirrorExecutor) fetch(ctx context.Context, job *MirrorJob, repository, identity, destination, token string) (mirror.PublishVerification, error) {
	helper, err := helperArgument(m.config.HelperPath, m.config.MaterialRoot, identity)
	if err != nil {
		return mirror.PublishVerification{}, err
	}
	arguments, err := mirrorArguments(helper, repository, job.Ref, m.config.SSLCAInfo)
	if err != nil {
		return mirror.PublishVerification{}, err
	}
	// A bare repository is what the internal Git Server serves: it runs `git upload-pack` in the content
	// directory, and a bare repository is that directory rather than a working tree inside one. The
	// destination already exists, because the caller created it as the place a fetch of unknown size
	// can be abandoned without leaving anything behind.
	if _, stderr, err := runGit(ctx, m.config.GitPath, m.config.HomeDirectory,
		[]string{"init", "--quiet", "--bare", "--", destination}, destination); err != nil {
		return mirror.PublishVerification{}, redactFailure(stderr, token)
	}
	if _, stderr, err := runGit(ctx, m.config.GitPath, m.config.HomeDirectory, arguments, destination); err != nil {
		return mirror.PublishVerification{}, redactFailure(stderr, token)
	}
	commit, err := m.gitFact(ctx, destination, "rev-parse", "--verify", "--quiet", mirrorpublish.MirrorRef+"^{commit}")
	if err != nil {
		return mirror.PublishVerification{}, fmt.Errorf("read back the fetched ref: %w", err)
	}
	// The commit is not shape-checked here. It becomes a component of the mirror's key, and the store's
	// own key rule — a full object name and nothing else — is applied by `Begin` in `publish` below.
	// Checking it twice would mean two rules for one value, and the one that matters is the one the
	// store enforces on the key it writes.
	refs, err := m.gitFact(ctx, destination, "for-each-ref", "--format=%(refname)")
	if err != nil {
		return mirror.PublishVerification{}, fmt.Errorf("list the mirror's refs: %w", err)
	}
	if refs != mirrorpublish.MirrorRef {
		// The fetched repository must expose one ref and only one. A second ref would be content no
		// key names and that a client could nevertheless clone.
		return mirror.PublishVerification{}, fmt.Errorf("the fetched repository exposes %q; exactly %q is permitted", refs, mirrorpublish.MirrorRef)
	}
	// HEAD is set so a client that clones without naming a ref gets the mirrored commit rather than an
	// error about an unborn branch.
	if _, err := m.gitFact(ctx, destination, "symbolic-ref", "HEAD", mirrorpublish.MirrorRef); err != nil {
		return mirror.PublishVerification{}, fmt.Errorf("point HEAD at the mirror: %w", err)
	}
	return mirror.PublishVerification{Commit: commit, Ref: mirrorpublish.MirrorRef}, nil
}

// gitFact runs one constrained Git command whose whole purpose is to print a fact, and returns what it
// printed with the surrounding whitespace removed.
//
// It goes through the same environment and the same closed configuration as the fetch, so a repository
// cannot influence how a revision or a ref list is read, and the secret is not passed because these
// commands never touch the network and therefore have no error output that could contain one.
func (m *MirrorExecutor) gitFact(ctx context.Context, directory string, arguments ...string) (string, error) {
	prefixed := append([]string{"-c", "protocol.allow=never", "-c", "core.hooksPath=/dev/null"}, arguments...)
	output, stderr, err := runGit(ctx, m.config.GitPath, m.config.HomeDirectory, prefixed, directory)
	if err != nil {
		return "", redactFailure(stderr, "")
	}
	return strings.TrimSpace(output), nil
}

// publish installs the fetched repository as the mirror of the commit it contains.
//
// The store is asked to begin the key the fetch *produced* rather than one the job named, and the order
// is what makes a retried preparation idempotent: if that exact commit is already published, `Begin`
// answers with the record that exists, and this returns it instead of fetching a second copy of content
// the store already serves. A retry therefore costs one fetch and publishes nothing.
//
// A withdrawn key is not undone here. `Begin` refuses it, and that refusal is reported as a failure
// rather than treated as "publish it again": a withdrawal is durable by design, and the code that may
// reverse one is a deployment decision, not a preparation attempt.
func (m *MirrorExecutor) publish(ctx context.Context, binding *cloneauth.Binding, verification mirror.PublishVerification, fetchDirectory string) (mirror.Record, error) {
	key := mirror.Key{
		ProjectID: binding.ProjectID,
		// The repository is the canonical identity the authorization bound, which `matchMirrorBinding`
		// has already compared against the job's canonicalized spelling — so the key stores the same
		// spelling the credential was scoped to, and not a second one derived here.
		Repository: binding.Repository,
		Commit:     verification.Commit,
	}
	prep, err := m.config.Mirrors.Begin(ctx, key)
	if err != nil {
		var published *mirror.AlreadyPublishedError
		if errors.As(err, &published) {
			return published.Record, nil
		}
		return mirror.Record{}, err
	}
	defer func() { _ = prep.Close() }()
	// The fetch is moved into the staging directory the preparation reserved rather than fetched into it
	// directly, because the key that names the staging directory is derived from the commit and the
	// commit is what the fetch produces. `Adopt` is the store's own name for that move, and it is where
	// the same-filesystem requirement is reported rather than silently copied around.
	if err := prep.Adopt(fetchDirectory); err != nil {
		return mirror.Record{}, err
	}
	record, err := prep.Publish(ctx, verification)
	if err != nil {
		// `Publish` has already recorded what it found — a size or volume limit is terminal for this
		// attempt and the staged content is discarded — so this only reports it.
		return mirror.Record{}, err
	}
	return record, nil
}
