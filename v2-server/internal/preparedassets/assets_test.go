package preparedassets

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func TestResourceRelocationAndLegacyAbsolutePath(t *testing.T) {
	source := filepath.Join(t.TempDir(), "original")
	if err := os.MkdirAll(filepath.Join(source, "bin"), 0700); err != nil {
		t.Fatal(err)
	}
	data := []byte("unit fixture, not a release executable")
	sum := sha256.Sum256(data)
	a := asset{Path: "bin/ora-node", SHA256: hex.EncodeToString(sum[:])}
	if err := os.WriteFile(filepath.Join(source, a.Path), data, 0700); err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(t.TempDir(), "moved")
	if err := os.Rename(source, moved); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(moved)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := verifyAsset(root, moved, "bin/ora-node", a); err != nil {
		t.Fatalf("relative moved: %v", err)
	}
	a.Path = filepath.Join(moved, "bin/ora-node")
	if err := verifyAsset(root, moved, "bin/ora-node", a); err != nil {
		t.Fatalf("legacy absolute: %v", err)
	}
	a.Path = "../outside"
	if err := verifyAsset(root, moved, "bin/ora-node", a); err == nil {
		t.Fatal("escaping manifest path accepted")
	}
	a.Path = "bin/ora-node"
	a.SHA256 = "bad"
	if err := verifyAsset(root, moved, "bin/ora-node", a); err == nil {
		t.Fatal("digest mismatch accepted")
	}
}
