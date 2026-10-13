package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/internal/cloneexec"
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
