package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wanglongan587/cloud/internal/credentialmaterial"
)

const (
	testExecutionID = "6f1d0a52-1c3b-4c7e-9a10-2b8f4d6e0c31"
	testHost        = "git.example.com"
	testPath        = "/ora/private-repo"
	testToken       = "ghp_this-is-a-generated-test-token"
)

// stage writes one execution's material into a fresh root and returns the root. The scope is the host
// and path the executor would have derived from the authorized repository, in the normalized spelling
// the material package stores.
func stage(t *testing.T, host, path, token string) string {
	t.Helper()
	root := t.TempDir()
	if err := credentialmaterial.Stage(root, testExecutionID, token, host, path); err != nil {
		t.Fatalf("stage credential material: %v", err)
	}
	return root
}

// gitRequest renders the credential request as `git credential fill` was observed to write it for an
// https URL with `credential.useHttpPath=true`: the host lowercased, and the path without a leading
// slash and in the spelling the URL carried (so a `.git` suffix is present if the URL had one). The
// tests use this spelling on purpose — it is the one the helper has to reconcile with the normalized
// scope.
func gitRequest(host, path string) string {
	return "protocol=https\nhost=" + host + "\npath=" + path + "\n\n"
}

func TestRunDeliversTheStagedTokenForTheStagedRepository(t *testing.T) {
	root := stage(t, testHost, testPath, testToken)
	var stdout strings.Builder
	err := run([]string{root, testExecutionID, "get"}, strings.NewReader(gitRequest(testHost, "ora/private-repo.git")), &stdout)
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

// TestRunMatchesTheHostCaseInsensitivelyButNotThePath states the one asymmetry of the comparison: a
// host differing only in case is the same DNS name and matches, while a path differing only in case is
// a different repository on a case-sensitive forge and is refused.
func TestRunMatchesTheHostCaseInsensitivelyButNotThePath(t *testing.T) {
	root := stage(t, testHost, testPath, testToken)

	var delivered strings.Builder
	if err := run([]string{root, testExecutionID, "get"},
		strings.NewReader(gitRequest(strings.ToUpper(testHost), "ora/private-repo.git")), &delivered); err != nil {
		t.Fatalf("a request for the staged host in another case was refused: %v", err)
	}
	if !strings.Contains(delivered.String(), "password="+testToken) {
		t.Fatalf("helper output = %q, want the staged token", delivered.String())
	}

	var refused strings.Builder
	if err := run([]string{root, testExecutionID, "get"},
		strings.NewReader(gitRequest(testHost, "Ora/Private-Repo.git")), &refused); err == nil {
		t.Fatal("a request whose path differs only in case was answered")
	}
	if refused.Len() != 0 {
		t.Fatalf("a refused request still wrote %q to standard output", refused.String())
	}
}

// TestRunMatchesTheGitSuffixInBothDirections pins that removing one `.git` is symmetric: a scope
// staged without the suffix answers a request with it, and a scope staged with it answers a request
// without it. They are the same repository, so neither direction may be refused.
func TestRunMatchesTheGitSuffixInBothDirections(t *testing.T) {
	withSuffix := stage(t, testHost, "/ora/private-repo.git", testToken)
	if err := run([]string{withSuffix, testExecutionID, "get"},
		strings.NewReader(gitRequest(testHost, "ora/private-repo")), &strings.Builder{}); err != nil {
		t.Fatalf("a scope staged with .git refused the request without it: %v", err)
	}
	withoutSuffix := stage(t, testHost, "/ora/private-repo", testToken)
	if err := run([]string{withoutSuffix, testExecutionID, "get"},
		strings.NewReader(gitRequest(testHost, "ora/private-repo.git")), &strings.Builder{}); err != nil {
		t.Fatalf("a scope staged without .git refused the request with it: %v", err)
	}
}

// TestRunRefusesARequestThatDoesNotMatchTheScopedRepository is the adversarial table: every request
// that is not byte-for-byte the scoped repository after the one documented normalization is refused,
// and every request that is the same repository is delivered. The case name is in every failure
// message.
func TestRunRefusesARequestThatDoesNotMatchTheScopedRepository(t *testing.T) {
	root := stage(t, testHost, testPath, testToken)
	cases := []struct {
		name        string
		host        string
		path        string
		wantDeliver bool
	}{
		{"the same repository in Git's spelling", testHost, "ora/private-repo.git", true},
		{"the same repository without the .git suffix", testHost, "ora/private-repo", true},
		{"the same repository with a trailing slash", testHost, "ora/private-repo/", true},
		{"a second .git is not stripped twice", testHost, "ora/private-repo.git.git", false},
		{"a different repository on the same host", testHost, "ora/other-repo.git", false},
		{"a sub-path of the scoped repository", testHost, "ora/private-repo/sub", false},
		{"a super-path of the scoped repository", testHost, "ora", false},
		{"a case-differing path", testHost, "Ora/Private-Repo.git", false},
		{"a percent-encoded variant of the scoped path", testHost, "ora/private%2Drepo.git", false},
		{"a query in the request path", testHost, "ora/private-repo.git?ref=x", false},
		{"a fragment in the request path", testHost, "ora/private-repo.git#frag", false},
		{"the same path on another host", "git.attacker.example", "ora/private-repo.git", false},
		{"the scoped path under a host that only shares a suffix", "attacker-git.example.com", "ora/private-repo.git", false},
		{"no path at all", testHost, "", false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			var stdout strings.Builder
			err := run([]string{root, testExecutionID, "get"},
				strings.NewReader(gitRequest(testCase.host, testCase.path)), &stdout)
			if testCase.wantDeliver {
				if err != nil {
					t.Fatalf("%s: the scoped repository was refused: %v", testCase.name, err)
				}
				if !strings.Contains(stdout.String(), "password="+testToken) {
					t.Fatalf("%s: helper output = %q, want the staged token", testCase.name, stdout.String())
				}
				return
			}
			if err == nil {
				t.Fatalf("%s: a request outside the scope was answered", testCase.name)
			}
			if stdout.Len() != 0 {
				t.Fatalf("%s: a refused request still wrote %q to standard output", testCase.name, stdout.String())
			}
		})
	}
}

// TestRunRefusalDoesNotDiscloseTheStagedScope keeps the refusal from becoming a fact about the
// deployment in the clone log: Git shows a helper's stderr, so neither the staged host nor the staged
// path may appear in it.
func TestRunRefusalDoesNotDiscloseTheStagedScope(t *testing.T) {
	root := stage(t, testHost, testPath, testToken)
	var stdout strings.Builder
	err := run([]string{root, testExecutionID, "get"},
		strings.NewReader(gitRequest("git.attacker.example", "some/repo.git")), &stdout)
	if err == nil {
		t.Fatal("a request outside the scope was answered")
	}
	if strings.Contains(err.Error(), testHost) || strings.Contains(err.Error(), testPath) {
		t.Fatalf("the refusal discloses the staged scope: %v", err)
	}
}

// TestRunRefusesAScopeFileInAnOldOrCorruptFormat proves at the helper boundary what the material
// package proves in isolation: material staged by an older build — a bare host — and any damaged
// record are refused, never read as a scope.
func TestRunRefusesAScopeFileInAnOldOrCorruptFormat(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{"the old bare-host format", testHost},
		{"a record missing the path", "host=" + testHost},
		{"a record with an unknown field", "host=" + testHost + "\npath=" + testPath + "\nowner=someone"},
		{"a record with no equals sign", testHost + "\n" + testPath},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			root := stage(t, testHost, testPath, testToken)
			scopePath := filepath.Join(root, testExecutionID, credentialmaterial.ScopeFile)
			if err := os.WriteFile(scopePath, []byte(testCase.content), 0o600); err != nil {
				t.Fatalf("overwrite the scope record: %v", err)
			}
			var stdout strings.Builder
			if err := run([]string{root, testExecutionID, "get"},
				strings.NewReader(gitRequest(testHost, "ora/private-repo.git")), &stdout); err == nil {
				t.Fatalf("%s: material with a scope in the wrong format was delivered", testCase.name)
			}
			if stdout.Len() != 0 {
				t.Fatalf("%s: a refused request still wrote %q to standard output", testCase.name, stdout.String())
			}
		})
	}
}

func TestRunRefusesWhatItCannotDeliver(t *testing.T) {
	root := stage(t, testHost, testPath, testToken)
	empty := t.TempDir()
	cases := []struct {
		name  string
		args  []string
		stdin string
	}{
		{"no material root", []string{empty, testExecutionID, "get"}, gitRequest(testHost, "ora/private-repo.git")},
		{"an execution id that is not a uuid", []string{root, "not-a-uuid", "get"}, gitRequest(testHost, "ora/private-repo.git")},
		{"an execution id that escapes the root", []string{root, "../../etc", "get"}, gitRequest(testHost, "ora/private-repo.git")},
		{"a request that names no host", []string{root, testExecutionID, "get"}, "protocol=https\npath=ora/private-repo.git\n\n"},
		{"a request that names no path", []string{root, testExecutionID, "get"}, "protocol=https\nhost=" + testHost + "\n\n"},
		{"an empty request", []string{root, testExecutionID, "get"}, ""},
		{"too few arguments", []string{root, testExecutionID}, gitRequest(testHost, "ora/private-repo.git")},
		{"too many arguments", []string{root, testExecutionID, "get", "extra"}, gitRequest(testHost, "ora/private-repo.git")},
		{"an operation Git does not define", []string{root, testExecutionID, "fill"}, gitRequest(testHost, "ora/private-repo.git")},
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
				strings.NewReader("host="+testHost+"\npath=ora/private-repo.git\nusername=x-access-token\npassword="+testToken+"\n\n"), &stdout)
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
	root := stage(t, testHost, testPath, testToken)
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
	if err := run([]string{root, testExecutionID, "get"},
		strings.NewReader(gitRequest(testHost, "ora/private-repo.git")), &stdout); err == nil {
		t.Fatal("material with a widened mode was delivered")
	}
	if stdout.Len() != 0 {
		t.Fatalf("a refused request still wrote %q to standard output", stdout.String())
	}
}
