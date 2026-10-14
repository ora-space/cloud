package config

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/internal/cloneauth"
	"github.com/wanglongan587/cloud/internal/cloneexec"
	"github.com/wanglongan587/cloud/internal/core"
)

// cloneExecutorDeployment builds a deployment that is complete except where a case changes it. Every
// path it names is a temporary file, so the key material never reaches a durable fixture.
func cloneExecutorDeployment(t *testing.T, dir string) CloneExecutorServiceConfig {
	t.Helper()
	certificate, key, ca := writePKI(t, dir, "clone-executor.test", "spiffe://ora/cloud/clone-executor")
	storeCertificate, storeKey, storeCA := writePKI(t, dir, "broker-resolver.test", "spiffe://ora/node/credential-executor")
	// The three roots are created by the deployment, not by the executor: a root the executor could
	// create for itself is a root it could widen, and on a container whose volume was not mounted it
	// would silently write the token to an ephemeral layer instead of refusing. `cluster/compose/
	// clone-executor-setup.sh` is what does this in a real stack. The home is one of them because Git
	// reads it as `HOME`, and `cloneexec.New` refuses a deployment whose home is absent or not 0700.
	for _, root := range []string{"material", "checkout", "home"} {
		if err := os.MkdirAll(filepath.Join(dir, root), 0o700); err != nil {
			t.Fatalf("prepare the %s root: %v", root, err)
		}
	}
	return CloneExecutorServiceConfig{
		Listen:                     ":8444",
		MaterialRoot:               filepath.Join(dir, "material"),
		CheckoutRoot:               filepath.Join(dir, "checkout"),
		HomeDirectory:              filepath.Join(dir, "home"),
		HelperPath:                 filepath.Join(dir, "git-credential-ora"),
		GitPath:                    "/usr/bin/git",
		AuthorizationPublicKeyFile: writeAuthorizationKey(t, dir),
		CertificateFile:            certificate,
		PrivateKeyFile:             key,
		ClientCAFile:               ca,
		DispatcherIdentity:         "spiffe://ora/cloud/clone-dispatcher",
		StoreEndpoint:              "https://credential-store:8443",
		StoreCertificateFile:       storeCertificate,
		StorePrivateKeyFile:        storeKey,
		StoreCAFile:                storeCA,
		Timeout:                    5 * time.Minute,
	}
}

// startCloneExecutor mirrors the sequence `cmd/clone-executor` runs before it binds the listener, so a
// case asserts the deployment is refused at startup rather than that one accessor happens to fail.
func startCloneExecutor(t *testing.T, cfg CloneExecutorServiceConfig) error {
	t.Helper()
	executor, err := cfg.Executor()
	if err != nil {
		return err
	}
	if _, err := executor.Sweep(); err != nil {
		return err
	}
	tlsConfig, err := cfg.ServerTLS()
	if err != nil {
		return err
	}
	_, err = cloneexec.NewServer(executor, cloneexec.ServerConfig{TLS: tlsConfig, Caller: cfg.DispatcherIdentity})
	return err
}

// TestCloneExecutorRefusesAnIncompleteDeployment pins the startup gate the isolation decision's D17
// configuration table requires: a deployment that declares credentialed clone establishes the whole
// boundary or does not start. A partially configured executor would accept jobs and fail them one at
// a time, which reads as a flaky clone rather than as a misconfigured deployment.
func TestCloneExecutorRefusesAnIncompleteDeployment(t *testing.T) {
	dir := t.TempDir()
	complete := cloneExecutorDeployment(t, dir)
	if err := startCloneExecutor(t, complete); err != nil {
		t.Fatalf("a complete deployment did not start: %v", err)
	}

	cases := []struct {
		name   string
		mutate func(*CloneExecutorServiceConfig)
		reason string
	}{
		{"no listen address", func(c *CloneExecutorServiceConfig) { c.Listen = "  " }, "the executor would bind nothing"},
		{"no materialization root", func(c *CloneExecutorServiceConfig) { c.MaterialRoot = "" }, "there would be nowhere to stage a token"},
		{"a relative materialization root", func(c *CloneExecutorServiceConfig) { c.MaterialRoot = "material" }, "the material's location would depend on the working directory"},
		{"a materialization root that is the checkout root", func(c *CloneExecutorServiceConfig) { c.CheckoutRoot = c.MaterialRoot }, "one directory cannot be both untraversable by the workload and its handoff point"},
		{"no checkout root", func(c *CloneExecutorServiceConfig) { c.CheckoutRoot = "" }, "there would be nowhere to clone into"},
		{"no home directory", func(c *CloneExecutorServiceConfig) { c.HomeDirectory = "" }, "Git would fall back to a real home directory"},
		{"no credential helper", func(c *CloneExecutorServiceConfig) { c.HelperPath = "" }, "Git would find no credential at all, or an inherited one"},
		{"a helper path with a shell metacharacter", func(c *CloneExecutorServiceConfig) { c.HelperPath = "/app/git-credential-ora;sh" }, "the path would change what Git executes"},
		{"no git program", func(c *CloneExecutorServiceConfig) { c.GitPath = "" }, "the executor would run whatever `git` resolves to"},
		{"no verification key", func(c *CloneExecutorServiceConfig) { c.AuthorizationPublicKeyFile = "" }, "no authorization could be verified, so every job would be unverifiable"},
		{"an unreadable verification key", func(c *CloneExecutorServiceConfig) { c.AuthorizationPublicKeyFile = filepath.Join(dir, "absent.pem") }, "an absent key must not read as an unconfigured one"},
		{"no server certificate", func(c *CloneExecutorServiceConfig) { c.CertificateFile = "" }, "the executor could not prove who it is"},
		{"no client authority", func(c *CloneExecutorServiceConfig) { c.ClientCAFile = "" }, "an anonymous caller would reach the identity comparison"},
		{"no dispatcher identity", func(c *CloneExecutorServiceConfig) { c.DispatcherIdentity = "" }, "any verified certificate would be able to spend an authorization"},
		{"a dispatcher identity that is not a spiffe URI", func(c *CloneExecutorServiceConfig) { c.DispatcherIdentity = "clone-dispatcher" }, "the identity would not match the platform's spelling"},
		{"no credential service endpoint", func(c *CloneExecutorServiceConfig) { c.StoreEndpoint = "" }, "the executor could not redeem an authorization"},
		{"no resolver certificate", func(c *CloneExecutorServiceConfig) { c.StoreCertificateFile = "" }, "the executor could not prove itself to the Service"},
		{"no resolver authority", func(c *CloneExecutorServiceConfig) { c.StoreCAFile = "" }, "the Service's certificate could not be verified"},
		{"a proxy URL with no host to use it for", func(c *CloneExecutorServiceConfig) { c.GitProxyURL = "http://172.17.0.1:17890" }, "the policy would route every host, not the ones the deployment named"},
		{"proxy hosts with no proxy URL", func(c *CloneExecutorServiceConfig) { c.GitProxyHosts = "github.com" }, "the policy would silently route nothing while reading as if it did"},
		{"a proxy URL carrying credentials", func(c *CloneExecutorServiceConfig) {
			c.GitProxyURL = "http://user:secret@172.17.0.1:17890"
			c.GitProxyHosts = "github.com"
		}, "a credential would reach the command line"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			cfg := cloneExecutorDeployment(t, t.TempDir())
			test.mutate(&cfg)
			if err := startCloneExecutor(t, cfg); err == nil {
				t.Fatalf("an incomplete deployment started: %s", test.reason)
			}
		})
	}
}

// TestCloneExecutorBindsOnlyItsOwnEnvironment is the deployment-integration contract: it sets exactly
// the variables `cluster/compose.yaml` sets on the executor service and asserts the loader reads each
// one, and that no Cloud or Credential Service variable reaches the executor.
//
// It is written against the compose file's spellings on purpose. A renamed field would otherwise fail
// only in a running stack, where it looks like a clone that cannot be dispatched rather than like a
// configuration typo.
func TestCloneExecutorBindsOnlyItsOwnEnvironment(t *testing.T) {
	dir := t.TempDir()
	certificate, key, ca := writePKI(t, dir, "clone-executor.test", "spiffe://ora/cloud/clone-executor")
	storeCertificate, storeKey, storeCA := writePKI(t, dir, "broker-resolver.test", "spiffe://ora/node/credential-executor")

	for name, value := range map[string]string{
		"CLONE_EXECUTOR_LISTEN":                        "0.0.0.0:8444",
		"CLONE_EXECUTOR_MATERIAL_ROOT":                 "/var/lib/clone-executor/material",
		"CLONE_EXECUTOR_CHECKOUT_ROOT":                 "/var/lib/clone-executor/checkout",
		"CLONE_EXECUTOR_HOME_DIRECTORY":                "/var/lib/clone-executor/home",
		"CLONE_EXECUTOR_HELPER_PATH":                   "/app/git-credential-ora",
		"CLONE_EXECUTOR_GIT_PATH":                      "/usr/bin/git",
		"CLONE_EXECUTOR_AUTHORIZATION_PUBLIC_KEY_FILE": writeAuthorizationKey(t, dir),
		"CLONE_EXECUTOR_CERTIFICATE_FILE":              certificate,
		"CLONE_EXECUTOR_PRIVATE_KEY_FILE":              key,
		"CLONE_EXECUTOR_CLIENT_CA_FILE":                ca,
		"CLONE_EXECUTOR_DISPATCHER_IDENTITY":           "spiffe://ora.local/cloud/clone-dispatcher",
		"CLONE_EXECUTOR_STORE_ENDPOINT":                "https://credential-store:8443",
		"CLONE_EXECUTOR_STORE_CERTIFICATE_FILE":        storeCertificate,
		"CLONE_EXECUTOR_STORE_PRIVATE_KEY_FILE":        storeKey,
		"CLONE_EXECUTOR_STORE_CA_FILE":                 storeCA,
		"CLONE_EXECUTOR_TIMEOUT":                       "5m",
		"CLONE_EXECUTOR_MIRROR_ROOT":                   "/var/lib/clone-executor/mirrors",
		"CLONE_EXECUTOR_MIRROR_MAX_BYTES":              "4294967296",
		"CLONE_EXECUTOR_MIRROR_VOLUME_BYTES":           "68719476736",
		"CLONE_EXECUTOR_MIRROR_GRACE":                  "24h",
		"CLONE_EXECUTOR_MIRROR_TIMEOUT":                "10m",
		"CLONE_EXECUTOR_PUSH_ROOT":                     "/var/lib/clone-executor/pushes",
		"CLONE_EXECUTOR_PUSH_TIMEOUT":                  "5m",
		"CLONE_EXECUTOR_GIT_PROXY_URL":                 "http://172.17.0.1:17890",
		"CLONE_EXECUTOR_GIT_PROXY_HOSTS":               "github.com,gist.github.com",
	} {
		t.Setenv(name, value)
	}
	// A Cloud-prefixed spelling of the same field must not reach the executor, and neither must the
	// Credential Service's. Cloud cannot point the executor at its own storage, and the Service cannot
	// give it a volume.
	t.Setenv("CLOUD_CLONE_DISPATCH_ENDPOINT", "https://elsewhere:8444")
	t.Setenv("CLOUD_LISTEN", ":9999")
	t.Setenv("CREDENTIAL_STORE_VOLUME", "/var/lib/credential-store")

	// An empty path lets viper search its documented locations, none of which holds an executor file in
	// this repository; every asserted value therefore comes from the environment.
	cfg, err := LoadCloneExecutorConfig("")
	if err != nil {
		t.Fatalf("LoadCloneExecutorConfig: %v", err)
	}
	if cfg.Listen != "0.0.0.0:8444" {
		t.Fatalf("listen = %q, want the value from CLONE_EXECUTOR_LISTEN", cfg.Listen)
	}
	if cfg.MaterialRoot != "/var/lib/clone-executor/material" || cfg.CheckoutRoot != "/var/lib/clone-executor/checkout" {
		t.Fatalf("roots = (%q, %q), want the mounted paths the executor was given", cfg.MaterialRoot, cfg.CheckoutRoot)
	}
	if cfg.HelperPath != "/app/git-credential-ora" || cfg.GitPath != "/usr/bin/git" {
		t.Fatalf("programs = (%q, %q), want the deployment-fixed paths", cfg.HelperPath, cfg.GitPath)
	}
	if cfg.DispatcherIdentity != "spiffe://ora.local/cloud/clone-dispatcher" {
		t.Fatalf("dispatcher identity = %q, want the one identity allowed to dispatch", cfg.DispatcherIdentity)
	}
	if cfg.StoreEndpoint != "https://credential-store:8443" {
		t.Fatalf("store endpoint = %q, want the Service's internal name", cfg.StoreEndpoint)
	}
	if cfg.Timeout != 5*time.Minute {
		t.Fatalf("timeout = %v, want 5m", cfg.Timeout)
	}
	// The mirror section, in the spellings the compose file uses. A renamed field would otherwise fail
	// only in a running stack, where a deployment that declared no mirror at all looks like an executor
	// that cannot publish rather than like a configuration typo.
	if cfg.MirrorRoot != "/var/lib/clone-executor/mirrors" {
		t.Fatalf("mirror root = %q, want the mounted mirror volume", cfg.MirrorRoot)
	}
	if cfg.MirrorMaxBytes != 4<<30 || cfg.MirrorVolumeBytes != 64<<30 {
		t.Fatalf("mirror limits = (%d, %d), want (4GiB, 64GiB)", cfg.MirrorMaxBytes, cfg.MirrorVolumeBytes)
	}
	if cfg.MirrorGrace != 24*time.Hour {
		t.Fatalf("mirror grace = %v, want 24h", cfg.MirrorGrace)
	}
	if cfg.MirrorTimeout != 10*time.Minute {
		t.Fatalf("mirror timeout = %v, want 10m", cfg.MirrorTimeout)
	}
	// The push section, which is the one whose absence is the approved default and whose misspelling would
	// therefore surface as a deployment that silently performs no controlled push at all.
	if cfg.PushRoot != "/var/lib/clone-executor/pushes" {
		t.Fatalf("push root = %q, want the mounted push volume", cfg.PushRoot)
	}
	if cfg.PushTimeout != 5*time.Minute {
		t.Fatalf("push timeout = %v, want 5m", cfg.PushTimeout)
	}
	// The egress policy, whose empty default is what keeps a deployment that never mentions a proxy on
	// exactly the command line that clears every proxy. It is asserted here because a misspelling would
	// otherwise show up only as a clone that still cannot reach its upstream — which is the symptom this
	// option exists to fix, and so the one that would be read as the option not working.
	if cfg.GitProxyURL != "http://172.17.0.1:17890" || cfg.GitProxyHosts != "github.com,gist.github.com" {
		t.Fatalf("egress = (%q, %q), want the values from CLONE_EXECUTOR_GIT_PROXY_URL and CLONE_EXECUTOR_GIT_PROXY_HOSTS", cfg.GitProxyURL, cfg.GitProxyHosts)
	}
	// The loaded pair is the one the arguments are built from, and a host on the list is the only thing
	// that routes: the host list is split and validated rather than passed through as one string.
	egress, err := cfg.gitEgress()
	if err != nil {
		t.Fatalf("the deployment's egress policy was refused: %v", err)
	}
	if egress.ProxyURL != "http://172.17.0.1:17890" || len(egress.Hosts) != 2 || egress.Hosts[0] != "github.com" || egress.Hosts[1] != "gist.github.com" {
		t.Fatalf("egress = %+v, want the two named hosts split from the comma-separated setting", egress)
	}
	if _, err := os.Stat(cfg.AuthorizationPublicKeyFile); err != nil {
		t.Fatalf("the verification key the environment named is not the file that was written: %v", err)
	}
	// The whole point of the separate loader: the Cloud variable above must not have become the
	// executor's endpoint.
	if cfg.StoreEndpoint == "https://elsewhere:8444" {
		t.Fatal("a Cloud-prefixed variable reached the executor's configuration")
	}
}

// mirrorDeployment completes a clone-executor deployment with a declared mirror section.
func mirrorDeployment(t *testing.T, dir string) CloneExecutorServiceConfig {
	t.Helper()
	cfg := cloneExecutorDeployment(t, dir)
	cfg.MirrorRoot = filepath.Join(dir, "mirrors")
	cfg.MirrorMaxBytes = 4 << 30
	cfg.MirrorVolumeBytes = 64 << 30
	cfg.MirrorGrace = 24 * time.Hour
	cfg.MirrorTimeout = 10 * time.Minute
	return cfg
}

// startMirrorExecutor mirrors the sequence `cmd/clone-executor` runs for the mirror capability, so a
// case asserts the deployment is refused at startup rather than that one accessor happens to fail.
func startMirrorExecutor(t *testing.T, cfg CloneExecutorServiceConfig) error {
	t.Helper()
	executor, err := cfg.MirrorExecutor()
	if err != nil {
		return err
	}
	if executor == nil {
		return nil
	}
	_, err = executor.Sweep()
	return err
}

// pushDeployment completes a clone-executor deployment with a declared push section.
func pushDeployment(t *testing.T, dir string) CloneExecutorServiceConfig {
	t.Helper()
	cfg := cloneExecutorDeployment(t, dir)
	cfg.PushRoot = filepath.Join(dir, "pushes")
	cfg.PushTimeout = 5 * time.Minute
	return cfg
}

// startPushExecutor mirrors the sequence `cmd/clone-executor` runs for the push capability, including the
// startup sweep, so a case asserts the deployment is refused at startup rather than that one accessor
// happens to fail.
func startPushExecutor(t *testing.T, cfg CloneExecutorServiceConfig) error {
	t.Helper()
	executor, err := cfg.PushExecutor()
	if err != nil {
		return err
	}
	if executor == nil {
		return nil
	}
	_, err = executor.Sweep()
	return err
}

// TestCloneExecutorPushIsAbsentUnlessTheDeploymentDeclaresIt pins the approved default for the one
// capability that changes something outside the platform.
//
// A deployment with no push root performs no controlled push at all, and that is not an error: a
// deployment has to be able to run C for clones and preparations while refusing every write, which is
// what an incremental rollout of the write boundary needs. What the route answers in that state is the
// executor's business, and `internal/cloneexec` proves it answers `credential_executor_unavailable`.
func TestCloneExecutorPushIsAbsentUnlessTheDeploymentDeclaresIt(t *testing.T) {
	dir := t.TempDir()
	cfg := cloneExecutorDeployment(t, dir)
	executor, err := cfg.PushExecutor()
	if err != nil {
		t.Fatalf("a deployment that declared no push was refused: %v", err)
	}
	if executor != nil {
		t.Fatal("a deployment that declared no push produced a push executor")
	}
	if _, err := os.Stat(filepath.Join(dir, "pushes")); !os.IsNotExist(err) {
		t.Fatalf("an undeclared push root was created anyway: %v", err)
	}
}

// TestCloneExecutorPushRefusesAnIncompleteSection is the D17 rule applied to the third capability: a
// deployment that declares a push establishes the whole of it or does not start. Every case removes
// something a push cannot work without, and a deployment that started anyway would accept an approval and
// then fail it, which reads as a flaky write rather than as a misconfigured deployment.
func TestCloneExecutorPushRefusesAnIncompleteSection(t *testing.T) {
	if err := startPushExecutor(t, pushDeployment(t, t.TempDir())); err != nil {
		t.Fatalf("a complete push deployment did not start: %v", err)
	}
	cases := []struct {
		name   string
		mutate func(*CloneExecutorServiceConfig)
		reason string
	}{
		{"no verification key", func(c *CloneExecutorServiceConfig) { c.AuthorizationPublicKeyFile = "" }, "no push authorization could be verified, so every write would be unverifiable"},
		{"no credential service endpoint", func(c *CloneExecutorServiceConfig) { c.StoreEndpoint = "" }, "the executor could not redeem a push authorization"},
		{"no credential service authority", func(c *CloneExecutorServiceConfig) { c.StoreCAFile = "" }, "the Service's certificate could not be verified"},
		{"no materialization root", func(c *CloneExecutorServiceConfig) { c.MaterialRoot = "" }, "there would be nowhere to stage the token the fetch authenticates with"},
		{"no credential helper", func(c *CloneExecutorServiceConfig) { c.HelperPath = "" }, "Git would find no credential at all, or an inherited one"},
		{"no git program", func(c *CloneExecutorServiceConfig) { c.GitPath = "" }, "the executor would write with whatever `git` resolves to"},
		{"no home directory", func(c *CloneExecutorServiceConfig) { c.HomeDirectory = "" }, "Git would fall back to a real home directory"},
		{"a relative push root", func(c *CloneExecutorServiceConfig) { c.PushRoot = "pushes" }, "the scratch repository's location would depend on the working directory"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			cfg := pushDeployment(t, t.TempDir())
			test.mutate(&cfg)
			if err := startPushExecutor(t, cfg); err == nil {
				t.Fatalf("an incomplete push deployment started: %s", test.reason)
			}
		})
	}
}

// TestCloneExecutorPushRefusesARootItWouldSweepAway is the containment rule, and for a push it guards
// more than its siblings do.
//
// The push executor's own sweep removes every entry beneath its root, and what lands there is a
// plaintext copy of a private upstream fetched with a delivered credential. Nesting it inside the mirror
// store would delete published content on the first restart — mirrors the internal Git Server is
// serving — and nesting it inside the checkout root would have a Workspace's clone deleted out from
// under it. The reverse nestings put a private repository's copy inside the directory staged tokens live
// in, or inside the volume served to workloads.
func TestCloneExecutorPushRefusesARootItWouldSweepAway(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*CloneExecutorServiceConfig)
	}{
		{"the push root is inside the materialization root", func(c *CloneExecutorServiceConfig) {
			c.PushRoot = filepath.Join(c.MaterialRoot, "pushes")
		}},
		{"the push root contains the materialization root", func(c *CloneExecutorServiceConfig) {
			c.PushRoot = filepath.Dir(c.MaterialRoot)
		}},
		{"the push root is the materialization root", func(c *CloneExecutorServiceConfig) {
			c.PushRoot = c.MaterialRoot
		}},
		{"the push root is inside the checkout root", func(c *CloneExecutorServiceConfig) {
			c.PushRoot = filepath.Join(c.CheckoutRoot, "pushes")
		}},
		{"the push root contains the checkout root", func(c *CloneExecutorServiceConfig) {
			c.PushRoot = filepath.Dir(c.CheckoutRoot)
		}},
		{"the push root is the checkout root", func(c *CloneExecutorServiceConfig) {
			c.PushRoot = c.CheckoutRoot
		}},
		{"the push root is inside the mirror root", func(c *CloneExecutorServiceConfig) {
			c.MirrorRoot = filepath.Join(filepath.Dir(c.MaterialRoot), "mirrors")
			c.PushRoot = filepath.Join(c.MirrorRoot, "pushes")
		}},
		{"the push root contains the mirror root", func(c *CloneExecutorServiceConfig) {
			c.PushRoot = filepath.Join(filepath.Dir(c.MaterialRoot), "pushes")
			c.MirrorRoot = filepath.Join(c.PushRoot, "mirrors")
		}},
		{"the push root is the mirror root", func(c *CloneExecutorServiceConfig) {
			c.PushRoot = filepath.Join(filepath.Dir(c.MaterialRoot), "mirrors")
			c.MirrorRoot = c.PushRoot
		}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			cfg := pushDeployment(t, t.TempDir())
			test.mutate(&cfg)
			if err := startPushExecutor(t, cfg); err == nil {
				t.Fatal("a deployment whose roots contain one another started")
			}
		})
	}
	// The control: sibling directories under the same parent are a legitimate deployment, so the rule
	// above is containment and not merely "shares an ancestor".
	cfg := pushDeployment(t, t.TempDir())
	cfg.MirrorRoot = filepath.Join(filepath.Dir(cfg.MaterialRoot), "mirrors")
	if err := startPushExecutor(t, cfg); err != nil {
		t.Fatalf("a deployment with sibling roots was refused: %v", err)
	}
}

// TestCloneExecutorMirrorIsAbsentUnlessTheDeploymentDeclaresIt pins the approved default.
//
// A deployment with no mirror root serves no mirror capability, and that is not an error: filling the
// internal Git Server's mirrors is separable from cloning into a Workspace, and a deployment that runs
// only one of them must be able to say so by saying nothing. What the route answers in that state is
// the executor's business, and `internal/cloneexec` proves it answers `credential_executor_unavailable`.
func TestCloneExecutorMirrorIsAbsentUnlessTheDeploymentDeclaresIt(t *testing.T) {
	dir := t.TempDir()
	cfg := cloneExecutorDeployment(t, dir)
	executor, err := cfg.MirrorExecutor()
	if err != nil {
		t.Fatalf("a deployment that declared no mirror was refused: %v", err)
	}
	if executor != nil {
		t.Fatal("a deployment that declared no mirror produced a mirror executor")
	}
	if _, err := os.Stat(filepath.Join(dir, "mirrors")); !os.IsNotExist(err) {
		t.Fatalf("an undeclared mirror root was created anyway: %v", err)
	}
}

// TestCloneExecutorMirrorRefusesAnIncompleteSection is the D17 rule applied to the second capability:
// a deployment that declares a mirror establishes the whole of it or does not start.
func TestCloneExecutorMirrorRefusesAnIncompleteSection(t *testing.T) {
	dir := t.TempDir()
	if err := startMirrorExecutor(t, mirrorDeployment(t, dir)); err != nil {
		t.Fatalf("a complete mirror deployment did not start: %v", err)
	}
	cases := []struct {
		name   string
		mutate func(*CloneExecutorServiceConfig)
		reason string
	}{
		{"no per-mirror limit", func(c *CloneExecutorServiceConfig) { c.MirrorMaxBytes = 0 }, "the store would refuse every publication, including ones it should accept"},
		{"no volume limit", func(c *CloneExecutorServiceConfig) { c.MirrorVolumeBytes = 0 }, "a store with no volume budget is a store that fills its disk"},
		{"a volume limit below the per-mirror limit", func(c *CloneExecutorServiceConfig) { c.MirrorVolumeBytes = c.MirrorMaxBytes - 1 }, "one mirror could never be published, so the two limits describe no reachable state"},
		{"a relative mirror root", func(c *CloneExecutorServiceConfig) { c.MirrorRoot = "mirrors" }, "the mirrors' location would depend on the working directory"},
		{"no verification key", func(c *CloneExecutorServiceConfig) { c.AuthorizationPublicKeyFile = "" }, "no mirror authorization could be verified"},
		{"no credential service endpoint", func(c *CloneExecutorServiceConfig) { c.StoreEndpoint = "" }, "the executor could not redeem a mirror authorization"},
		{"no git program", func(c *CloneExecutorServiceConfig) { c.GitPath = "" }, "the executor would fetch with whatever `git` resolves to"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			cfg := mirrorDeployment(t, t.TempDir())
			test.mutate(&cfg)
			if err := startMirrorExecutor(t, cfg); err == nil {
				t.Fatalf("an incomplete mirror deployment started: %s", test.reason)
			}
		})
	}
}

// TestCloneExecutorMirrorRefusesARootItWouldSweepAway is the containment rule, and the reason it
// cannot be a warning.
//
// `credentialmaterial.Sweep` removes *every* entry under the materialization root and the clone
// executor runs it at startup, so a mirror root nested inside it would be deleted — published mirrors
// the internal Git Server is serving — by a process that was only cleaning up after itself. The reverse
// nesting puts credential material on the volume the Git Server serves. Neither is a state a
// deployment should be able to describe, so neither is one it can start in.
func TestCloneExecutorMirrorRefusesARootItWouldSweepAway(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*CloneExecutorServiceConfig)
	}{
		{"the mirror root is inside the materialization root", func(c *CloneExecutorServiceConfig) {
			c.MirrorRoot = filepath.Join(c.MaterialRoot, "mirrors")
		}},
		{"the mirror root contains the materialization root", func(c *CloneExecutorServiceConfig) {
			c.MirrorRoot = filepath.Dir(c.MaterialRoot)
		}},
		{"the mirror root is the materialization root", func(c *CloneExecutorServiceConfig) {
			c.MirrorRoot = c.MaterialRoot
		}},
		{"the mirror root is inside the checkout root", func(c *CloneExecutorServiceConfig) {
			c.MirrorRoot = filepath.Join(c.CheckoutRoot, "mirrors")
		}},
		{"the mirror root contains the checkout root", func(c *CloneExecutorServiceConfig) {
			c.MirrorRoot = filepath.Dir(c.CheckoutRoot)
		}},
		{"the mirror root is the checkout root", func(c *CloneExecutorServiceConfig) {
			c.MirrorRoot = c.CheckoutRoot
		}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			cfg := mirrorDeployment(t, t.TempDir())
			test.mutate(&cfg)
			if err := startMirrorExecutor(t, cfg); err == nil {
				t.Fatal("a deployment whose roots contain one another started")
			}
		})
	}
	// The control: a sibling directory under the same parent is a legitimate deployment, so the rule
	// above is containment and not merely "shares an ancestor".
	cfg := mirrorDeployment(t, t.TempDir())
	if err := startMirrorExecutor(t, cfg); err != nil {
		t.Fatalf("a deployment with a sibling mirror root was refused: %v", err)
	}
}

// writeSigningKey writes Cloud's Ed25519 authorization signing key, in the PKCS#8 form the deployment
// generates it with. The test file is temporary, so no key material reaches a durable fixture.
func writeSigningKey(t *testing.T, dir string) string {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate the signing key: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		t.Fatalf("marshal the signing key: %v", err)
	}
	path := filepath.Join(dir, "cloneauth-signing.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatalf("write the signing key: %v", err)
	}
	return path
}

// dispatchDeployment builds the shared half of the executor dispatch section, complete except where a
// case changes it.
func dispatchDeployment(t *testing.T, dir string) ExecutorDispatchConfig {
	t.Helper()
	certificate, key, ca := writePKI(t, dir, "clone-dispatcher.test", "spiffe://ora/cloud/clone-dispatcher")
	return ExecutorDispatchConfig{
		Endpoint:        "https://clone-executor:8444",
		CertificateFile: certificate,
		PrivateKeyFile:  key,
		CAFile:          ca,
		SigningKeyFile:  writeSigningKey(t, dir),
		Timeout:         5 * time.Minute,
	}
}

// TestExecutorDispatchBuildsOnlyTheCapabilitiesTheDeploymentEnabled pins the whole point of splitting
// the switches: a deployment that enables none installs nothing and builds nothing, one that enables
// any gets that capability alone, and the transport and key are shared rather than built three times.
func TestExecutorDispatchBuildsOnlyTheCapabilitiesTheDeploymentEnabled(t *testing.T) {
	cases := []struct {
		name       string
		mutate     func(*ExecutorDispatchConfig)
		wantClone  bool
		wantMirror bool
		wantPush   bool
	}{
		{
			// The approved fail-closed default: no capability, and no key or certificate is even read,
			// so an absent capability cannot fail startup.
			name: "no capability",
		},
		{
			name:      "clone only",
			mutate:    func(c *ExecutorDispatchConfig) { c.Enabled = true },
			wantClone: true,
		},
		{
			// The incremental-rollout case: the mirror path can be switched on while the clone path
			// stays refused, which is what keeps `repository_credential_executor_unavailable` the answer
			// for every credentialed clone until that path has its own acceptance evidence.
			name: "mirror only",
			mutate: func(c *ExecutorDispatchConfig) {
				c.MirrorEnabled = true
				c.ExecutorIdentity = "spiffe://ora.local/credential-store/broker-resolver"
			},
			wantMirror: true,
		},
		{
			// The case the whole write boundary is rolled out on: push alone. Every credentialed clone
			// stays refused while a push is dispatched, which is the only way to enable the write side
			// without also enabling a read path that has no acceptance evidence yet.
			name: "push only",
			mutate: func(c *ExecutorDispatchConfig) {
				c.PushEnabled = true
				c.ExecutorIdentity = "spiffe://ora.local/credential-store/broker-resolver"
			},
			wantPush: true,
		},
		{
			name: "clone and mirror",
			mutate: func(c *ExecutorDispatchConfig) {
				c.Enabled = true
				c.MirrorEnabled = true
				c.ExecutorIdentity = "spiffe://ora.local/credential-store/broker-resolver"
			},
			wantClone:  true,
			wantMirror: true,
		},
		{
			name: "every capability",
			mutate: func(c *ExecutorDispatchConfig) {
				c.Enabled = true
				c.MirrorEnabled = true
				c.PushEnabled = true
				c.ExecutorIdentity = "spiffe://ora.local/credential-store/broker-resolver"
			},
			wantClone:  true,
			wantMirror: true,
			wantPush:   true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := dispatchDeployment(t, t.TempDir())
			if c.mutate != nil {
				c.mutate(&cfg)
			}
			dispatch, err := cfg.Open()
			if err != nil {
				t.Fatalf("a complete deployment was refused: %v", err)
			}
			if dispatch == nil {
				t.Fatal("a section that was read returned no dispatch")
			}
			if (dispatch.Clone != nil) != c.wantClone {
				t.Fatalf("the clone capability is %v, want %v", dispatch.Clone != nil, c.wantClone)
			}
			if (dispatch.Mirror != nil) != c.wantMirror {
				t.Fatalf("the mirror capability is %v, want %v", dispatch.Mirror != nil, c.wantMirror)
			}
			if (dispatch.Push != nil) != c.wantPush {
				t.Fatalf("the push capability is %v, want %v", dispatch.Push != nil, c.wantPush)
			}
			if !c.wantClone && !c.wantMirror && !c.wantPush {
				// Nothing was enabled, so nothing may have been built: a signing key loaded "just in
				// case" is a capability present without a switch.
				if dispatch.Client != nil || dispatch.SigningKey != nil {
					t.Fatal("a section with no capability enabled built a transport or loaded a key")
				}
				return
			}
			if dispatch.Client == nil || len(dispatch.SigningKey) != ed25519.PrivateKeySize {
				t.Fatal("an enabled capability was built without a transport or a signing key")
			}
			if c.wantClone && dispatch.Clone.Lifetime != DefaultAuthorizationLifetime {
				t.Fatalf("the clone authorization lifetime is %s, want the default %s", dispatch.Clone.Lifetime, DefaultAuthorizationLifetime)
			}
			if c.wantMirror {
				if dispatch.Mirror.Lifetime != DefaultMirrorAuthorizationLifetime {
					t.Fatalf("the mirror authorization lifetime is %s, want the default %s", dispatch.Mirror.Lifetime, DefaultMirrorAuthorizationLifetime)
				}
				if dispatch.Mirror.ExecutorIdentity == "" {
					t.Fatal("the mirror capability did not carry the executor identity")
				}
			}
			if c.wantPush {
				// A push's authorization is the one whose lifetime is also the width of the window in
				// which an approval can still be redeemed after the writer has stopped waiting for it,
				// so the default is the bound rather than a number this section may choose freely.
				if dispatch.Push.Lifetime != core.PushDispatchLifetime {
					t.Fatalf("the push authorization lifetime is %s, want the bound %s", dispatch.Push.Lifetime, core.PushDispatchLifetime)
				}
				if dispatch.Push.ExecutorIdentity == "" {
					t.Fatal("the push capability did not carry the executor identity")
				}
			}
		})
	}
}

// TestExecutorDispatchRefusesAHalfDeclaredCapability pins that an enabled capability is an error when
// its half of the boundary is missing, rather than a capability that starts and fails per job. The
// shared half is required by either capability, because both sign with the same key over the same
// connection.
func TestExecutorDispatchRefusesAHalfDeclaredCapability(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*ExecutorDispatchConfig)
	}{
		{"only the clone capability is enabled, with no signing key", func(c *ExecutorDispatchConfig) {
			c.Enabled = true
			c.SigningKeyFile = ""
		}},
		{"only the mirror capability is enabled, with no signing key", func(c *ExecutorDispatchConfig) {
			c.MirrorEnabled = true
			c.ExecutorIdentity = "spiffe://ora.local/credential-store/broker-resolver"
			c.SigningKeyFile = ""
		}},
		{"the mirror capability is enabled with no executor identity", func(c *ExecutorDispatchConfig) {
			c.MirrorEnabled = true
		}},
		{"the mirror capability is enabled with a blank executor identity", func(c *ExecutorDispatchConfig) {
			c.MirrorEnabled = true
			c.ExecutorIdentity = "   "
		}},
		{"the executor identity is not a URI", func(c *ExecutorDispatchConfig) {
			c.MirrorEnabled = true
			c.ExecutorIdentity = "://"
		}},
		{"the executor identity is not SPIFFE", func(c *ExecutorDispatchConfig) {
			c.MirrorEnabled = true
			c.ExecutorIdentity = "https://ora.local/credential-store/broker-resolver"
		}},
		{"the executor identity names no workload", func(c *ExecutorDispatchConfig) {
			c.MirrorEnabled = true
			c.ExecutorIdentity = "spiffe://ora.local"
		}},
		{"the executor identity names two workloads", func(c *ExecutorDispatchConfig) {
			c.MirrorEnabled = true
			c.ExecutorIdentity = "spiffe://ora.local/credential-store/broker/resolver"
		}},
		{"the executor identity carries a query", func(c *ExecutorDispatchConfig) {
			c.MirrorEnabled = true
			c.ExecutorIdentity = "spiffe://ora.local/credential-store/broker-resolver?x=1"
		}},
		{"the signing key file does not exist", func(c *ExecutorDispatchConfig) {
			c.Enabled = true
			c.SigningKeyFile = "absent.pem"
		}},
		{"the dispatcher certificate does not exist", func(c *ExecutorDispatchConfig) {
			c.Enabled = true
			c.CertificateFile = "absent.pem"
		}},
		{"the endpoint is not HTTPS", func(c *ExecutorDispatchConfig) {
			c.Enabled = true
			c.Endpoint = "http://clone-executor:8444"
		}},
		{"the endpoint is empty", func(c *ExecutorDispatchConfig) {
			c.Enabled = true
			c.Endpoint = ""
		}},
		{
			// The bound that is specific to the mirror capability: an authorization outliving its
			// attempt's lease authorizes a fetch whose result the generation fence will refuse.
			name: "the mirror authorization lifetime exceeds the preparation lease", mutate: func(c *ExecutorDispatchConfig) {
				c.MirrorEnabled = true
				c.ExecutorIdentity = "spiffe://ora.local/credential-store/broker-resolver"
				c.MirrorAuthorizationLifetime = core.MirrorPreparationLease + time.Second
			},
		},
		{
			name: "the clone authorization lifetime exceeds the decision's ceiling", mutate: func(c *ExecutorDispatchConfig) {
				c.Enabled = true
				c.AuthorizationLifetime = cloneauth.MaxLifetime + time.Second
			},
		},
		{
			// Enabled state is not what makes a section usable: a deployment that turns a capability on
			// and leaves the shared half empty must not start a capability it cannot establish.
			name: "the mirror capability is enabled with no endpoint", mutate: func(c *ExecutorDispatchConfig) {
				c.MirrorEnabled = true
				c.ExecutorIdentity = "spiffe://ora.local/credential-store/broker-resolver"
				c.Endpoint = ""
			},
		},
		{"only the push capability is enabled, with no signing key", func(c *ExecutorDispatchConfig) {
			c.PushEnabled = true
			c.ExecutorIdentity = "spiffe://ora.local/credential-store/broker-resolver"
			c.SigningKeyFile = ""
		}},
		{"the push capability is enabled with no executor identity", func(c *ExecutorDispatchConfig) {
			c.PushEnabled = true
		}},
		{"the push capability is enabled with a blank executor identity", func(c *ExecutorDispatchConfig) {
			c.PushEnabled = true
			c.ExecutorIdentity = "   "
		}},
		{
			// The same shape the mirror cases pin, repeated for push because it is the capability whose
			// identity is checked by the Store rather than by the executor: a non-SPIFFE value here would
			// be signed into every approval as the executor the Store must see.
			name: "the push executor identity is not SPIFFE", mutate: func(c *ExecutorDispatchConfig) {
				c.PushEnabled = true
				c.ExecutorIdentity = "https://ora.local/credential-store/broker-resolver"
			},
		},
		{
			// The bound that is specific to the push capability, and the only one whose value is also a
			// security window: the authorization stays redeemable from the moment it is signed, so a
			// lifetime longer than the bound widens the interval in which a late executor can still write
			// after Cloud stopped waiting for the result.
			name: "the push authorization lifetime exceeds the revocation window bound", mutate: func(c *ExecutorDispatchConfig) {
				c.PushEnabled = true
				c.ExecutorIdentity = "spiffe://ora.local/credential-store/broker-resolver"
				c.PushAuthorizationLifetime = core.PushDispatchLifetime + time.Second
			},
		},
		{
			name: "the push capability is enabled with no endpoint", mutate: func(c *ExecutorDispatchConfig) {
				c.PushEnabled = true
				c.ExecutorIdentity = "spiffe://ora.local/credential-store/broker-resolver"
				c.Endpoint = ""
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := dispatchDeployment(t, t.TempDir())
			c.mutate(&cfg)
			if dispatch, err := cfg.Open(); err == nil {
				t.Fatalf("a half-declared deployment opened: %+v", dispatch)
			}
		})
	}
	// The control: the same shapes are accepted when the shared half is present, so the refusals above
	// are about what is missing and not about the section being impossible to configure. The push
	// lifetime is set to exactly the bound, which is the value the ceiling must admit.
	cfg := dispatchDeployment(t, t.TempDir())
	cfg.Enabled, cfg.MirrorEnabled, cfg.PushEnabled = true, true, true
	cfg.ExecutorIdentity = "spiffe://ora.local/credential-store/broker-resolver"
	cfg.AuthorizationLifetime = cloneauth.MaxLifetime
	cfg.MirrorAuthorizationLifetime = core.MirrorPreparationLease
	cfg.PushAuthorizationLifetime = core.PushDispatchLifetime
	if _, err := cfg.Open(); err != nil {
		t.Fatalf("a complete deployment was refused: %v", err)
	}
}
