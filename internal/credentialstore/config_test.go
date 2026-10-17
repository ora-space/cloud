package credentialstore

import (
	"crypto/ed25519"
	"strings"
	"testing"
)

func validEnv() map[string]string {
	return map[string]string{
		"CREDENTIAL_STORE_ROOT":                        "/var/lib/credential-store",
		"CREDENTIAL_STORE_KEYRING_FILE":                "/etc/credential-store/keyring.json",
		"CREDENTIAL_STORE_CERT_FILE":                   "/etc/credential-store/server.pem",
		"CREDENTIAL_STORE_KEY_FILE":                    "/etc/credential-store/server.key",
		"CREDENTIAL_STORE_CLIENT_CA_FILE":              "/etc/credential-store/client-ca.pem",
		"CREDENTIAL_STORE_EXECUTION_PUBLIC_KEYS_FILE":  "/etc/credential-store/execution.pub",
		"CREDENTIAL_STORE_SUBMISSION_PUBLIC_KEYS_FILE": "/etc/credential-store/submission.pub",
		"CREDENTIAL_STORE_INGRESS_CERT_FILE":           "/etc/credential-store/ingress.pem",
		"CREDENTIAL_STORE_INGRESS_KEY_FILE":            "/etc/credential-store/ingress.key",
		"CREDENTIAL_STORE_INGRESS_ALLOWED_ORIGINS":     "https://app.ora.example, http://localhost:5173",
	}
}

func lookup(env map[string]string) func(string) string {
	return func(k string) string { return env[k] }
}

func TestLoadConfigDefaults(t *testing.T) {
	cfg, err := LoadConfig(lookup(validEnv()))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.RedemptionAddr != ":8461" || cfg.ManagementAddr != ":8462" || cfg.KeyAddr != ":8463" || cfg.IngressAddr != ":8460" {
		t.Fatalf("addresses = %s %s %s %s, want the documented defaults", cfg.RedemptionAddr, cfg.ManagementAddr, cfg.KeyAddr, cfg.IngressAddr)
	}
	if cfg.IngressRatePerMinute != DefaultRatePerMinute || cfg.IngressRateBurst != DefaultRateBurst {
		t.Fatalf("rate = %d/%d, want %d/%d", cfg.IngressRatePerMinute, cfg.IngressRateBurst, DefaultRatePerMinute, DefaultRateBurst)
	}
	if len(cfg.IngressAllowedOrigins) != 2 || cfg.IngressAllowedOrigins[1] != "http://localhost:5173" {
		t.Fatalf("origins = %v", cfg.IngressAllowedOrigins)
	}
	if cfg.Identities != DefaultIdentities() {
		t.Fatalf("identities = %+v, want the deployed topology", cfg.Identities)
	}
}

func TestLoadConfigRefusals(t *testing.T) {
	cases := map[string]map[string]string{
		"missing keyring":         {"CREDENTIAL_STORE_KEYRING_FILE": ""},
		"relative root":           {"CREDENTIAL_STORE_ROOT": "data"},
		"shared key files":        {"CREDENTIAL_STORE_SUBMISSION_PUBLIC_KEYS_FILE": "/etc/credential-store/execution.pub"},
		"duplicate listener":      {"CREDENTIAL_STORE_KEY_ADDR": ":8461"},
		"bad address":             {"CREDENTIAL_STORE_INGRESS_ADDR": "8460"},
		"no origin":               {"CREDENTIAL_STORE_INGRESS_ALLOWED_ORIGINS": ""},
		"wildcard origin":         {"CREDENTIAL_STORE_INGRESS_ALLOWED_ORIGINS": "*"},
		"origin with path":        {"CREDENTIAL_STORE_INGRESS_ALLOWED_ORIGINS": "https://app.ora.example/"},
		"plain http remote":       {"CREDENTIAL_STORE_INGRESS_ALLOWED_ORIGINS": "http://app.ora.example"},
		"zero rate":               {"CREDENTIAL_STORE_INGRESS_RATE_PER_MINUTE": "0"},
		"non-numeric rate":        {"CREDENTIAL_STORE_INGRESS_RATE_PER_MINUTE": "ten"},
		"zero burst":              {"CREDENTIAL_STORE_INGRESS_RATE_BURST": "-1"},
		"missing ingress cert":    {"CREDENTIAL_STORE_INGRESS_CERT_FILE": ""},
		"missing client CA":       {"CREDENTIAL_STORE_CLIENT_CA_FILE": ""},
		"missing execution keys":  {"CREDENTIAL_STORE_EXECUTION_PUBLIC_KEYS_FILE": ""},
		"missing submission keys": {"CREDENTIAL_STORE_SUBMISSION_PUBLIC_KEYS_FILE": ""},
	}
	for name, override := range cases {
		t.Run(name, func(t *testing.T) {
			env := validEnv()
			for k, v := range override {
				env[k] = v
			}
			if _, err := LoadConfig(lookup(env)); err == nil {
				t.Fatal("LoadConfig accepted an invalid configuration")
			}
		})
	}
}

func TestIdentitiesRefuseOverlap(t *testing.T) {
	cases := map[string]Identities{
		"redeemer also manages":     {Redeemer: ExecutorIdentity, Manager: ExecutorIdentity, KeyOperator: KEKOperatorIdentity},
		"manager also operates key": {Redeemer: ExecutorIdentity, Manager: KEKOperatorIdentity, KeyOperator: KEKOperatorIdentity},
		"redeemer also operates":    {Redeemer: KEKOperatorIdentity, Manager: CredentialManagerIdentity, KeyOperator: KEKOperatorIdentity},
		"missing":                   {Redeemer: ExecutorIdentity, Manager: CredentialManagerIdentity},
		"not spiffe":                {Redeemer: "https://ora.local/repo-executor", Manager: CredentialManagerIdentity, KeyOperator: KEKOperatorIdentity},
		"no path":                   {Redeemer: "spiffe://ora.local", Manager: CredentialManagerIdentity, KeyOperator: KEKOperatorIdentity},
	}
	for name, identities := range cases {
		if err := identities.Validate(); err == nil {
			t.Fatalf("%s: Validate accepted %+v", name, identities)
		}
		cfg, err := LoadConfig(lookup(validEnv()))
		if err != nil {
			t.Fatal(err)
		}
		cfg.Identities = identities
		if err := cfg.Validate(); err == nil {
			t.Fatalf("%s: Config.Validate accepted overlapping identities", name)
		}
	}
	if err := DefaultIdentities().Validate(); err != nil {
		t.Fatalf("default identities: %v", err)
	}
}

func TestNewServerRefusesSharedSigningKeys(t *testing.T) {
	h := newHarness(t)
	shared := h.executionKey.Public().(ed25519.PublicKey)
	_, err := NewServer(&ServerOptions{
		Store: h.store, ExecutionKeys: []ed25519.PublicKey{shared}, SubmissionKeys: []ed25519.PublicKey{shared},
		Identities: DefaultIdentities(), AllowedOrigins: []string{testOrigin}, RatePerMinute: 1, RateBurst: 1,
	})
	if err == nil || !strings.Contains(err.Error(), "distinct") {
		t.Fatalf("NewServer = %v, want a refusal of shared signing keys", err)
	}
}
