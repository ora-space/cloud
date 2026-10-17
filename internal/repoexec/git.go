package repoexec

import (
	"context"
	"encoding/base64"
	"errors"
	"net/url"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/wanglongan587/cloud/internal/capture"
	"github.com/wanglongan587/cloud/internal/processgroup"
)

// captureLimit bounds each output stream of one Git process. Everything read as a fact (a porcelain
// report, an ls-remote answer) is far smaller; a remote decides how much sideband text arrives, so
// the bound is what keeps it from growing this process. A truncated stdout is never read as a fact.
const captureLimit = 64 << 10

// detailLimit bounds the diagnostic excerpt returned to Cloud, after redaction.
const detailLimit = 2 << 10

// Transfer stall bounds (ADR D4): a remote that holds a connection open while sending nothing is
// ended by Git itself rather than spending the whole task budget.
const (
	lowSpeedLimit = 1000
	lowSpeedTime  = 60 * time.Second
)

// fixedPath is the only PATH Git processes see; nothing is inherited from the executor's environment.
const fixedPath = "/usr/local/bin:/usr/bin:/bin"

// hardenedConfig is the single closed Git configuration every command runs with (ADR D4). With
// GIT_CONFIG_NOSYSTEM and GIT_CONFIG_GLOBAL=/dev/null in the environment, these options are the whole
// configuration Git reads.
func hardenedConfig() []string {
	return []string{
		"protocol.allow=never",
		"protocol.https.allow=always",
		"protocol.ssh.allow=never",
		"protocol.file.allow=never",
		"protocol.ext.allow=never",
		"core.hooksPath=/dev/null",
		"core.askPass=",
		"core.symlinks=false",
		"http.followRedirects=false",
		"http.proxy=",
		"http.sslVerify=true",
		"http.lowSpeedLimit=" + strconv.Itoa(lowSpeedLimit),
		"http.lowSpeedTime=" + strconv.Itoa(int(lowSpeedTime/time.Second)),
		"submodule.recurse=false",
		"fetch.recurseSubmodules=false",
		"filter.lfs.smudge=",
		"filter.lfs.process=",
		"filter.lfs.required=false",
		"push.followTags=false",
	}
}

// gitEnvironment is built from nothing (ADR D4): inheriting would let any variable of the executor's
// container (proxy, askpass, config locations) become part of a credentialed command.
func gitEnvironment(home string) []string {
	return []string{
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"HOME=" + home,
		"PATH=" + fixedPath,
		"LC_ALL=C",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=",
		"SSH_ASKPASS=",
		"GIT_LFS_SKIP_SMUDGE=1",
		"GIT_OPTIONAL_LOCKS=0",
	}
}

// operationHeader names the Operation a Git Server service-listener request serves. It equals
// gitserver.OperationHeader (asserted by a test); the executor does not import the Git Server, which
// would link Cloud's core and database layers into this binary.
const operationHeader = "Ora-Git-Operation-Id"

// gitServerAccess is how the executor reaches the Git Server's service repository listener.
type gitServerAccess struct {
	// base is the listener's URL with a trailing "/", the key of the URL-scoped options.
	base                   string
	cert, key, certificate string
}

// scopedConfig returns the options that apply only to URLs under the Git Server base: the executor's
// service certificate, the Git Server CA and the Operation header. Git selects http.<url>.* by URL
// prefix, so the external remote, a different scheme/host/port, never receives any of them.
func (g *gitServerAccess) scopedConfig(operationID string) []string {
	prefix := "http." + g.base + "."
	return []string{
		prefix + "sslCert=" + g.cert,
		prefix + "sslKey=" + g.key,
		prefix + "sslCAInfo=" + g.certificate,
		prefix + "extraHeader=" + operationHeader + ": " + operationID,
	}
}

// repository is the Project repository URL on the service listener.
func (g *gitServerAccess) repository(tenantID, projectID string) string {
	return g.base + tenantID + "/" + projectID + ".git"
}

// git runs Git for one task: one program, one environment, one configuration.
type git struct {
	program string
	dir     string
	env     []string
	config  []string
}

// output is what one Git process wrote and how it ended.
type output struct {
	stdout    string
	stdoutCut bool
	stderr    string
	exit      int
	err       error
}

// run executes one Git command in the task's bare repository. The process heads its own group and
// cancellation kills the whole group, so no helper (remote-https, index-pack) outlives the task.
func (g *git) run(ctx context.Context, args ...string) output {
	full := make([]string, 0, 2*len(g.config)+len(args))
	for _, c := range g.config {
		full = append(full, "-c", c)
	}
	full = append(full, args...)
	cmd := exec.CommandContext(ctx, g.program, full...) //nolint:gosec // G204: fixed deployment program; arguments are validated refs, OIDs and normalized URLs passed as argv, never through a shell.
	processgroup.Isolate(cmd)
	cmd.Dir = g.dir
	cmd.Env = g.env
	cmd.Stdin = nil
	stdout := &capture.Buffer{Limit: captureLimit}
	stderr := &capture.Buffer{Limit: captureLimit}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	err := cmd.Run()
	o := output{stdout: string(stdout.Bytes()), stdoutCut: stdout.Truncated(), stderr: stderr.String(), err: err}
	var exit *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exit):
		o.exit = exit.ExitCode()
	default:
		o.exit = -1
	}
	return o
}

// ok reports a zero exit.
func (o *output) ok() bool { return o.err == nil }

// userinfo matches the userinfo part of URLs in diagnostics.
var userinfo = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://)[^/\s@]+@`)

// redact removes every staged secret (and its common encodings) and URL userinfo from text, and only
// then cuts it to detailLimit, so a secret printed across the cut is replaced before the cut decides.
func redact(text string, secrets []string) string {
	for _, s := range secrets {
		if s == "" {
			continue
		}
		for _, form := range []string{s, url.QueryEscape(s), url.PathEscape(s), base64.StdEncoding.EncodeToString([]byte(s)), base64.RawURLEncoding.EncodeToString([]byte(s))} {
			text = strings.ReplaceAll(text, form, "[redacted]")
		}
	}
	text = userinfo.ReplaceAllString(text, "${1}[redacted]@")
	text = strings.TrimSpace(text)
	if len(text) > detailLimit {
		cut := detailLimit
		for cut > 0 && !utf8.RuneStart(text[cut]) {
			cut--
		}
		text = text[:cut]
	}
	return text
}

// basicAuthSecrets returns the forms in which a username/token pair can appear on the wire.
func basicAuthSecrets(username, token string) []string {
	if token == "" {
		return nil
	}
	return []string{token, username + ":" + token}
}
