package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wanglongan587/cloud/internal/mirror"
)

// gitServerMirrorKey is a well-formed mirror key, so the address a case builds is refused for the
// reason under test rather than for a malformed key.
var gitServerMirrorKey = mirror.Key{
	ProjectID:  "prj_01",
	Repository: "https://git.example.invalid/group/private",
	Commit:     "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
}

// TestTheGitServerSectionNamesAtMostOneContentChannel is the deployment matrix for the internal Git
// Server's address. Each case is a deployment an operator could plausibly write, and the two refusals
// are the two that would otherwise start and then quietly not do what the file says: a credentialed
// clone executor that is never dispatched to, and a mirror nobody publishes.
//
// The cases that name no Git Server are asserted as carefully as the ones that do, because the
// approved default and the pre-mirror channel both live there: an empty section must not become a
// startup error merely because some executor capability is switched on.
func TestTheGitServerSectionNamesAtMostOneContentChannel(t *testing.T) {
	cases := []struct {
		name string
		base string
		// clone and mirror are the two capabilities the opened `clone_dispatch` section yields.
		clone, mirror bool
		// wantAdmitted is true when the deployment must start, and the remaining fields describe the
		// refusal when it must not.
		wantAdmitted bool
		wantMentions []string
	}{
		{
			name:         "an unconfigured deployment serves no mirror",
			wantAdmitted: true,
		},
		{
			name:         "the credentialed-clone channel is untouched by an empty section",
			clone:        true,
			wantAdmitted: true,
		},
		{
			name:         "mirror preparation without a Git Server is a coherent deployment",
			mirror:       true,
			wantAdmitted: true,
		},
		{
			name:         "a named Git Server with a publisher is admitted",
			base:         "https://git-server.example.invalid",
			mirror:       true,
			wantAdmitted: true,
		},
		{
			// The value is trimmed before it is validated, so a deployment file that indented the value
			// does not turn into a startup failure about a URL that is in fact well formed.
			name:         "surrounding whitespace is not part of the address",
			base:         "  https://git-server.example.invalid\n",
			mirror:       true,
			wantAdmitted: true,
		},
		{
			name:         "a named Git Server alongside credentialed clone dispatch is refused",
			base:         "https://git-server.example.invalid",
			clone:        true,
			mirror:       true,
			wantMentions: []string{"git_server.sandbox_base_url", "clone_dispatch.enabled"},
		},
		{
			name:         "a named Git Server with no publisher is refused",
			base:         "https://git-server.example.invalid",
			wantMentions: []string{"git_server.sandbox_base_url", "clone_dispatch.mirror_enabled"},
		},
		{
			name:         "a plaintext address is refused",
			base:         "http://git-server.example.invalid",
			mirror:       true,
			wantMentions: []string{"git_server.sandbox_base_url", "https"},
		},
		{
			name:         "an address that smuggles a credential is refused",
			base:         "https://user:token@git-server.example.invalid",
			mirror:       true,
			wantMentions: []string{"git_server.sandbox_base_url", "user info"},
		},
		{
			name:         "an address with a path is refused",
			base:         "https://git-server.example.invalid/mirror/v1",
			mirror:       true,
			wantMentions: []string{"git_server.sandbox_base_url", "path"},
		},
		{
			name:         "an address with a query is refused",
			base:         "https://git-server.example.invalid?token=1",
			mirror:       true,
			wantMentions: []string{"git_server.sandbox_base_url", "query"},
		},
		{
			name:         "an address that names no host is refused",
			base:         "https://",
			mirror:       true,
			wantMentions: []string{"git_server.sandbox_base_url"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dispatch := &ExecutorDispatch{}
			if c.clone {
				dispatch.Clone = &CloneCapability{}
			}
			if c.mirror {
				dispatch.Mirror = &MirrorCapability{}
			}
			cfg := &Config{GitServer: GitServerConfig{SandboxBaseURL: c.base}}

			endpoint, err := cfg.MirrorEndpoint(dispatch)
			if c.wantAdmitted {
				if err != nil {
					t.Fatalf("the deployment must start, got %v", err)
				}
				// An endpoint exists exactly when the deployment named one. A section left empty serves no
				// mirror even where mirror preparation is on: preparation publishes into a volume, and Cloud
				// has not been told where a sandbox reads it from.
				if named := strings.TrimSpace(c.base) != ""; (endpoint != nil) != named {
					t.Fatalf("endpoint is %v, want it non-nil exactly when a Git Server is named", endpoint)
				}
				if endpoint == nil {
					return
				}
				address, err := endpoint.CloneAddress(gitServerMirrorKey)
				if err != nil {
					t.Fatalf("the endpoint must address a mirror: %v", err)
				}
				// The address the deployment named has to survive into the URL Cloud records, and the path
				// has to be the server's own read route rather than a second spelling of it.
				if want := "https://git-server.example.invalid/mirror/v1/"; !strings.HasPrefix(address, want) || !strings.HasSuffix(address, ".git") {
					t.Fatalf("the configured base must appear in the mirror's address under the server's route, got %q", address)
				}
				return
			}
			if err == nil {
				t.Fatal("the deployment must be refused at startup")
			}
			if endpoint != nil {
				t.Fatal("a refused deployment still yielded an endpoint")
			}
			for _, want := range c.wantMentions {
				// The refusal has to name the keys the operator must change: a startup error that only
				// says the URL is bad sends them to the wrong section.
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("the refusal must mention %q, got %q", want, err)
				}
			}
		})
	}
}

// TestTheGitServerAddressIsDeployableFromTheEnvironment pins the part of this section a compose file
// relies on: `BindEnvKeys` resolves the key from `CLOUD_GIT_SERVER_SANDBOX_BASE_URL` even when the
// configuration file does not mention the section at all. A section registered on `Config` but missed
// by the binder would leave the deployment's value silently ignored, and the symptom would be a
// deployment that publishes mirrors no Workspace may clone from.
func TestTheGitServerAddressIsDeployableFromTheEnvironment(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yaml := "server: {port: 8080, mode: test, read_timeout: 5s, write_timeout: 5s}\n" +
		"logger: {level: info, filename: " + filepath.ToSlash(filepath.Join(dir, "app.log")) + ", max_size: 1, max_backups: 1, max_age: 1, compress: false, enable_console: false}\n" +
		"database: {driver: postgres, dsn: \"host=db\", max_idle_conns: 1, max_open_conns: 1, conn_max_lifetime: 1h}\n" +
		"control: {grpc_addr: 127.0.0.1:8082}\n"
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLOUD_GIT_SERVER_SANDBOX_BASE_URL", "https://git-server.example.invalid")

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GitServer.SandboxBaseURL != "https://git-server.example.invalid" {
		t.Fatalf("the environment must supply the Git Server's address, got %q", cfg.GitServer.SandboxBaseURL)
	}
}
