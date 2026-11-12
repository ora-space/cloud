//go:build linux

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoginPreflightBeforeEffects(t *testing.T) {
	for _, login := range []string{"", "unknown", "idaas"} {
		t.Run(login, func(t *testing.T) {
			t.Setenv("V2_LOGIN", login)
			root := filepath.Join(t.TempDir(), "must-not-exist")
			t.Setenv("V2_DATA_DIR", root)
			err := run("127.0.0.1:0", "invalid database URL", false, false, false)
			if err == nil {
				t.Fatal("startup accepted unsupported login")
			}
			if login == "idaas" && (!strings.Contains(err.Error(), "this build does not provide IDaaS login") || !strings.Contains(err.Error(), "must not fall back to email") || strings.Contains(err.Error(), "#")) {
				t.Fatalf("integration prerequisite missing: %v", err)
			}
			if _, err := os.Stat(root); !os.IsNotExist(err) {
				t.Fatal("startup touched filesystem before rejecting deployment")
			}
		})
	}
	if err := validateLogin("email", false); err != nil {
		t.Fatal(err)
	}
	if err := validateLogin("idaas", true); err == nil {
		t.Fatal("deployment accepted fixtures")
	}
}

func TestOptionalIntegrationUnsetAndEmpty(t *testing.T) {
	const key = "V2_PLUGIN_MARKETPLACE_URL"
	old, wasSet := os.LookupEnv(key)
	t.Cleanup(func() {
		if wasSet {
			os.Setenv(key, old)
		} else {
			os.Unsetenv(key)
		}
	})
	os.Unsetenv(key)
	if got := envDefault(key, "development"); got != "development" {
		t.Fatalf("unset=%q", got)
	}
	t.Setenv(key, "")
	if got := envDefault(key, "development"); got != "" {
		t.Fatalf("empty=%q", got)
	}
	t.Setenv(key, "https://internal.example/market")
	if got := envDefault(key, "development"); got != "https://internal.example/market" {
		t.Fatalf("custom=%q", got)
	}
}

func TestDeploymentPreflightPreservesExistingData(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "existing-data")
	if err := os.WriteFile(file, []byte("preserve existing data"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("V2_LOGIN", "idaas")
	t.Setenv("V2_DATA_DIR", root)
	t.Setenv("V2_USERS_FILE", "nonexistent-preset")
	if err := run("127.0.0.1:0", "invalid database URL", false, false, false); err == nil {
		t.Fatal("deployment unexpectedly started")
	}
	if data, err := os.ReadFile(file); err != nil || string(data) != "preserve existing data" {
		t.Fatalf("existing data changed: %q %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(root, "server.lock")); !os.IsNotExist(err) {
		t.Fatal("preflight acquired data directory")
	}
}
