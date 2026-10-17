package main

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/wanglongan587/cloud/internal/credentialstore"
)

// TestRunRefusesMissingKeyring: without a usable keyring the command exits before binding any
// listener or opening the volume. The test mutates process environment, so it is not parallel.
func TestRunRefusesMissingKeyring(t *testing.T) {
	dir := t.TempDir()
	env := map[string]string{
		"ROOT":                        filepath.Join(dir, "store"),
		"KEYRING_FILE":                filepath.Join(dir, "absent-keyring.json"),
		"CERT_FILE":                   filepath.Join(dir, "server.pem"),
		"KEY_FILE":                    filepath.Join(dir, "server.key"),
		"CLIENT_CA_FILE":              filepath.Join(dir, "ca.pem"),
		"EXECUTION_PUBLIC_KEYS_FILE":  filepath.Join(dir, "execution.pub"),
		"SUBMISSION_PUBLIC_KEYS_FILE": filepath.Join(dir, "submission.pub"),
		"INGRESS_CERT_FILE":           filepath.Join(dir, "ingress.pem"),
		"INGRESS_KEY_FILE":            filepath.Join(dir, "ingress.key"),
		"INGRESS_ALLOWED_ORIGINS":     "https://app.ora.example",
		"REDEMPTION_ADDR":             "127.0.0.1:0",
		"MANAGEMENT_ADDR":             "127.0.0.1:1",
		"KEY_ADDR":                    "127.0.0.1:2",
		"INGRESS_ADDR":                "127.0.0.1:3",
		"LOG_LEVEL":                   "error",
	}
	for key, value := range env {
		t.Setenv("CREDENTIAL_STORE_"+key, value)
	}
	if err := run(); !errors.Is(err, credentialstore.ErrKeyringUnavailable) {
		t.Fatalf("run = %v, want ErrKeyringUnavailable", err)
	}
}

// TestRunRefusesInvalidConfiguration: an incomplete environment is refused before anything else.
func TestRunRefusesInvalidConfiguration(t *testing.T) {
	t.Setenv("CREDENTIAL_STORE_ROOT", "")
	if err := run(); err == nil {
		t.Fatal("run accepted an empty configuration")
	}
}
