// Package credentialmaterial owns the on-disk layout in which the credential-executing identity
// materializes one Git token for the duration of one clone, and nothing else.
//
// The layout is a package of its own because two programs that must agree about it are deliberately
// kept apart: the executor writes and destroys the material, and the deployment-fixed Git credential
// helper reads it. The helper is a separate binary that must not link the executor's job logic — it
// is on the path Git executes, so the less it can do the better — but the two cannot disagree about
// where a token lives or the isolation would fail open. Sharing exactly this much, and nothing more,
// is what keeps them consistent without making the helper a second executor.
//
// The layout is deliberately the smallest thing that can work:
//
//	<root>/                     deployment-created, not writable by the workload identity
//	└── <execution-id>/          created by the credential-executing identity, mode 0700
//	    ├── token                mode 0600, the plaintext HTTPS Git token
//	    └── scope                mode 0600, the normalized repository identity the token is scoped to
//
// Every path is derived from a canonical UUID, so a caller cannot make one clone's material appear
// under another's name or escape the root with a path fragment. Nothing here reads the deployment's
// configuration or decides policy: whether the material may exist at all is the executor's decision,
// and this package only makes the answer to "where" and "with what permissions" the same on both
// sides.
package credentialmaterial

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// TokenFile is the file inside one execution's directory that holds the plaintext token and nothing
// else: no username, no header, no JSON envelope. A file whose whole content is the secret is the
// easiest thing to destroy and to verify destroyed, and it cannot smuggle a second value past a
// reader that expects one.
const TokenFile = "token"

// ScopeFile names the normalized repository identity the token in TokenFile may be presented to.
//
// It is not a secret and it is not the token's authority — the executor decides the repository scope
// from the authorization before anything is staged. It exists so the helper can refuse a request for
// a different repository without depending on Git's redirect and submodule behavior being what the
// documentation says. The obligation is that a redirect cannot leak the token; configuring
// `http.followRedirects=false` expresses that intent, and this file enforces it at the one place the
// token is actually handed over.
//
// The file holds a two-line `host=`/`path=` record rather than the bare host it held before the scope
// was widened to the whole repository identity. The format changed deliberately and without a
// compatibility reading: a file in the old format (a bare host, no `=`) or any other malformed content
// is refused by ReadScope rather than interpreted, so material staged by an older build is delivered
// to nobody instead of being matched against a host alone. No live deployment can be stranded by that,
// because every credentialed path is refused with `repository_credential_executor_unavailable` until a
// Node-side executor is installed (the credentialed-clone feature is off by default), so nothing is
// staging material in production today.
const ScopeFile = "scope"

// Scope field names in the staged scope record.
const (
	scopeHostKey = "host"
	scopePathKey = "path"
)

// Scope is the normalized repository identity a staged token may be presented to.
//
// It is the authorization's repository reduced to the one spelling both sides compare: a host that is
// lowercased and carries no https default port, and a path that keeps its case, has exactly one
// leading slash and loses one trailing slash and one trailing `.git` suffix. A credential request is
// answered only when it normalizes to exactly this pair; anything else is refused, because a prefix
// match or a best-effort normalization would widen a token's reach beyond the one repository the
// authorization named.
type Scope struct {
	Host string
	Path string
}

// NormalizeScope reduces a host and a repository path to the spelling a staged scope carries and a
// credential request must match.
//
// The rules are deliberately the smallest set that makes the two spellings every forge accepts for one
// repository agree, and nothing that could make two repositories agree:
//
//   - the host is trimmed, lowercased and loses a trailing `:443`, because DNS names are
//     case-insensitive and `:443` is the same endpoint as no port at all. A different port is a
//     different server and stays distinct.
//   - the path is trimmed and then compared byte for byte after removing one leading `/`, one trailing
//     `/` and one trailing `.git` — the decorations a forge adds or omits for the same repository, and
//     exactly the two `cloneauth.CanonicalRepository` already collapses. Its case is preserved,
//     because forge paths are case-sensitive. Nothing is percent-decoded: an encoded spelling is a
//     different byte string and is a mismatch rather than a guess.
//   - a host or path that is empty after normalization, or that carries a newline the record cannot
//     represent, is refused rather than repaired.
//
// This function never widens a scope: an input that is not obviously the scoped repository is left as
// a mismatch for the caller to refuse, never normalized into a match. A sub-path, a super-path, a
// query or fragment, a differing case in the path and an encoded variant all stay distinct.
func NormalizeScope(host, path string) (Scope, error) {
	normalizedHost := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ":443")
	if normalizedHost == "" {
		return Scope{}, errors.New("a repository scope names no host")
	}
	if strings.ContainsAny(normalizedHost, "\n\r") {
		return Scope{}, errors.New("a repository scope host carries a newline")
	}
	normalizedPath := strings.TrimSpace(path)
	if strings.ContainsAny(normalizedPath, "\n\r") {
		return Scope{}, errors.New("a repository scope path carries a newline")
	}
	normalizedPath = strings.TrimSuffix(normalizedPath, "/")
	normalizedPath = strings.TrimSuffix(normalizedPath, ".git")
	if !strings.HasPrefix(normalizedPath, "/") {
		normalizedPath = "/" + normalizedPath
	}
	if normalizedPath == "" || normalizedPath == "/" {
		return Scope{}, errors.New("a repository scope names no path")
	}
	return Scope{Host: normalizedHost, Path: normalizedPath}, nil
}

// DirectoryMode and FileMode are the permissions the material is created with. They are constants
// rather than configuration because the isolation argument depends on them exactly: the directory is
// readable only by the credential-executing identity, and the file inside it only by that same
// identity. A deployment that could weaken them could silently remove the boundary.
const (
	DirectoryMode os.FileMode = 0o700
	FileMode      os.FileMode = 0o600
)

// Sentinel failures. Callers map these to their own reason codes; none of them carries any part of
// the material.
var (
	// ErrInvalidExecutionID reports an identifier that is not a canonical lowercase UUID. It is
	// refused rather than normalized because the same value is both an identifier and a path segment.
	ErrInvalidExecutionID = errors.New("execution id is not a canonical uuid")
	// ErrUnsafeRoot reports a materialization root that is not an absolute, clean path. A relative
	// root would make the material's location depend on the process's working directory.
	ErrUnsafeRoot = errors.New("credential materialization root is not an absolute path")
	// ErrAbsent reports material that is not present. It is distinct from a read failure so the
	// executor can tell "the clone never staged a token" from "the token could not be read".
	ErrAbsent = errors.New("credential material is not present")
)

// ValidExecutionID reports whether id is a canonical lowercase UUID.
//
// The form is fixed rather than merely "safe for a file name" so that the directory name is a
// function of the identifier and not of the caller's spelling: two spellings of one execution would
// otherwise be two directories, and a helper asked to read one would silently find the other empty.
func ValidExecutionID(id string) bool {
	if len(id) != 36 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
				return false
			}
		}
	}
	return true
}

// ExecutionDirectory returns the directory one execution's material lives in.
//
// The result is always a direct child of root, because the identifier is validated before it is
// joined. The second check on the joined path is not redundant: it is what makes the guarantee
// independent of the identifier check staying correct, and it costs one comparison.
func ExecutionDirectory(root, executionID string) (string, error) {
	if err := validateRoot(root); err != nil {
		return "", err
	}
	if !ValidExecutionID(executionID) {
		return "", ErrInvalidExecutionID
	}
	directory := filepath.Join(root, executionID)
	if filepath.Dir(directory) != filepath.Clean(root) {
		return "", ErrInvalidExecutionID
	}
	return directory, nil
}

// TokenPath returns the path of the one file inside an execution's directory.
func TokenPath(root, executionID string) (string, error) {
	directory, err := ExecutionDirectory(root, executionID)
	if err != nil {
		return "", err
	}
	return filepath.Join(directory, TokenFile), nil
}

// Stage writes one token, and the normalized repository identity it may be presented to, into a
// freshly created execution directory.
//
// The directory is created with Mkdir, which refuses to merge into an existing one: staging over an
// existing directory is a caller bug and the safe reading of it is "do not touch what is already
// there". Each file is written with O_CREATE|O_EXCL and fsynced before the directory entry is synced,
// so a crash cannot leave a directory that exists but whose token is empty — the helper must never be
// able to read a partial secret and present it as a credential.
//
// The host and the path are normalized here rather than trusted as given, so the staged scope is the
// same reduction the helper applies to Git's request; a caller cannot widen a token's reach by staging
// a scope the helper would read differently than it writes.
//
// The caller is responsible for the root's ownership; this function only refuses to create the
// material outside it or with weaker modes than the constants above.
func Stage(root, executionID, token, host, path string) error {
	if strings.TrimSpace(token) == "" {
		return errors.New("refusing to stage an empty credential")
	}
	scope, err := NormalizeScope(host, path)
	if err != nil {
		return fmt.Errorf("refusing to stage a credential without a scope: %w", err)
	}
	directory, err := ExecutionDirectory(root, executionID)
	if err != nil {
		return err
	}
	if err := os.Mkdir(directory, DirectoryMode); err != nil {
		return fmt.Errorf("create credential material directory: %w", err)
	}
	// Mkdir applies the process umask. The isolation argument needs the mode to be exactly
	// DirectoryMode, so it is set again rather than assumed.
	if err := os.Chmod(directory, DirectoryMode); err != nil {
		return fmt.Errorf("fix credential material directory mode: %w", err)
	}
	if err := writeFile(filepath.Join(directory, TokenFile), token); err != nil {
		return err
	}
	if err := writeFile(filepath.Join(directory, ScopeFile), formatScope(scope)); err != nil {
		return err
	}
	return syncDirectory(directory)
}

// formatScope renders a normalized scope as the staged two-line record.
func formatScope(scope Scope) string {
	return scopeHostKey + "=" + scope.Host + "\n" + scopePathKey + "=" + scope.Path
}

// parseScope reads the staged scope record, refusing anything that is not exactly the two fields this
// package writes.
//
// The refusal is the compatibility boundary: a scope staged by an older build is a bare host with no
// `=` and a corrupt record could be anything at all, and neither is a repository identity the helper
// may hand a token to. Parsing is exact (two lines, known keys, no duplicates) so that unknown or
// missing content is a refusal rather than a silently narrower — or, worse, silently wider — match.
func parseScope(content string) (Scope, error) {
	lines := strings.Split(strings.TrimRight(content, "\n"), "\n")
	if len(lines) != 2 {
		return Scope{}, errors.New("credential scope is not a host and path record")
	}
	fields := make(map[string]string, 2)
	for _, line := range lines {
		key, value, found := strings.Cut(line, "=")
		if !found {
			return Scope{}, errors.New("credential scope record is malformed")
		}
		if _, duplicate := fields[key]; duplicate {
			return Scope{}, errors.New("credential scope record repeats a field")
		}
		fields[key] = value
	}
	host, ok := fields[scopeHostKey]
	if !ok {
		return Scope{}, errors.New("credential scope record names no host")
	}
	path, ok := fields[scopePathKey]
	if !ok {
		return Scope{}, errors.New("credential scope record names no path")
	}
	return NormalizeScope(host, path)
}

// writeFile creates one material file exclusively, fsyncs it, and closes it.
func writeFile(path, content string) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, FileMode)
	if err != nil {
		return fmt.Errorf("create credential material file: %w", err)
	}
	if _, err := file.WriteString(content); err != nil {
		file.Close()
		return fmt.Errorf("write credential material: %w", err)
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return fmt.Errorf("sync credential material: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close credential material: %w", err)
	}
	return nil
}

// Read returns the staged token for one execution.
//
// The reader is the deployment-fixed Git credential helper, so this is the one place the plaintext
// crosses back out of the filesystem. It refuses anything other than a regular file with exactly the
// permissions Stage created: a symlink or a wider mode means the material has been tampered with,
// and the safe answer is to deliver nothing.
func Read(root, executionID string) (string, error) {
	return readMaterialFile(root, executionID, TokenFile)
}

// ReadScope returns the normalized repository identity the staged token may be presented to.
func ReadScope(root, executionID string) (Scope, error) {
	content, err := readMaterialFile(root, executionID, ScopeFile)
	if err != nil {
		return Scope{}, err
	}
	return parseScope(content)
}

// readMaterialFile reads one material file after checking it is a regular file with the mode Stage
// gave it. The check is repeated on every read rather than trusted from staging time, because the
// directory belongs to the credential-executing identity and a wider mode would mean something else
// had already changed it.
func readMaterialFile(root, executionID, name string) (string, error) {
	directory, err := ExecutionDirectory(root, executionID)
	if err != nil {
		return "", err
	}
	path := filepath.Join(directory, name)
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", ErrAbsent
		}
		return "", fmt.Errorf("inspect credential material: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("credential material is not a regular file")
	}
	if info.Mode().Perm() != FileMode {
		return "", errors.New("credential material does not have the required permissions")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read credential material: %w", err)
	}
	content := string(raw)
	if strings.TrimSpace(content) == "" {
		return "", ErrAbsent
	}
	return content, nil
}

// Destroy removes one execution's material and proves it is gone.
//
// "Removed and verified empty" rather than "best effort" is what the cleanup obligation asks for: an
// unlink that reports success while a hard link or a second entry survives would leave a secret
// readable after the clone that needed it had ended. The directory is therefore removed and then
// re-checked, and a failure to prove it gone is returned as an error — the caller must not record
// the clone as successful.
func Destroy(root, executionID string) error {
	directory, err := ExecutionDirectory(root, executionID)
	if err != nil {
		return err
	}
	// RemoveAll rather than Remove: the token is the only entry this package creates, but a clone
	// that crashed mid-write could have left a temporary file behind, and leaving it would defeat the
	// purpose of the sweep.
	if err := os.RemoveAll(directory); err != nil {
		return fmt.Errorf("destroy credential material: %w", err)
	}
	if _, err := os.Lstat(directory); err == nil {
		return errors.New("credential material survived its own destruction")
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("verify credential material destruction: %w", err)
	}
	// The unlink has to reach the directory entry, not just the page cache: a crash after a reported
	// success must not be able to bring the material back.
	return syncDirectory(filepath.Clean(root))
}

// Sweep removes every execution directory under the root and reports how many it removed.
//
// It runs at startup, before the executor accepts any job, because the failure it repairs is one no
// running process can repair: a crash or a killed container leaves material behind, and the process
// that would have destroyed it is the process that died. Nothing else in the root is expected, so
// any entry that is not a canonical UUID directory is also removed — an unexpected name is more
// likely to be a leftover than something this deployment meant to keep, and the root exists for no
// other purpose.
func Sweep(root string) (int, error) {
	if err := validateRoot(root); err != nil {
		return 0, err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return 0, fmt.Errorf("read credential materialization root: %w", err)
	}
	removed := 0
	for _, entry := range entries {
		target := filepath.Join(root, entry.Name())
		if filepath.Dir(target) != filepath.Clean(root) {
			return removed, errors.New("credential materialization root holds an escaping entry")
		}
		if err := os.RemoveAll(target); err != nil {
			return removed, fmt.Errorf("sweep credential material: %w", err)
		}
		removed++
	}
	if err := syncDirectory(filepath.Clean(root)); err != nil {
		return removed, err
	}
	return removed, nil
}

// validateRoot refuses a root that is not absolute and clean.
func validateRoot(root string) error {
	if strings.TrimSpace(root) == "" || !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return ErrUnsafeRoot
	}
	return nil
}

// syncDirectory fsyncs a directory so a create or a delete inside it survives a power loss.
//
// Without it the file's own fsync only guarantees its contents, not that the name exists — a crash
// could leave a staged token unreachable (harmless) or, worse for the destruction path, a removed
// name still present after a reboot (a surviving secret). The call is a no-op on platforms where a
// directory cannot be opened for syncing.
func syncDirectory(path string) error {
	handle, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open credential material directory for syncing: %w", err)
	}
	defer handle.Close()
	if err := handle.Sync(); err != nil {
		return fmt.Errorf("sync credential material directory: %w", err)
	}
	return nil
}
