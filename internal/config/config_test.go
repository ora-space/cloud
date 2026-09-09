package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadParsesDurations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	contents := []byte("server:\n  read_timeout: 10s\n  write_timeout: 15s\ndatabase:\n  conn_max_lifetime: 1h\n")
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.ReadTimeout != 10*time.Second || cfg.Server.WriteTimeout != 15*time.Second || cfg.Database.ConnMaxLifetime != time.Hour {
		t.Fatalf("durations were not parsed: %+v %+v", cfg.Server, cfg.Database)
	}
}
