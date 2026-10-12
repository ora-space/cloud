package cloneauth

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// ErrRepository reports a repository reference that cannot be a clone target.
var ErrRepository = errors.New("repository is not a usable https reference")

// CanonicalRepository reduces a repository reference to the one spelling an authorization binds and
// the executor checks against.
//
// The binding carries a repository identity, and both the issuer and the executor have to compute the
// same string from whatever the user typed, or a legitimate clone would be refused as a binding
// mismatch — or, worse, a request for a different repository could be made to match. Normalization
// therefore lives here, next to the field it defines, rather than in either caller.
//
// The rules are the smallest set that makes two spellings of one repository equal without making two
// repositories equal:
//
//   - the scheme must be `https`. The credentialed path is HTTPS-only by decision: the token is an
//     HTTPS credential, and accepting `ssh://` or `git://` here would let an authorization for one
//     transport be spent on another.
//   - the host is lowercased, because DNS names are case-insensitive, and the port is preserved
//     verbatim when present, because two ports are two servers.
//   - the path keeps its case, because forge paths are case-sensitive, and loses one trailing `/` and
//     one trailing `.git`, which are the two spellings every forge accepts for the same repository.
//   - userinfo, query and fragment are refused rather than dropped. A URL carrying a credential in
//     its userinfo would put a secret in a field that is signed into an authorization, and a query or
//     fragment names something other than the repository.
//
// Nothing here resolves the reference or contacts the network: an authorization binds what was
// requested, and whether that repository exists is the clone's answer to give.
func CanonicalRepository(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", fmt.Errorf("%w: it is empty", ErrRepository)
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return "", fmt.Errorf("%w: it is not a valid URL", ErrRepository)
	}
	if parsed.Scheme != "https" {
		return "", fmt.Errorf("%w: scheme %q is not https", ErrRepository, parsed.Scheme)
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("%w: it names no host", ErrRepository)
	}
	if parsed.User != nil {
		return "", fmt.Errorf("%w: it carries user info", ErrRepository)
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("%w: it carries a query or fragment", ErrRepository)
	}
	host := strings.ToLower(parsed.Host)
	path := strings.TrimSuffix(parsed.EscapedPath(), "/")
	path = strings.TrimSuffix(path, ".git")
	if path == "" || path == "/" {
		return "", fmt.Errorf("%w: it names no repository path", ErrRepository)
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return "https://" + host + path, nil
}
