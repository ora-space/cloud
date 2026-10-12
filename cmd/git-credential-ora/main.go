// Command git-credential-ora is the deployment-fixed Git credential helper that hands Git the token
// the credential executor staged for one clone.
//
// It is a Git credential helper in the protocol sense — Git runs it with `get`, `store` or `erase`
// and writes the request on standard input — but it is not a credential store. It reads one file
// whose location the executor chose, refuses to read anything else, and forgets everything the
// moment it exits.
//
// The deployment fixes the program's path, and the executor fixes its arguments; no part of a clone
// request reaches it. That split is the point: the helper is on the path Git executes, so the only
// thing it must be trusted to do is read a file it was told to read and print it in the protocol's
// shape. Every decision about whether the token may be used at all was already made, and the token
// does not exist on disk unless it was.
//
// Usage, as configured by the executor through `-c credential.helper`:
//
//	git-credential-ora <material-root> <execution-id> <get|store|erase>
//
// `store` and `erase` succeed without doing anything. Git calls `store` after a successful
// authentication and `erase` after a rejected one; both are opportunities for a helper to persist or
// discard a credential, and this helper does neither, because the material is the executor's and its
// lifetime is the clone's.
package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/wanglongan587/cloud/internal/credentialmaterial"
)

// tokenUsername is the username Git is told to use.
//
// The stored secret is a user-managed HTTPS Git token, which is presented as the password; the
// username is a fixed label. `x-access-token` is the form GitHub documents for token authentication
// and is accepted by the other forges this product targets. It is deliberately not read from the
// material: a username is not a secret, and letting the file carry a second value would make the
// helper's output depend on more than the one thing it is supposed to deliver.
const tokenUsername = "x-access-token"

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		// Git shows a helper's stderr to the user and puts it in its own error output, which becomes
		// part of the clone log. Nothing derived from the material may appear there, so the message
		// names the failure and never the content.
		fmt.Fprintln(os.Stderr, "git-credential-ora:", err)
		os.Exit(1)
	}
}

// run is the whole program, with its streams injected so the protocol behavior can be tested
// directly.
func run(args []string, stdin io.Reader, stdout io.Writer) error {
	if len(args) != 3 {
		return errors.New("expected a material root, an execution id and an operation")
	}
	root, executionID, operation := args[0], args[1], args[2]
	// The request is consumed on every operation before anything else happens. Git writes it and
	// waits; leaving it unread risks a broken pipe on Git's side, and reading it first means a
	// malformed request cannot change what this program does, because nothing in it is used.
	request, err := readRequest(stdin)
	if err != nil {
		return err
	}
	switch operation {
	case "get":
	case "store", "erase":
		// Git is telling this helper about an outcome. There is nothing to remember and nothing to
		// forget: the material belongs to one clone and the executor destroys it.
		return nil
	default:
		return errors.New("unknown operation")
	}
	// The host check is what keeps a redirect from carrying the token somewhere the executor never
	// authorized. `http.followRedirects=false` and the protocol constraints already say Git will not
	// follow one; this makes the refusal happen at the point the secret would leave, so the guarantee
	// does not rest on Git's behavior staying as documented.
	scope, err := credentialmaterial.ReadScope(root, executionID)
	if err != nil {
		return err
	}
	if !strings.EqualFold(strings.TrimSpace(scope), request.host) {
		return errors.New("credential material is not scoped to the requested host")
	}
	token, err := credentialmaterial.Read(root, executionID)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(stdout, "username=%s\npassword=%s\n", tokenUsername, token); err != nil {
		return err
	}
	return nil
}

// credentialRequest is the subset of Git's credential request this helper reads.
//
// Only `host` is inspected. The rest is read so the stream is drained and so the protocol shape is
// visible in one place; no other field influences the outcome.
type credentialRequest struct {
	host string
}

// readRequest consumes Git's request. Unknown keys are ignored rather than refused: the request is
// Git's own output, not caller input, and a newer Git that adds a key must not stop authentication.
func readRequest(stdin io.Reader) (credentialRequest, error) {
	var request credentialRequest
	scanner := bufio.NewScanner(stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			break
		}
		key, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		if key == "host" {
			request.host = strings.ToLower(strings.TrimSpace(value))
		}
	}
	if err := scanner.Err(); err != nil {
		return credentialRequest{}, errors.New("the credential request could not be read")
	}
	if request.host == "" {
		return credentialRequest{}, errors.New("the credential request named no host")
	}
	return request, nil
}
