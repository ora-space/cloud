package cloneexec

import (
	"reflect"
	"strings"
	"testing"
)

// TestGitEgressIsOffUnlessADeploymentEnablesIt pins the default: the zero value and an empty
// configuration both produce a policy that contributes nothing, so a deployment that never mentions a
// proxy runs exactly the command line that clears every proxy.
func TestGitEgressIsOffUnlessADeploymentEnablesIt(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		proxyURL string
		hosts    []string
	}{
		{name: "the zero value"},
		{name: "both settings blank", proxyURL: "  ", hosts: []string{"", "  "}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			egress, err := NewGitEgress(testCase.proxyURL, testCase.hosts)
			if err != nil {
				t.Fatalf("an unset egress policy was refused: %v", err)
			}
			want := GitEgress{}
			if !reflect.DeepEqual(egress, want) {
				t.Fatalf("egress = %+v, want the disabled policy %+v", egress, want)
			}
			arguments, err := egress.arguments("https://github.com/octocat/Hello-World")
			if err != nil {
				t.Fatalf("a disabled policy refused an address: %v", err)
			}
			if len(arguments) != 0 {
				t.Fatalf("a disabled policy produced arguments: %v", arguments)
			}
		})
	}
}

// TestGitEgressRoutesOnlyTheHostsItNames is the direct evidence that the policy is host-scoped rather
// than global: a listed host is routed, and every other address — including the executor's own
// internal services — is left exactly as it was.
func TestGitEgressRoutesOnlyTheHostsItNames(t *testing.T) {
	egress, err := NewGitEgress("http://172.17.0.1:17890", []string{"github.com", "gist.github.com"})
	if err != nil {
		t.Fatalf("a valid egress policy was refused: %v", err)
	}
	cases := []struct {
		name    string
		address string
		want    []string
	}{
		{
			name:    "a listed host",
			address: "https://github.com/octocat/Hello-World",
			want:    []string{"-c", "http.https://github.com.proxy=http://172.17.0.1:17890"},
		},
		{
			name:    "another listed host",
			address: "https://gist.github.com/octocat/1234",
			want:    []string{"-c", "http.https://gist.github.com.proxy=http://172.17.0.1:17890"},
		},
		{
			name:    "a listed host spelled with the default port",
			address: "https://github.com:443/octocat/Hello-World",
			want:    []string{"-c", "http.https://github.com:443.proxy=http://172.17.0.1:17890"},
		},
		{
			name:    "a listed host on a non-default port",
			address: "https://github.com:8443/octocat/Hello-World",
			want:    []string{"-c", "http.https://github.com:8443.proxy=http://172.17.0.1:17890"},
		},
		{
			name:    "a host that is not listed",
			address: "https://gitlab.com/group/repo",
		},
		{
			name:    "a host that merely ends with a listed host",
			address: "https://notgithub.com/group/repo",
		},
		// The internal services C also speaks to. None of them is named by the policy, and this is what
		// makes an exclusion list unnecessary: they are not excluded, they simply never match.
		{name: "the internal Git Server", address: "https://git-server/ora/mirror.git"},
		{name: "the credential store", address: "https://credential-store/authorizations"},
		{name: "a bare host with no scheme", address: "github.com/octocat/Hello-World"},
		{name: "a non-https scheme", address: "ssh://git@github.com/octocat/Hello-World"},
		{name: "an address that is not a URL", address: "://"},
		{name: "the empty address", address: ""},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			arguments, err := egress.arguments(testCase.address)
			if err != nil {
				t.Fatalf("arguments(%q) failed: %v", testCase.address, err)
			}
			if !reflect.DeepEqual(arguments, testCase.want) {
				t.Fatalf("arguments(%q) = %v, want %v", testCase.address, arguments, testCase.want)
			}
			// A policy that does not apply must never emit a bare or empty key. An empty URL-specific
			// entry outranks the generic `http.proxy=`, so writing one would enable a proxy rather than
			// clear it — the exact inversion of "this policy does not apply here".
			for _, argument := range arguments {
				if strings.Contains(argument, ".proxy=") && strings.HasSuffix(argument, ".proxy=") {
					t.Fatalf("an empty proxy key was emitted for %q: %v", testCase.address, arguments)
				}
			}
		})
	}
}

// TestGitEgressRefusesAPolicyItCouldNotApplyAsDeclared covers the startup refusal: a policy is either
// applied exactly as declared or the deployment does not start. Half-declared policies are refused in
// both directions, because either reading of one is a deployment that does not mean what it says.
func TestGitEgressRefusesAPolicyItCouldNotApplyAsDeclared(t *testing.T) {
	cases := []struct {
		name     string
		proxyURL string
		hosts    []string
	}{
		{name: "a URL with no host to use it for", proxyURL: "http://172.17.0.1:17890"},
		{name: "a URL with only blank hosts", proxyURL: "http://172.17.0.1:17890", hosts: []string{"", " "}},
		{name: "hosts with no URL", hosts: []string{"github.com"}},
		{name: "a URL with no scheme", proxyURL: "172.17.0.1:17890", hosts: []string{"github.com"}},
		{name: "a URL with a scheme Git would not dial", proxyURL: "socks5://172.17.0.1:1080", hosts: []string{"github.com"}},
		{name: "a URL with no host", proxyURL: "http://", hosts: []string{"github.com"}},
		{name: "a URL carrying credentials", proxyURL: "http://user:secret@172.17.0.1:17890", hosts: []string{"github.com"}},
		{name: "a URL carrying a query", proxyURL: "http://172.17.0.1:17890/?x=1", hosts: []string{"github.com"}},
		{name: "a URL carrying a fragment", proxyURL: "http://172.17.0.1:17890/#x", hosts: []string{"github.com"}},
		{name: "a URL with whitespace inside it", proxyURL: "http://172.17.0.1:17890 /", hosts: []string{"github.com"}},
		{name: "a URL with a newline inside it", proxyURL: "http://172.17.0.1:17890\nevil", hosts: []string{"github.com"}},
		{name: "a URL with a carriage return inside it", proxyURL: "http://172.17.0.1:17890\revil", hosts: []string{"github.com"}},
		{name: "a host that is not lowercase", hosts: []string{"GitHub.com"}, proxyURL: "http://172.17.0.1:17890"},
		{name: "a host carrying a scheme", hosts: []string{"https://github.com"}, proxyURL: "http://172.17.0.1:17890"},
		{name: "a host carrying a port", hosts: []string{"github.com:443"}, proxyURL: "http://172.17.0.1:17890"},
		{name: "a host carrying a path", hosts: []string{"github.com/octocat"}, proxyURL: "http://172.17.0.1:17890"},
		{name: "a host with whitespace", hosts: []string{"git hub.com"}, proxyURL: "http://172.17.0.1:17890"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			egress, err := NewGitEgress(testCase.proxyURL, testCase.hosts)
			if err == nil {
				t.Fatalf("a policy that cannot be applied as declared was accepted: %+v", egress)
			}
			if !reflect.DeepEqual(egress, GitEgress{}) {
				t.Fatalf("a refused policy produced %+v", egress)
			}
		})
	}
}

// TestNewGitEgressTrimsTheHostListItIsGiven pins that a list assembled from a comma-separated setting
// tolerates the blanks a trailing comma or a stray space produces, so a cosmetic typo is not a startup
// failure while a policy that names no host at all still is.
//
// The same trimming is what makes the proxy URL safe to supply from the environment: a value written
// from a file or a shell export routinely carries a trailing newline, and trimming it at this boundary is
// what keeps that newline out of the command line. A control character *inside* the value is a different
// matter and is refused, because it could split the `-c` entry rather than travel inside it.
func TestNewGitEgressTrimsTheHostListItIsGiven(t *testing.T) {
	egress, err := NewGitEgress(" http://172.17.0.1:17890\n", []string{"", " github.com ", "", "gist.github.com", " "})
	if err != nil {
		t.Fatalf("a valid policy with a padded list was refused: %v", err)
	}
	want := GitEgress{ProxyURL: "http://172.17.0.1:17890", Hosts: []string{"github.com", "gist.github.com"}}
	if !reflect.DeepEqual(egress, want) {
		t.Fatalf("egress = %+v, want %+v", egress, want)
	}
}

// TestTheSharedConstraintsCarryTheEgressPolicyForAListedHost is the evidence that the policy reaches
// the one shared argument list rather than a copy per path: the entry appears beside the `http.proxy=`
// clearing entry in the clone command line, and a deployment without a policy produces no such entry.
func TestTheSharedConstraintsCarryTheEgressPolicyForAListedHost(t *testing.T) {
	const (
		helper = "/usr/local/bin/git-credential-ora"
		root   = "/run/ora/credentials"
		target = "/var/lib/ora/clones/" + testExecutionID
	)
	egress, err := NewGitEgress("http://172.17.0.1:17890", []string{"git.example.com"})
	if err != nil {
		t.Fatalf("a valid egress policy was refused: %v", err)
	}
	job := Job{ExecutionID: testExecutionID, Repository: testRepository}

	arguments, err := cloneArguments(&job, helper, root, target, gitTransport{egress: egress})
	if err != nil {
		t.Fatalf("cloneArguments refused a valid job: %v", err)
	}
	want := []string{"-c", "http.https://git.example.com.proxy=http://172.17.0.1:17890"}
	if got := egressEntries(arguments); !reflect.DeepEqual(got, want) {
		t.Fatalf("egress entries = %v, want %v", got, want)
	}
	// The generic clearing entry stays: the specific key outranks it for the host it names, and every
	// other host is still reached with no proxy at all.
	if index := indexOf(arguments, "http.proxy="); index < 0 {
		t.Fatalf("the command line no longer clears the generic proxy: %v", arguments)
	}

	// A policy that does not name this host adds nothing, which is what keeps an internal clone on the
	// same code path unaffected by a proxy the deployment enabled for a public forge.
	other, err := NewGitEgress("http://172.17.0.1:17890", []string{"github.com"})
	if err != nil {
		t.Fatalf("a valid egress policy was refused: %v", err)
	}
	unrouted, err := cloneArguments(&job, helper, root, target, gitTransport{egress: other})
	if err != nil {
		t.Fatalf("cloneArguments refused a valid job: %v", err)
	}
	if got := egressEntries(unrouted); len(got) != 0 {
		t.Fatalf("an unlisted host was routed: %v", got)
	}
}

// egressEntries returns the host-scoped proxy entries of a command line, so a test can assert on them
// without depending on where in the list they are placed.
func egressEntries(arguments []string) []string {
	var entries []string
	for index, argument := range arguments {
		if !strings.HasPrefix(argument, "http.https://") || !strings.Contains(argument, ".proxy=") {
			continue
		}
		entries = append(entries, arguments[index-1], argument)
	}
	return entries
}

// indexOf returns the position of a value in a command line, or -1.
func indexOf(values []string, want string) int {
	for index, value := range values {
		if value == want {
			return index
		}
	}
	return -1
}
