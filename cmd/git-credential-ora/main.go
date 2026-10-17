// Command git-credential-ora is the fixed Git credential helper through which the Repo Executor hands
// Git the credential it redeemed for one task.
//
// The executor configures it as the only helper for the external remote:
//
//	-c credential.helper= -c "credential.helper=/usr/local/bin/git-credential-ora <material-dir>"
//	-c credential.useHttpPath=true
//
// so Git runs `git-credential-ora <material-dir> get|store|erase` and writes the request on standard
// input. For `get` the helper answers with the staged username and password only when the request is
// for https and its host and path normalize (cloneauth.NormalizeRepository) to exactly the staged
// scope; for anything else it prints nothing, which the protocol reads as "no credential". This keeps
// a redirect, a submodule or a second repository on the same host from receiving the secret, without
// relying on Git's redirect behavior staying as documented.
//
// `store` and `erase` do nothing: the material belongs to one task and the executor destroys it.
// Nothing derived from the material is ever written to standard error, which Git copies into its own
// diagnostics.
package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/wanglongan587/cloud/internal/cloneauth"
	"github.com/wanglongan587/cloud/internal/credentialmaterial"
)

// maxRequestBytes bounds the request read from Git. A real request is a few short lines.
const maxRequestBytes = 64 << 10

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		// The message names the failure, never the material or the request.
		fmt.Fprintln(os.Stderr, "git-credential-ora:", err)
		os.Exit(1)
	}
}

// run is the whole program with its streams injected, so the protocol behavior is tested directly.
//
// An error means the helper could not do its job (bad arguments, unreadable material); a request it
// declines is not an error and produces no output at all.
func run(args []string, stdin io.Reader, stdout io.Writer) error {
	if len(args) != 2 {
		return errors.New("usage: git-credential-ora <material-dir> get|store|erase")
	}
	dir, operation := args[0], args[1]
	// The request is drained on every operation first: Git writes it and waits, and leaving it unread
	// risks a broken pipe on Git's side.
	request, err := readRequest(stdin)
	if err != nil {
		return err
	}
	switch operation {
	case "get":
	case "store", "erase":
		return nil
	default:
		return errors.New("unknown operation")
	}
	requested, ok := request.repository()
	if !ok {
		return nil
	}
	scope, err := credentialmaterial.ReadScope(dir)
	if err != nil {
		return fmt.Errorf("read staged scope: %w", err)
	}
	if requested != scope.Repository() {
		return nil
	}
	username, password, err := credentialmaterial.ReadSecret(dir)
	if err != nil {
		return fmt.Errorf("read staged credential: %w", err)
	}
	if _, err := fmt.Fprintf(stdout, "username=%s\npassword=%s\n", username, password); err != nil {
		return errors.New("write the credential to git")
	}
	return nil
}

// credentialRequest is the subset of Git's request the scope check compares.
type credentialRequest struct {
	protocol, host, path string
	duplicate            bool
}

// repository returns the normalized repository the request names, or false when the request cannot
// be matched: not https, no host or path, a repeated attribute, or a URL normalization refuses. A
// missing path is never read as "the whole host".
func (r credentialRequest) repository() (string, bool) {
	if r.duplicate || r.protocol != "https" || r.host == "" || r.path == "" {
		return "", false
	}
	normalized, err := cloneauth.NormalizeRepository("https://" + r.host + "/" + r.path)
	if err != nil {
		return "", false
	}
	return normalized, true
}

// readRequest reads attributes up to a blank line or end of input. Unknown attributes are ignored so
// a newer Git that adds one keeps working; a repeated protocol, host or path is remembered so the
// request is declined rather than resolved by position.
func readRequest(stdin io.Reader) (credentialRequest, error) {
	var request credentialRequest
	scanner := bufio.NewScanner(io.LimitReader(stdin, maxRequestBytes))
	seen := map[string]bool{}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			break
		}
		key, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		var target *string
		switch key {
		case "protocol":
			target = &request.protocol
		case "host":
			target = &request.host
		case "path":
			target = &request.path
		default:
			continue
		}
		if seen[key] {
			request.duplicate = true
		}
		seen[key] = true
		*target = value
	}
	if err := scanner.Err(); err != nil {
		return credentialRequest{}, errors.New("the credential request could not be read")
	}
	return request, nil
}
