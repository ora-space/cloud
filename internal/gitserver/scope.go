package gitserver

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/wanglongan587/cloud/internal/core"
)

var errScope = errors.New("git-access certificate required")

// RuntimeScope accepts only the dedicated git-access purpose. WSS server and model-access
// certificates chain to the same CA, so the purpose segment is what keeps them out.
func RuntimeScope(r *http.Request) (core.GitRuntimeScope, error) {
	if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 || len(r.TLS.PeerCertificates) == 0 {
		return core.GitRuntimeScope{}, errScope
	}
	cert := r.TLS.PeerCertificates[0]
	if len(cert.URIs) != 1 {
		return core.GitRuntimeScope{}, errScope
	}
	u := cert.URIs[0]
	p := strings.Split(strings.Trim(u.Path, "/"), "/")
	if u.Scheme != "spiffe" || u.Host != "ora.local" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || len(p) != 7 || p[0] != "git-access" || p[1] != "tenant" || p[3] != "runtime" || p[5] != "generation" || uuid.Validate(p[2]) != nil || uuid.Validate(p[4]) != nil {
		return core.GitRuntimeScope{}, errScope
	}
	g, err := strconv.ParseInt(p[6], 10, 64)
	if err != nil || g <= 0 || strconv.FormatInt(g, 10) != p[6] {
		return core.GitRuntimeScope{}, errScope
	}
	return core.GitRuntimeScope{TenantID: p[2], WorkspaceID: p[4], Generation: g}, nil
}

// ExecutorIdentity is the Repo Executor's service SPIFFE ID. It proves only which component is
// calling; every repository permission comes from the Operation the request names.
const ExecutorIdentity = "spiffe://ora.local/repo-executor"

var errExecutor = errors.New("repo-executor certificate required")

// RequireExecutor accepts only a verified certificate whose sole URI is the executor identity.
// The service listener trusts only the executor CA, so this is the second of two checks.
func RequireExecutor(r *http.Request) error {
	if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 || len(r.TLS.PeerCertificates) == 0 {
		return errExecutor
	}
	cert := r.TLS.PeerCertificates[0]
	if len(cert.URIs) != 1 || cert.URIs[0].String() != ExecutorIdentity {
		return errExecutor
	}
	return nil
}
