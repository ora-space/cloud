package cloneexec

import (
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"unicode"
)

// GitEgress is a deployment's explicit, opt-in route for Git's own egress.
//
// The zero value is disabled, and disabled is the default every deployment gets: the command line is
// then byte-for-byte the one that refuses every proxy, which is what makes this option additive rather
// than a change in behavior. When it is enabled, Git reaches the hosts it names through ProxyURL and
// every other address exactly as before — internal hosts are not excluded from the proxy, they simply
// never match it, because a proxy entry is emitted only for a host on Hosts.
//
// It exists because Git's own transport to a public forge can degrade on bulk transfer to the point that
// a mirror never completes, while an ordinary forward proxy on the same host carries the same fetch at
// full speed. A deployment in that position has no other way to reach its upstreams: the executor's
// environment is a fixed allow-list that deliberately excludes `HTTPS_PROXY`, and the shared command line
// deliberately clears `http.proxy`.
//
// # Deviation from approved decisions
//
// This option departs from two approved decisions. They remain the governing text for this path, and no
// ADR has been amended to ratify the departure, so it is recorded here where the next reader of the
// package will meet it rather than left to be mistaken for an oversight:
//
//   - `specs/decisions/cloud/identity-access/20261012-repository-credential-capability-vocabulary.md`
//     fixes "只组装一条固定的 clone 命令行，环境是 7 变量白名单". The environment half is preserved
//     exactly — the allow-list is untouched and no variable is added to it. What changes is that one
//     further `-c` key may follow `http.proxy=` on the command line, and it comes from deployment
//     configuration alone.
//   - `specs/decisions/cloud/identity-access/20261013-push-capability-vocabulary-extension.md` lists the
//     write direction's egress as `http.followRedirects=false` / `http.proxy=` / `http.sslVerify=true`
//     and requires it to reuse "同一份白名单，不新增环境变量". Both halves hold: no environment variable
//     is added, and the key is spliced into the one shared argument list, so the read and write
//     directions still cannot drift.
//
// The core test case `specs/test-cases/node/repository/git-credential-execution-isolation.md` records the
// obligation this closes — "仍缺…代理配置的独立断言" — at `Partial`. Its evidence row is deliberately left
// untouched until the decisions themselves are amended.
//
// # Why this travels on the command line
//
// The Node's marketplace path solves the same problem by carrying these keys in `GIT_CONFIG_KEY_n`
// environment variables, precisely so a credential-bearing proxy URL stays out of a logged argv. The
// executor cannot use that route: its environment is the allow-list `gitEnvironment` builds from nothing,
// and adding a variable to it would break the invariant that makes a clone's configuration auditable at a
// glance. It uses `-c` instead and refuses a proxy URL carrying any userinfo, which reaches the same end
// by construction: a proxy that needs a credential is refused rather than passed through. The core test
// case requires that credentials never appear in 代理配置, so that is the honest boundary for this option
// rather than an oversight in it.
//
// ProxyURL and Hosts are deployment configuration only. Nothing a clone job, an authorization or a
// repository carries can reach them, and no request-side surface is added for them: the core test case
// forbids 允许请求指定 helper 或代理.
type GitEgress struct {
	// ProxyURL is the proxy Git dials, for example `http://172.17.0.1:17890`. It must carry no userinfo,
	// because it becomes part of the command line.
	ProxyURL string
	// Hosts are the bare, lowercased hostnames — for example `github.com` — that may be reached through
	// ProxyURL. A host is matched without its port, so a deployment never has to enumerate ports. Every
	// other address is dialed directly, which is what keeps internal traffic structurally out of the
	// proxy rather than out of it by an exclusion list someone has to keep current.
	Hosts []string
}

// NewGitEgress validates a deployment's egress policy and returns the disabled policy for empty input.
//
// Both halves are required or neither: a proxy URL with no hosts would route everything, and hosts with
// no proxy URL would silently do nothing. Either reading of a half-declared policy is a deployment that
// does not mean what it says, so it is refused — the same rule every other half-declared capability in
// this package follows. Blank entries in hosts are dropped rather than refused, so a trailing comma in a
// list is not a startup failure.
func NewGitEgress(proxyURL string, hosts []string) (GitEgress, error) {
	egress := GitEgress{ProxyURL: strings.TrimSpace(proxyURL)}
	for _, host := range hosts {
		if trimmed := strings.TrimSpace(host); trimmed != "" {
			egress.Hosts = append(egress.Hosts, trimmed)
		}
	}
	if err := egress.validate(); err != nil {
		return GitEgress{}, err
	}
	return egress, nil
}

// validate refuses an egress policy the executor could not apply as declared.
//
// It is called from `gitDeployment.validate`, so all three constructors refuse a bad policy at startup
// for free, before any job could be accepted under it.
//
//nolint:gocritic // Value receiver on purpose, for the reason gitDeployment.validate has one: this reads a snapshot of the deployment's own configuration, and there is nothing here a caller could write back through.
func (e GitEgress) validate() error {
	if e.ProxyURL == "" && len(e.Hosts) == 0 {
		return nil
	}
	if e.ProxyURL == "" {
		return errors.New("git egress names hosts but no proxy URL to reach them through")
	}
	if len(e.Hosts) == 0 {
		return errors.New("git egress names a proxy URL but no host to use it for")
	}
	if _, err := e.proxyEndpoint(); err != nil {
		return err
	}
	for _, host := range e.Hosts {
		if err := validateEgressHost(host); err != nil {
			return err
		}
	}
	return nil
}

// proxyEndpoint parses the proxy URL and constrains what it may be.
//
// Every constraint is about what the value becomes rather than about the proxy: it is spliced into a `-c`
// entry on a command line, so whitespace or a control character could split or terminate that entry
// instead of traveling inside it, and a userinfo would put a credential into argv and into any log of it.
// The URL is otherwise not rewritten — what the deployment declared is what Git is handed.
func (e GitEgress) proxyEndpoint() (*url.URL, error) {
	if strings.ContainsFunc(e.ProxyURL, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
		return nil, errors.New("git egress proxy URL must not contain whitespace or control characters")
	}
	endpoint, err := url.Parse(e.ProxyURL)
	if err != nil {
		return nil, errors.New("git egress proxy URL is not a URL")
	}
	if endpoint.Scheme != "http" && endpoint.Scheme != "https" {
		return nil, errors.New("git egress proxy URL must be an http or https URL")
	}
	if endpoint.Host == "" {
		return nil, errors.New("git egress proxy URL names no host")
	}
	if endpoint.User != nil {
		return nil, errors.New("git egress proxy URL must not carry credentials")
	}
	if endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return nil, errors.New("git egress proxy URL must not carry a query or a fragment")
	}
	return endpoint, nil
}

// validateEgressHost constrains one allow-listed host to a bare, lowercased hostname.
//
// A bare name is required because the value becomes part of a Git configuration key:
// `http.https://<host>.proxy`. A slash, a colon, a `?` or a `#` would make that key name something other
// than the host, and the entry would then match nothing — or match an address the operator did not read.
func validateEgressHost(host string) error {
	if host == "" {
		return errors.New("git egress host must not be blank")
	}
	if strings.ToLower(host) != host {
		return fmt.Errorf("git egress host %q must be lowercase", host)
	}
	if strings.ContainsAny(host, " \t\n\r/:@?#[]") {
		return fmt.Errorf("git egress host %q is not a bare hostname", host)
	}
	return nil
}

// arguments is the Git command-line configuration that routes one address through the proxy, or nothing
// when this policy does not apply to that address.
//
// It yields no arguments for a disabled policy, for an address Git would not dial over https, and for any
// host the deployment did not list. That last case is what keeps the executor's internal traffic out of
// the proxy: Cloud, the internal Git Server and the Credential Service are reached by hosts this policy
// does not name, so they are never routed through it and no exclusion list has to be kept in step with
// them.
//
// An enabled policy for a listed host always yields exactly one key,
// `http.https://<host>.proxy=<url>`, which outranks the generic `http.proxy=` the shared arguments set.
// It is deliberately never emitted empty: an empty URL-specific entry also outranks `http.proxy=`, and
// being *more* specific it would win — so writing one would enable proxy use rather than disable it,
// inverting what an unlisted or disabled policy means.
func (e GitEgress) arguments(address string) ([]string, error) {
	if e.ProxyURL == "" {
		return nil, nil
	}
	if _, err := e.proxyEndpoint(); err != nil {
		return nil, err
	}
	target, err := url.Parse(address)
	if err != nil || target.Scheme != "https" || target.Host == "" {
		return nil, nil
	}
	if !slices.Contains(e.Hosts, strings.ToLower(target.Hostname())) {
		return nil, nil
	}
	// The port is kept when the address spells a non-default one, because Git normalizes only the default
	// port out of a URL-specific key: `https://github.com:8443` would not be matched by the bare-host key.
	return []string{"-c", "http.https://" + strings.ToLower(target.Host) + ".proxy=" + e.ProxyURL}, nil
}
