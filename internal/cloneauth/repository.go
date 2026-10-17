package cloneauth

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
)

// ErrRepository reports a repository URL NormalizeRepository refuses.
var ErrRepository = errors.New("cloneauth: repository is not a usable https URL")

// NormalizeRepository reduces an external repository URL to the one spelling that Binding validation,
// credential scopes, the executor and the credential helper all compare byte for byte.
//
// The rules make the spellings forges accept for one repository equal, and nothing else:
//
//   - the scheme must be https: the credential is an HTTPS credential and the executor speaks nothing
//     else;
//   - userinfo, query and fragment are refused rather than dropped: userinfo would put a secret in a
//     signed field, and a query or fragment names something other than the repository;
//   - the host is lowercased (DNS is case-insensitive) and the default ":443" removed; any other port
//     is kept, because two ports are two servers. Only ASCII host names and IP literals are accepted;
//   - the path keeps its case (forge paths are case-sensitive), loses one trailing "/" and then one
//     trailing ".git", and must still name something;
//   - empty, "." and ".." segments and any percent sign are refused. An encoded "/" or "." would let
//     two different strings name one path after a server decodes them; refusing every escape keeps the
//     result a single spelling, and Git hosting URLs never need one.
//
// The result is "https://host[:port]/path" and NormalizeRepository is idempotent on it, so a signed
// repository field is valid exactly when normalizing it returns it unchanged.
func NormalizeRepository(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", fmt.Errorf("%w: it is empty", ErrRepository)
	}
	for i := 0; i < len(trimmed); i++ {
		c := trimmed[i]
		if c <= ' ' || c == 0x7f || c == '\\' {
			return "", fmt.Errorf("%w: it contains whitespace, a control character or a backslash", ErrRepository)
		}
	}
	if strings.ContainsAny(trimmed, "?#") {
		return "", fmt.Errorf("%w: it carries a query or fragment", ErrRepository)
	}
	if strings.Contains(trimmed, "%") {
		return "", fmt.Errorf("%w: it carries a percent-encoded character", ErrRepository)
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return "", fmt.Errorf("%w: it is not a URL", ErrRepository)
	}
	if parsed.Scheme != "https" || parsed.Opaque != "" {
		return "", fmt.Errorf("%w: the scheme is not https", ErrRepository)
	}
	if parsed.User != nil || strings.Contains(parsed.Host, "@") {
		return "", fmt.Errorf("%w: it carries user information", ErrRepository)
	}
	host, err := normalizeHost(parsed.Host)
	if err != nil {
		return "", err
	}
	path, err := normalizePath(parsed.Path)
	if err != nil {
		return "", err
	}
	return "https://" + host + path, nil
}

// normalizeHost lowercases the host, validates it and drops the default port.
func normalizeHost(hostport string) (string, error) {
	if hostport == "" {
		return "", fmt.Errorf("%w: it names no host", ErrRepository)
	}
	host, port := hostport, ""
	if strings.HasPrefix(hostport, "[") {
		end := strings.IndexByte(hostport, ']')
		if end < 0 {
			return "", fmt.Errorf("%w: the IPv6 host is not closed", ErrRepository)
		}
		host = hostport[:end+1]
		rest := hostport[end+1:]
		if rest != "" {
			var found bool
			if port, found = strings.CutPrefix(rest, ":"); !found {
				return "", fmt.Errorf("%w: unexpected text after the host", ErrRepository)
			}
			if port == "" {
				return "", fmt.Errorf("%w: the port is empty", ErrRepository)
			}
		}
	} else if before, after, found := strings.Cut(hostport, ":"); found {
		host, port = before, after
		if port == "" {
			return "", fmt.Errorf("%w: the port is empty", ErrRepository)
		}
	}
	host = strings.ToLower(host)
	switch {
	case strings.HasPrefix(host, "["):
		addr, err := netip.ParseAddr(strings.Trim(host, "[]"))
		if err != nil || !addr.Is6() || addr.Zone() != "" {
			return "", fmt.Errorf("%w: the host is not an IPv6 literal", ErrRepository)
		}
		host = "[" + addr.String() + "]"
	case !validHostName(host):
		return "", fmt.Errorf("%w: the host is not an ASCII host name", ErrRepository)
	}
	if port == "" || port == "443" {
		return host, nil
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65535 || strconv.Itoa(number) != port {
		return "", fmt.Errorf("%w: the port is not a number in 1..65535", ErrRepository)
	}
	return host + ":" + port, nil
}

// validHostName accepts dot-separated labels of lowercase letters, digits and inner hyphens. A trailing
// dot is refused rather than stripped so that a fully qualified spelling is not silently equal.
func validHostName(host string) bool {
	if host == "" || len(host) > 253 {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
				return false
			}
		}
	}
	return true
}

// normalizePath removes one trailing "/" and one trailing ".git" and refuses a path that is then
// empty, has an empty, "." or ".." segment, or still ends in ".git" (which would make the result not
// idempotent).
func normalizePath(path string) (string, error) {
	path = strings.TrimSuffix(path, "/")
	path = strings.TrimSuffix(path, ".git")
	if path == "" || path == "/" || !strings.HasPrefix(path, "/") {
		return "", fmt.Errorf("%w: it names no repository path", ErrRepository)
	}
	for i := 0; i < len(path); i++ {
		if !pathByte(path[i]) {
			return "", fmt.Errorf("%w: the path has a character outside the URL path set", ErrRepository)
		}
	}
	for _, segment := range strings.Split(path[1:], "/") {
		switch segment {
		case "", ".", "..":
			return "", fmt.Errorf("%w: the path has an empty, \".\" or \"..\" segment", ErrRepository)
		}
	}
	if strings.HasSuffix(path, ".git") {
		return "", fmt.Errorf("%w: the path ends in more than one \".git\"", ErrRepository)
	}
	return path, nil
}

// pathByte reports whether c may appear in a normalized path: RFC 3986 pchar without percent-encoding,
// plus "/". Anything else (non-ASCII, quotes, angle brackets) would need an escape to be a URL.
func pathByte(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	default:
		return strings.IndexByte("-._~!$&'()*+,;=:@/", c) >= 0
	}
}
