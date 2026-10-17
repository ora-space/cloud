// Package credentialmaterial owns the on-disk layout in which the Repo Executor stages one redeemed
// Git credential for the duration of one task, and nothing else.
//
// Two programs must agree about the layout and are deliberately kept apart: the executor writes and
// destroys the material, and the fixed credential helper git-credential-ora reads it. The helper is on
// the path Git executes, so it links only this package (and the repository normalization it needs);
// sharing exactly the layout keeps the two consistent without making the helper a second executor.
//
// Layout:
//
//	<root>/                 deployment-owned
//	└── <random>/           one task's material, mode 0700
//	    ├── scope           mode 0600, the normalized repository the credential may be presented to
//	    ├── username        mode 0600
//	    └── password        mode 0600
//
// Each file holds one value with no envelope, so nothing can smuggle a second value past a reader,
// and the plaintext reaches Git only through the helper — never through arguments, environment, URLs
// or Git configuration.
package credentialmaterial

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/wanglongan587/cloud/internal/cloneauth"
)

// File names inside one material directory.
const (
	ScopeFile    = "scope"
	UsernameFile = "username"
	PasswordFile = "password"
)

// DirectoryMode and FileMode are constants rather than configuration because the isolation argument
// depends on them exactly: only the executor's own user may list the directory or read the files.
const (
	DirectoryMode os.FileMode = 0o700
	FileMode      os.FileMode = 0o600
)

// Sentinel failures. None of them carries any part of the material.
var (
	// ErrUnsafePath reports a root or material directory that is not an absolute, clean path.
	ErrUnsafePath = errors.New("credentialmaterial: path is not absolute and clean")
	// ErrInvalid reports a scope or secret that cannot be staged.
	ErrInvalid = errors.New("credentialmaterial: material is invalid")
	// ErrTampered reports material whose type or permissions are not what Stage created; the safe
	// answer is to deliver nothing.
	ErrTampered = errors.New("credentialmaterial: material does not have the staged type or mode")
	// ErrAbsent reports material that is not present.
	ErrAbsent = errors.New("credentialmaterial: material is not present")
)

// Scope is the repository a staged credential may be presented to, split into the host (with a
// non-default port) and the path, exactly as cloneauth.NormalizeRepository spells them.
type Scope struct {
	Host string
	Path string
}

// NewScope normalizes repository and splits it into a Scope.
func NewScope(repository string) (Scope, error) {
	normalized, err := cloneauth.NormalizeRepository(repository)
	if err != nil {
		return Scope{}, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	host, path, _ := strings.Cut(strings.TrimPrefix(normalized, "https://"), "/")
	return Scope{Host: host, Path: "/" + path}, nil
}

// Repository returns the scope as the normalized repository URL.
func (s Scope) Repository() string { return "https://" + s.Host + s.Path }

// validate refuses a scope that is not already in normalized form, so the staged scope is exactly the
// reduction the helper applies to Git's request and a caller cannot widen it by staging a spelling the
// helper would read differently.
func (s Scope) validate() error {
	if s.Host == "" || strings.Contains(s.Host, "/") || !strings.HasPrefix(s.Path, "/") {
		return fmt.Errorf("%w: scope is not a host and an absolute path", ErrInvalid)
	}
	normalized, err := cloneauth.NormalizeRepository(s.Repository())
	if err != nil || normalized != s.Repository() {
		return fmt.Errorf("%w: scope is not a normalized repository", ErrInvalid)
	}
	return nil
}

// Material is one staged credential. Its directory exists from a successful Stage until Destroy
// succeeds.
type Material struct {
	root string
	dir  string
}

// Dir returns the absolute material directory, the argument the executor passes to the helper.
func (m *Material) Dir() string { return m.dir }

// Stage writes one credential and its scope into a new private directory under root.
//
// The directory gets a random name and is created with Mkdir, which refuses to merge into an existing
// one. Each file is created with O_CREATE|O_EXCL and mode 0600, fsynced, and then the directory and the
// root are fsynced, so a crash cannot leave a directory whose files hold a partial secret.
//
// Username and password must be non-empty and free of CR, LF and NUL: the helper prints them as lines
// of Git's credential protocol, and a newline would let a secret inject a second attribute.
//
// A failure removes what this call created before returning; if that removal also fails the error
// says so, because a secret may then still be on disk.
func Stage(root string, scope Scope, username, password string) (_ *Material, err error) {
	if err := validatePath(root); err != nil {
		return nil, err
	}
	if err := scope.validate(); err != nil {
		return nil, err
	}
	for _, value := range []struct{ name, value string }{{"username", username}, {"password", password}} {
		if value.value == "" || strings.ContainsAny(value.value, "\r\n\x00") {
			return nil, fmt.Errorf("%w: %s is empty or contains CR, LF or NUL", ErrInvalid, value.name)
		}
	}
	name, err := randomName()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(root, name)
	if err := os.Mkdir(dir, DirectoryMode); err != nil {
		return nil, fmt.Errorf("credentialmaterial: create directory: %w", err)
	}
	// From here the directory is this call's, so every failure removes it.
	defer func() {
		if err == nil {
			return
		}
		if removeErr := os.RemoveAll(dir); removeErr != nil {
			err = errors.Join(err, fmt.Errorf("credentialmaterial: remove partially staged material: %w", removeErr))
		}
	}()
	// Mkdir applies the umask; the isolation needs exactly DirectoryMode.
	if err := os.Chmod(dir, DirectoryMode); err != nil {
		return nil, fmt.Errorf("credentialmaterial: set directory mode: %w", err)
	}
	for _, file := range []struct{ name, content string }{
		{ScopeFile, scope.Repository()},
		{UsernameFile, username},
		{PasswordFile, password},
	} {
		if err := writeExclusive(filepath.Join(dir, file.name), file.content); err != nil {
			return nil, err
		}
	}
	if err := syncDirectory(dir); err != nil {
		return nil, err
	}
	if err := syncDirectory(root); err != nil {
		return nil, err
	}
	return &Material{root: root, dir: dir}, nil
}

// Destroy removes the material directory and proves it is gone.
//
// "Removed and verified" rather than best effort: the caller must report a cleanup failure when the
// secret could still be readable. The root is fsynced so a crash after a reported success cannot bring
// the entry back. Destroy is idempotent.
func (m *Material) Destroy() error {
	if err := os.RemoveAll(m.dir); err != nil {
		return fmt.Errorf("credentialmaterial: destroy: %w", err)
	}
	if _, err := os.Lstat(m.dir); err == nil {
		return errors.New("credentialmaterial: material survived its destruction")
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("credentialmaterial: verify destruction: %w", err)
	}
	return syncDirectory(m.root)
}

// Sweep removes every entry under root and reports how many it removed.
//
// It runs at executor startup, before any task is accepted, because a crash or a killed container
// leaves material that the process that would have destroyed it can no longer reach. The root exists
// for no other purpose, so every entry is removed, not only names Stage could have produced.
func Sweep(root string) (int, error) {
	if err := validatePath(root); err != nil {
		return 0, err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return 0, fmt.Errorf("credentialmaterial: read root: %w", err)
	}
	removed := 0
	for _, entry := range entries {
		target := filepath.Join(root, entry.Name())
		if err := os.RemoveAll(target); err != nil {
			return removed, fmt.Errorf("credentialmaterial: sweep: %w", err)
		}
		if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
			return removed, errors.New("credentialmaterial: swept material survived its removal")
		}
		removed++
	}
	return removed, syncDirectory(root)
}

// randomName returns 128 random bits in hex, so concurrent tasks never collide and a name reveals
// nothing about the task.
func randomName() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("credentialmaterial: random name: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// writeExclusive creates one file exclusively with FileMode, writes content, fsyncs and closes it.
func writeExclusive(path, content string) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, FileMode) //nolint:gosec // G304: path is a fixed name inside a directory this call just created.
	if err != nil {
		return fmt.Errorf("credentialmaterial: create file: %w", err)
	}
	if _, err := file.WriteString(content); err != nil {
		return errors.Join(fmt.Errorf("credentialmaterial: write file: %w", err), file.Close())
	}
	if err := file.Sync(); err != nil {
		return errors.Join(fmt.Errorf("credentialmaterial: sync file: %w", err), file.Close())
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("credentialmaterial: close file: %w", err)
	}
	return nil
}

// syncDirectory fsyncs a directory so a create or delete inside it survives a power loss.
func syncDirectory(path string) error {
	handle, err := os.Open(path) //nolint:gosec // G304: path is the material root or a directory this package created.
	if err != nil {
		return fmt.Errorf("credentialmaterial: open directory for sync: %w", err)
	}
	if err := handle.Sync(); err != nil {
		return errors.Join(fmt.Errorf("credentialmaterial: sync directory: %w", err), handle.Close())
	}
	return handle.Close()
}

// validatePath refuses a path that is not absolute and clean, so the material's location never
// depends on the working directory or on a "..".
func validatePath(path string) error {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return ErrUnsafePath
	}
	return nil
}
