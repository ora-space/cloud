package main

import (
	"os"
	"strings"
	"testing"

	"github.com/wanglongan587/cloud/internal/credentialmaterial"
)

const (
	testExecutionID = "6f1d0a52-1c3b-4c7e-9a10-2b8f4d6e0c31"
	testHost        = "git.example.com"
	testToken       = "ghp_this-is-a-generated-test-token"
)

// stage writes one execution's material into a fresh root and returns the root.
func stage(t *testing.T, host, token string) string {
	t.Helper()
	root := t.TempDir()
	if err := credentialmaterial.Stage(root, testExecutionID, token, host); err != nil {
		t.Fatalf("stage credential material: %v", err)
	}
	return root
}

func TestRunDeliversTheStagedTokenForTheStagedHost(t *testing.T) {
	root := stage(t, testHost, testToken)
	var stdout strings.Builder
	err := run([]string{root, testExecutionID, "get"},
		strings.NewReader("protocol=https\nhost="+testHost+"\npath=ora/private-repo.git\n\n"), &stdout)
	if err != nil {
		t.Fatalf("a correctly scoped request was refused: %v", err)
	}
	want := "username=" + tokenUsername + "\npassword=" + testToken + "\n"
	if stdout.String() != want {
		t.Fatalf("helper output = %q, want %q", stdout.String(), want)
	}
	// The username is a fixed label, not a second value read out of the material: a helper whose output
	// depended on more than the one staged secret would have a second thing to get wrong.
	if strings.Contains(tokenUsername, testToken) {
		t.Fatal("the username is derived from the token")
	}
}

func TestRunMatchesTheHostCaseInsensitively(t *testing.T) {
	root := stage(t, testHost, testToken)
	var stdout strings.Builder
	err := run([]string{root, testExecutionID, "get"},
		strings.NewReader("host="+strings.ToUpper(testHost)+"\n\n"), &stdout)
	if err != nil {
		t.Fatalf("a request for the staged host in another case was refused: %v", err)
	}
	if !strings.Contains(stdout.String(), "password="+testToken) {
		t.Fatalf("helper output = %q, want the staged token", stdout.String())
	}
}

func TestRunRefusesARequestForAnotherHost(t *testing.T) {
	root := stage(t, testHost, testToken)
	var stdout strings.Builder
	err := run([]string{root, testExecutionID, "get"},
		strings.NewReader("host=git.attacker.example\n\n"), &stdout)
	if err == nil {
		t.Fatal("a request for a host the material is not scoped to was answered")
	}
	if stdout.Len() != 0 {
		t.Fatalf("a refused request still wrote %q to standard output", stdout.String())
	}
	// The refusal must not name the host it was scoped to: that is a fact about the deployment, and
	// Git's stderr ends up in the clone log.
	if strings.Contains(err.Error(), testHost) {
		t.Fatalf("the refusal discloses the staged scope: %v", err)
	}
}

func TestRunRefusesWhatItCannotDeliver(t *testing.T) {
	root := stage(t, testHost, testToken)
	empty := t.TempDir()
	cases := []struct {
		name  string
		args  []string
		stdin string
	}{
		{"no material root", []string{empty, testExecutionID, "get"}, "host=" + testHost + "\n\n"},
		{"an execution id that is not a uuid", []string{root, "not-a-uuid", "get"}, "host=" + testHost + "\n\n"},
		{"an execution id that escapes the root", []string{root, "../../etc", "get"}, "host=" + testHost + "\n\n"},
		{"a request that names no host", []string{root, testExecutionID, "get"}, "protocol=https\n\n"},
		{"an empty request", []string{root, testExecutionID, "get"}, ""},
		{"too few arguments", []string{root, testExecutionID}, "host=" + testHost + "\n\n"},
		{"too many arguments", []string{root, testExecutionID, "get", "extra"}, "host=" + testHost + "\n\n"},
		{"an operation Git does not define", []string{root, testExecutionID, "fill"}, "host=" + testHost + "\n\n"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			var stdout strings.Builder
			err := run(testCase.args, strings.NewReader(testCase.stdin), &stdout)
			if err == nil {
				t.Fatal("the helper answered a request it could not satisfy")
			}
			if stdout.Len() != 0 {
				t.Fatalf("a refused request still wrote %q to standard output", stdout.String())
			}
			if strings.Contains(err.Error(), testToken) {
				t.Fatalf("the refusal carries the token: %v", err)
			}
		})
	}
}

// TestRunAnswersStoreAndEraseWithoutReadingTheMaterial is the property that keeps the helper from
// being a credential store: Git calls `store` after a successful authentication and `erase` after a
// rejected one, and both must be no-ops rather than a place a token could be persisted.
func TestRunAnswersStoreAndEraseWithoutReadingTheMaterial(t *testing.T) {
	for _, operation := range []string{"store", "erase"} {
		t.Run(operation, func(t *testing.T) {
			// The root is empty, so an implementation that tried to read would fail; succeeding is the
			// proof that it did not.
			var stdout strings.Builder
			err := run([]string{t.TempDir(), testExecutionID, operation},
				strings.NewReader("host="+testHost+"\nusername=x-access-token\npassword="+testToken+"\n\n"), &stdout)
			if err != nil {
				t.Fatalf("%s was not a no-op: %v", operation, err)
			}
			if stdout.Len() != 0 {
				t.Fatalf("%s wrote %q to standard output", operation, stdout.String())
			}
		})
	}
}

func TestRunRefusesMaterialThatWasNotStagedByTheExecutor(t *testing.T) {
	root := stage(t, testHost, testToken)
	// A wider mode means something other than the executor created or changed the material, and the
	// safe answer is to deliver nothing.
	tokenPath, err := credentialmaterial.TokenPath(root, testExecutionID)
	if err != nil {
		t.Fatalf("resolve the token path: %v", err)
	}
	if err := os.Chmod(tokenPath, 0o644); err != nil {
		t.Fatalf("widen the material's mode: %v", err)
	}
	var stdout strings.Builder
	if err := run([]string{root, testExecutionID, "get"}, strings.NewReader("host="+testHost+"\n\n"), &stdout); err == nil {
		t.Fatal("material with a widened mode was delivered")
	}
	if stdout.Len() != 0 {
		t.Fatalf("a refused request still wrote %q to standard output", stdout.String())
	}
}
