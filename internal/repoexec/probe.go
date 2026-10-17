package repoexec

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/wanglongan587/cloud/internal/cloneauth"
)

// receivePackAdvertisement is the first pkt-line payload of a smart HTTP receive-pack advertisement.
const receivePackAdvertisement = "# service=git-receive-pack"

// probe reads one ref advertisement and nothing else (ADR D7): no objects are downloaded and no
// update command is ever sent, which is what lets a probe authorization be read-only.
func (t *taskRun) probe(ctx context.Context) Result {
	if t.task.Service == cloneauth.ServiceReceivePack {
		return t.probeReceivePack(ctx)
	}
	external, refusal := t.externalRepository(ctx)
	if refusal != nil {
		return *refusal
	}
	args := []string{"ls-remote", "--refs", "--", external}
	if t.task.TargetRef != "" {
		args = append(args, t.task.TargetRef)
	}
	o := t.run(ctx, args...)
	if !o.ok() {
		code := externalFailure(o.stderr, t.anonymous)
		if code == CodeFetchFailed || code == CodeUpstreamBranchMissing {
			code = CodeVerificationFailed
		}
		return t.fail(ctx, code, &o)
	}
	refs, ok := parseLsRemote(o.stdout)
	if o.stdoutCut || !ok {
		return failed(CodeVerificationFailed, "the ref advertisement is too large or unreadable")
	}
	return Result{Classification: Applied, Refs: refs}
}

// probeReceivePack requests the receive-pack advertisement with the redeemed credential and hangs up
// as soon as its first pkt-line identifies it. The Go client dials only addresses the egress policy
// admits at dial time and never follows a redirect.
func (t *taskRun) probeReceivePack(ctx context.Context) Result {
	if _, refusal := t.externalRepository(ctx); refusal != nil {
		return *refusal
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(t.task.Repository, "/")+"/info/refs?service=git-receive-pack", http.NoBody)
	if err != nil {
		return failed(CodeVerificationFailed, "probe request could not be built")
	}
	request.Header.Set("User-Agent", "git/ora-repo-executor")
	request.Header.Set("Accept", "application/x-git-receive-pack-advertisement")
	if !t.anonymous {
		request.SetBasicAuth(t.credential.Username, t.credential.Token)
	}
	response, err := t.e.probe.Do(request)
	if err != nil {
		switch {
		case errors.Is(err, ErrEgressDenied):
			return failed(CodeEgressDenied, "repository address is not allowed")
		case errors.Is(context.Cause(ctx), errTimeout):
			return failed(CodeTimeout, "task exceeded its timeout")
		}
		return failed(CodeVerificationFailed, redact(err.Error(), t.secrets))
	}
	defer func() { _ = response.Body.Close() }()
	result := Result{HTTPStatus: response.StatusCode}
	switch {
	case response.StatusCode == http.StatusUnauthorized, response.StatusCode == http.StatusForbidden:
		result.Classification, result.ErrorCode = Failed, CodePushDenied
		result.Detail = "the repository refused the receive-pack advertisement"
		return result
	case response.StatusCode != http.StatusOK:
		result.Classification, result.ErrorCode = Failed, CodeVerificationFailed
		result.Detail = "unexpected HTTP status " + strconv.Itoa(response.StatusCode)
		return result
	}
	first, err := readPktLine(bufio.NewReader(io.LimitReader(response.Body, 1<<16)))
	if err != nil || strings.TrimSuffix(first, "\n") != receivePackAdvertisement ||
		!strings.HasPrefix(response.Header.Get("Content-Type"), "application/x-git-receive-pack-advertisement") {
		result.Classification, result.ErrorCode = Failed, CodeVerificationFailed
		result.Detail = "the response is not a receive-pack advertisement"
		return result
	}
	result.Classification, result.Advertised = Applied, true
	return result
}

// readPktLine reads one pkt-line payload.
func readPktLine(r *bufio.Reader) (string, error) {
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return "", err
	}
	n, err := strconv.ParseUint(string(header[:]), 16, 16)
	if err != nil || n < 4 {
		return "", errors.New("invalid pkt-line")
	}
	payload := make([]byte, n-4)
	if _, err := io.ReadFull(r, payload); err != nil {
		return "", err
	}
	return string(payload), nil
}
