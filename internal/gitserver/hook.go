package gitserver

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/wanglongan587/cloud/internal/core"
)

// Environment names http-backend passes through receive-pack to the pre-receive hook. They are
// set by this server for each push; a client cannot set a process environment variable.
const (
	hookSocketEnv = "ORA_GIT_HOOK_SOCKET"
	pushTokenEnv  = "ORA_GIT_PUSH_TOKEN"
)

const hookTimeout = 30 * time.Second

// refUpdate is one pre-receive line plus facts about its new object that only the hook can read,
// because the pushed objects are still in Git's quarantine directory: the object's type, and
// whether the old value is its ancestor.
type refUpdate struct {
	Old         string `json:"old"`
	New         string `json:"new"`
	Ref         string `json:"ref"`
	NewType     string `json:"newType"`
	FastForward bool   `json:"fastForward"`
}

type hookRequest struct {
	Token       string      `json:"token"`
	PushOptions int         `json:"pushOptions"`
	Updates     []refUpdate `json:"updates"`
}

type hookResponse struct {
	Allow bool `json:"allow"`
}

// push is the authenticated context of one receive-pack request awaiting its pre-receive decision:
// the listener it arrived on, the sandbox scope (sandbox listener only), the execution or
// Operation it names, and the repository it addresses.
type push struct {
	listener                 listener
	scope                    core.GitRuntimeScope
	context, tenant, project string
}

// pushes maps one-time tokens to in-flight receive-pack requests. The token only links the hook
// back to its request; the decision itself is made again from Cloud state.
type pushes struct {
	mu     sync.Mutex
	active map[string]push
}

func (p *pushes) register(value *push) (token string, release func()) {
	token = rand.Text()
	p.mu.Lock()
	p.active[token] = *value
	p.mu.Unlock()
	return token, func() {
		p.mu.Lock()
		delete(p.active, token)
		p.mu.Unlock()
	}
}

func (p *pushes) lookup(token string) (push, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	value, ok := p.active[token]
	return value, ok
}

// decide re-runs the full Cloud authorization immediately before any ref is written, then admits
// the push only if the resulting rule set admits every update.
func (s *Service) decide(ctx context.Context, request hookRequest) bool {
	p, ok := s.pushes.lookup(request.Token)
	if !ok || request.PushOptions != 0 || len(request.Updates) == 0 {
		return false
	}
	grant, err := s.authorize(ctx, &p)
	if err != nil {
		return false
	}
	return admit(&grant, request.Updates, func(ref, commit string) (bool, error) {
		got, err := s.repos.resolve(ctx, p.tenant, p.project, ref)
		return got == commit, err
	})
}

func zeroID(s string) bool { return core.GitObjectID(s) && strings.Trim(s, "0") == "" }

// serveHooks answers pre-receive hooks over a Unix socket in the server's private runtime
// directory. Only processes running as this server's user can reach it.
func (s *Service) serveHooks(ctx context.Context, listener net.Listener) {
	var connections sync.WaitGroup
	defer connections.Wait()
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		connections.Add(1)
		go func() {
			defer connections.Done()
			defer func() { _ = conn.Close() }()
			_ = conn.SetDeadline(time.Now().Add(hookTimeout))
			var request hookRequest
			allow := false
			if err := json.NewDecoder(io.LimitReader(conn, 1<<20)).Decode(&request); err == nil {
				decisionCtx, cancel := context.WithTimeout(ctx, hookTimeout)
				allow = s.decide(decisionCtx, request)
				cancel()
			}
			_ = json.NewEncoder(conn).Encode(hookResponse{Allow: allow})
		}()
	}
}

// RunPreReceive is the pre-receive hook body. Git runs it inside receive-pack with the pushed
// objects quarantined; it reports the proposed updates to the server and exits non-zero unless
// the server admits all of them. The client only ever sees a generic rejection.
func RunPreReceive(stdin io.Reader, stderr io.Writer, git string) error {
	reject := func() error {
		_, _ = fmt.Fprintln(stderr, "ora: push rejected")
		return errors.New("push rejected")
	}
	request := hookRequest{Token: os.Getenv(pushTokenEnv)}
	if count := os.Getenv("GIT_PUSH_OPTION_COUNT"); count != "" && count != "0" {
		request.PushOptions = 1
	}
	scanner := bufio.NewScanner(stdin)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 3 || len(request.Updates) >= 64 {
			return reject()
		}
		u := refUpdate{Old: fields[0], New: fields[1], Ref: fields[2]}
		if core.GitObjectID(u.New) && !zeroID(u.New) {
			out, err := exec.Command(git, "cat-file", "-t", u.New).Output() //nolint:gosec // G204: u.New was validated as a hex object name; git is the deployment program.
			if err != nil {
				return reject()
			}
			u.NewType = strings.TrimSpace(string(out))
			if u.NewType == "commit" && core.GitObjectID(u.Old) && !zeroID(u.Old) {
				if u.FastForward, err = isAncestor(git, u.Old, u.New); err != nil {
					return reject()
				}
			}
		}
		request.Updates = append(request.Updates, u)
	}
	if scanner.Err() != nil {
		return reject()
	}
	conn, err := net.DialTimeout("unix", os.Getenv(hookSocketEnv), 5*time.Second) //nolint:gosec // G704: the socket path is set by this server for its own hook, not by a client.
	if err != nil {
		return reject()
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(hookTimeout + 5*time.Second))
	var response hookResponse
	if json.NewEncoder(conn).Encode(request) != nil || json.NewDecoder(conn).Decode(&response) != nil || !response.Allow {
		return reject()
	}
	return nil
}

// isAncestor reports whether old is an ancestor of next. An old value the repository does not
// hold is not an ancestor: the server's ref would not match it anyway.
func isAncestor(git, old, next string) (bool, error) {
	err := exec.Command(git, "merge-base", "--is-ancestor", old, next).Run() //nolint:gosec // G204: both are validated hex object names; git is the deployment program.
	var exit *exec.ExitError
	switch {
	case err == nil:
		return true, nil
	case errors.As(err, &exit):
		// 1: not an ancestor; 128: old is unknown or not a commit, so it cannot be the server's
		// current value either.
		return false, nil
	}
	return false, err
}

// hookScript execs the configured command; each argument is single-quoted for the shell.
func hookScript(command []string) string {
	quoted := make([]string, len(command))
	for i, arg := range command {
		quoted[i] = "'" + strings.ReplaceAll(arg, "'", `'\''`) + "'"
	}
	return "#!/bin/sh\nexec " + strings.Join(quoted, " ") + "\n"
}
