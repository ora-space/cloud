// Package preparedassets verifies the offline resource manifest without downloading anything.
package preparedassets

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/wanglongan587/cloud/v2-server/internal/nodeservices"
)

type asset struct{ Path, SHA256 string }
type manifest struct {
	Version                            int    `json:"version"`
	SourceCommit                       string `json:"sourceCommit"`
	Profile                            string `json:"profile"`
	Node, Reaper, Deno, Plugin, Claude asset
}

// Verify accepts relative manifest paths confined to root and legacy absolute development paths.
// Executables are locally built, so their digests attest bundle integrity, not publisher identity.
func Verify(directory string) error {
	raw, err := os.ReadFile(filepath.Join(directory, "assets.json"))
	if err != nil {
		return err
	}
	var m manifest
	if err = json.Unmarshal(raw, &m); err != nil {
		return err
	}
	if m.Version != 1 || (m.Profile != "release" && m.Profile != "debug") {
		return errors.New("unsupported prepared assets manifest")
	}
	opencode, err := nodeservices.HostOpenCodePackage()
	if err != nil {
		return err
	}
	claude, err := nodeservices.HostClaudePackage()
	if err != nil {
		return err
	}
	if m.Plugin.SHA256 != opencode.SHA256 || m.Claude.SHA256 != claude.SHA256 {
		return errors.New("plugin digest does not match pinned release")
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return err
	}
	defer root.Close()
	for name, a := range map[string]asset{"bin/ora-node": m.Node, "bin/ora-reaper": m.Reaper, "bin/deno": m.Deno, "opencode-0.6.11.orax": m.Plugin, "claude-0.4.5.orax": m.Claude} {
		if err := verifyAsset(root, directory, name, a); err != nil {
			return err
		}
	}
	return nil
}

// Match the manifest to the fixed paths the runtime actually opens. Resolving a different file
// with the right digest must not validate an unchecked executable at the runtime path.
func verifyAsset(root *os.Root, directory, name string, a asset) error {
	if filepath.IsAbs(a.Path) {
		expected, err := filepath.Abs(filepath.Join(directory, name))
		if err != nil {
			return err
		}
		if filepath.Clean(a.Path) != expected {
			return errors.New("absolute asset path does not match runtime path")
		}
	} else if a.Path != name {
		return errors.New("relative asset path does not match runtime path")
	}
	f, err := root.Open(name)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("asset must be regular")
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, f); err != nil {
		return err
	}
	if hex.EncodeToString(hash.Sum(nil)) != a.SHA256 {
		return fmt.Errorf("prepared asset checksum mismatch: %s", filepath.Base(name))
	}
	return nil
}
