package credentialmaterial

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ReadScope returns the scope staged in dir. The helper reads it before the secret, so a request for
// another repository never causes the secret to be read at all.
func ReadScope(dir string) (Scope, error) {
	content, err := readFile(dir, ScopeFile)
	if err != nil {
		return Scope{}, err
	}
	scope, err := NewScope(content)
	if err != nil || scope.Repository() != content {
		// The file must hold exactly the normalized spelling Stage wrote; anything else is not read
		// as a scope, so damaged material is delivered to nobody.
		return Scope{}, fmt.Errorf("%w: scope file is not a normalized repository", ErrTampered)
	}
	return scope, nil
}

// ReadSecret returns the staged username and password.
func ReadSecret(dir string) (username, password string, err error) {
	if username, err = readFile(dir, UsernameFile); err != nil {
		return "", "", err
	}
	if password, err = readFile(dir, PasswordFile); err != nil {
		return "", "", err
	}
	return username, password, nil
}

// readFile reads one material file after checking that the directory and the file have exactly the
// type and mode Stage created. The check runs on every read: a symlink or a wider mode means something
// other than the executor changed the material.
func readFile(dir, name string) (string, error) {
	if err := validatePath(dir); err != nil {
		return "", err
	}
	if err := checkEntry(dir, true); err != nil {
		return "", err
	}
	path := filepath.Join(dir, name)
	if err := checkEntry(path, false); err != nil {
		return "", err
	}
	raw, err := os.ReadFile(path) //nolint:gosec // G304: path is a fixed name inside a checked material directory.
	if err != nil {
		return "", fmt.Errorf("credentialmaterial: read %s: %w", name, err)
	}
	if len(raw) == 0 {
		return "", ErrAbsent
	}
	return string(raw), nil
}

// checkEntry requires path to be a directory with DirectoryMode or a regular file with FileMode, and
// not a symlink.
func checkEntry(path string, directory bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ErrAbsent
		}
		return fmt.Errorf("credentialmaterial: inspect: %w", err)
	}
	if directory {
		if !info.IsDir() || info.Mode().Perm() != DirectoryMode {
			return ErrTampered
		}
		return nil
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != FileMode {
		return ErrTampered
	}
	return nil
}
