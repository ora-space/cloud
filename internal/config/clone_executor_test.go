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
	if _, err := os.Stat(cfg.AuthorizationPublicKeyFile); err != nil {
		t.Fatalf("the verification key the environment named is not the file that was written: %v", err)
	}
	// The whole point of the separate loader: the Cloud variable above must not have become the
	// executor's endpoint.
	if cfg.StoreEndpoint == "https://elsewhere:8444" {
		t.Fatal("a Cloud-prefixed variable reached the executor's configuration")
	}
}
