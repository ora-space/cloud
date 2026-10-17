package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wanglongan587/cloud/internal/credentialmaterial"
)

const (
	testRepository = "https://git.example.com/ora/private-repo"
	testUsername   = "x-access-token"
	testPassword   = "ghp_generated-test-token"
)

func stage(t *testing.T, repository string) string {
	t.Helper()
	scope, err := credentialmaterial.NewScope(repository)
	if err != nil {
		t.Fatalf("NewScope: %v", err)
	}
	material, err := credentialmaterial.Stage(t.TempDir(), scope, testUsername, testPassword)
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	t.Cleanup(func() { _ = material.Destroy() })
	return material.Dir()
}

// request renders a request the way Git writes it for an https URL with credential.useHttpPath:
// the path has no leading slash and keeps the spelling of the URL.
func request(protocol, host, path string) string {
	return "protocol=" + protocol + "\nhost=" + host + "\npath=" + path + "\n\n"
}

func TestGetMatchesTheStagedScope(t *testing.T) {
	dir := stage(t, testRepository)
	want := "username=" + testUsername + "\npassword=" + testPassword + "\n"
	cases := []struct {
		name    string
		stdin   string
		deliver bool
	}{
		{"exact path", request("https", "git.example.com", "ora/private-repo"), true},
		{"git suffix", request("https", "git.example.com", "ora/private-repo.git"), true},
		{"trailing slash", request("https", "git.example.com", "ora/private-repo/"), true},
		{"host case", request("https", "Git.Example.COM", "ora/private-repo.git"), true},
		{"default port", request("https", "git.example.com:443", "ora/private-repo.git"), true},
		{"extra attributes", "capability[]=authtype\nprotocol=https\nhost=git.example.com\npath=ora/private-repo.git\nwwwauth[]=Basic realm=x\n\n", true},
		{"no trailing blank line", "protocol=https\nhost=git.example.com\npath=ora/private-repo\n", true},
		{"different host", request("https", "git.attacker.example", "ora/private-repo.git"), false},
		{"suffix-sharing host", request("https", "evil-git.example.com", "ora/private-repo.git"), false},
		{"other port", request("https", "git.example.com:8443", "ora/private-repo.git"), false},
		{"different path", request("https", "git.example.com", "ora/other-repo.git"), false},
		{"sub path", request("https", "git.example.com", "ora/private-repo/sub"), false},
		{"super path", request("https", "git.example.com", "ora"), false},
		{"path case", request("https", "git.example.com", "Ora/Private-Repo.git"), false},
		{"double git suffix", request("https", "git.example.com", "ora/private-repo.git.git"), false},
		{"traversal", request("https", "git.example.com", "ora/x/../private-repo"), false},
		{"encoded path", request("https", "git.example.com", "ora%2Fprivate-repo"), false},
		{"http protocol", request("http", "git.example.com", "ora/private-repo.git"), false},
		{"ssh protocol", request("ssh", "git.example.com", "ora/private-repo.git"), false},
		{"missing protocol", "host=git.example.com\npath=ora/private-repo.git\n\n", false},
		{"missing path", "protocol=https\nhost=git.example.com\n\n", false},
		{"missing host", "protocol=https\npath=ora/private-repo.git\n\n", false},
		{"repeated host", "protocol=https\nhost=git.attacker.example\nhost=git.example.com\npath=ora/private-repo\n\n", false},
		{"userinfo smuggled in host", request("https", "user@git.example.com", "ora/private-repo"), false},
		{"empty request", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout strings.Builder
			if err := run([]string{dir, "get"}, strings.NewReader(tc.stdin), &stdout); err != nil {
				t.Fatalf("run = %v; a matched or declined request is not an error", err)
			}
			switch {
			case tc.deliver && stdout.String() != want:
				t.Fatalf("output = %q, want %q", stdout.String(), want)
			case !tc.deliver && stdout.Len() != 0:
				t.Fatalf("a request outside the scope was answered with %q", stdout.String())
			}
		})
	}
}

func TestGetWithAPortScope(t *testing.T) {
	dir := stage(t, "https://git.example.com:8443/ora/private-repo")
	var stdout strings.Builder
	if err := run([]string{dir, "get"}, strings.NewReader(request("https", "git.example.com:8443", "ora/private-repo.git")), &stdout); err != nil || !strings.Contains(stdout.String(), "password="+testPassword) {
		t.Fatalf("a request for the staged port was not answered: %q, %v", stdout.String(), err)
	}
	stdout.Reset()
	if err := run([]string{dir, "get"}, strings.NewReader(request("https", "git.example.com", "ora/private-repo")), &stdout); err != nil || stdout.Len() != 0 {
		t.Fatalf("a request without the staged port was answered: %q, %v", stdout.String(), err)
	}
}

func TestStoreAndEraseAreSilentNoOps(t *testing.T) {
	for _, operation := range []string{"store", "erase"} {
		t.Run(operation, func(t *testing.T) {
			// The directory does not exist: succeeding proves nothing was read or written.
			dir := filepath.Join(t.TempDir(), "absent")
			var stdout strings.Builder
			stdin := request("https", "git.example.com", "ora/private-repo") + "username=u\npassword=" + testPassword + "\n\n"
			if err := run([]string{dir, operation}, strings.NewReader(stdin), &stdout); err != nil {
				t.Fatalf("%s = %v, want a no-op", operation, err)
			}
			if stdout.Len() != 0 {
				t.Fatalf("%s wrote %q", operation, stdout.String())
			}
			if _, err := os.Lstat(dir); !os.IsNotExist(err) {
				t.Fatalf("%s created something: %v", operation, err)
			}
		})
	}
}

func TestFailures(t *testing.T) {
	dir := stage(t, testRepository)
	tampered := stage(t, testRepository)
	if err := os.Chmod(filepath.Join(tampered, credentialmaterial.PasswordFile), 0o644); err != nil {
		t.Fatal(err)
	}
	good := request("https", "git.example.com", "ora/private-repo")
	cases := map[string][]string{
		"no arguments":       {},
		"missing operation":  {dir},
		"too many arguments": {dir, "get", "extra"},
		"unknown operation":  {dir, "fill"},
		"missing material":   {filepath.Join(t.TempDir(), "absent"), "get"},
		"relative material":  {"relative/dir", "get"},
		"tampered material":  {tampered, "get"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			var stdout strings.Builder
			err := run(args, strings.NewReader(good), &stdout)
			if err == nil {
				t.Fatal("run succeeded")
			}
			if stdout.Len() != 0 {
				t.Fatalf("a failed run wrote %q", stdout.String())
			}
			if strings.Contains(err.Error(), testPassword) {
				t.Fatalf("the error carries the secret: %v", err)
			}
		})
	}
}

// TestHelperWithGit runs the real `git credential fill` against the helper, so the request spelling
// the tests assume is the one Git actually writes.
func TestHelperWithGit(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}
	helper := filepath.Join(t.TempDir(), "git-credential-ora")
	build := exec.Command("go", "build", "-o", helper, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build helper: %v\n%s", err, out)
	}
	dir := stage(t, testRepository)
	fill := func(url string) string {
		command := exec.Command(git,
			"-c", "credential.helper=",
			"-c", "credential.helper="+helper+" "+dir,
			"-c", "credential.useHttpPath=true",
			"credential", "fill")
		command.Env = []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "HOME=" + t.TempDir(), "GIT_TERMINAL_PROMPT=0", "PATH=" + os.Getenv("PATH")}
		command.Stdin = strings.NewReader("url=" + url + "\n\n")
		out, _ := command.Output()
		return string(out)
	}
	if out := fill("https://git.example.com/ora/private-repo.git"); !strings.Contains(out, "password="+testPassword+"\n") {
		t.Fatalf("git credential fill for the staged repository = %q", out)
	}
	for _, url := range []string{"https://git.example.com/ora/other.git", "http://git.example.com/ora/private-repo.git", "https://other.example/ora/private-repo"} {
		if out := fill(url); strings.Contains(out, testPassword) {
			t.Fatalf("git credential fill for %s received the secret: %q", url, out)
		}
	}
}
