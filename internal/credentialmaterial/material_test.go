package credentialmaterial

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testExecutionID is a canonical lowercase UUID. The package refuses any other spelling, so the
// tests use one real identifier rather than a readable placeholder.
const testExecutionID = "6f1d0a52-1c3b-4c7e-9a10-2b8f4d6e0c31"

func newRoot(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "material")
	if err := os.Mkdir(root, DirectoryMode); err != nil {
		t.Fatalf("create materialization root: %v", err)
	}
	return root
}

func TestValidExecutionIDAcceptsOnlyCanonicalLowercaseUUIDs(t *testing.T) {
	cases := []struct {
		name string
		id   string
		want bool
	}{
		{"canonical", "6f1d0a52-1c3b-4c7e-9a10-2b8f4d6e0c31", true},
		{"uppercase is a different spelling", "6F1D0A52-1C3B-4C7E-9A10-2B8F4D6E0C31", false},
		{"braced", "{6f1d0a52-1c3b-4c7e-9a10-2b8f4d6e0c31}", false},
		{"no dashes", "6f1d0a521c3b4c7e9a102b8f4d6e0c31", false},
		{"traversal", "../../etc/passwd", false},
		{"separator", "6f1d0a52-1c3b-4c7e-9a10-2b8f4d6e0c3/", false},
		{"empty", "", false},
		{"non-hex", "zf1d0a52-1c3b-4c7e-9a10-2b8f4d6e0c31", false},
		{"short", "6f1d0a52-1c3b-4c7e-9a10-2b8f4d6e0c3", false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := ValidExecutionID(testCase.id); got != testCase.want {
				t.Fatalf("ValidExecutionID(%q) = %v, want %v", testCase.id, got, testCase.want)
			}
		})
	}
}

func TestExecutionDirectoryStaysDirectlyUnderTheRoot(t *testing.T) {
	root := newRoot(t)
	directory, err := ExecutionDirectory(root, testExecutionID)
	if err != nil {
		t.Fatalf("ExecutionDirectory: %v", err)
	}
	if filepath.Dir(directory) != root {
		t.Fatalf("execution directory %q is not a direct child of %q", directory, root)
	}
	for _, id := range []string{"../escape", "a/b", "/absolute", ""} {
		if _, err := ExecutionDirectory(root, id); !errors.Is(err, ErrInvalidExecutionID) {
			t.Fatalf("ExecutionDirectory(%q) error = %v, want ErrInvalidExecutionID", id, err)
		}
	}
	for _, bad := range []string{"relative/path", "/trailing/../trailing", " "} {
		if _, err := ExecutionDirectory(bad, testExecutionID); !errors.Is(err, ErrUnsafeRoot) {
			t.Fatalf("ExecutionDirectory(root=%q) error = %v, want ErrUnsafeRoot", bad, err)
		}
	}
}

func TestStageCreatesExactlyTheDocumentedLayoutWithTheDocumentedModes(t *testing.T) {
	root := newRoot(t)
	// The host is given in mixed case with the https default port and the path with a leading slash, a
	// `.git` suffix, a trailing slash and mixed case: staging must reduce only the decorations, never
	// the path's case, which is what makes the helper's later comparison exact.
	if err := Stage(root, testExecutionID, "ghp_example_token_value", "GitHub.com:443", "/Ora/Private-Repo.git/"); err != nil {
		t.Fatalf("Stage: %v", err)
	}
	directory := filepath.Join(root, testExecutionID)
	info, err := os.Stat(directory)
	if err != nil {
		t.Fatalf("stat execution directory: %v", err)
	}
	if info.Mode().Perm() != DirectoryMode {
		t.Fatalf("execution directory mode = %o, want %o", info.Mode().Perm(), DirectoryMode)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatalf("read execution directory: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("execution directory holds %d entries, want exactly token and scope", len(entries))
	}
	fileInfo, err := os.Stat(filepath.Join(directory, TokenFile))
	if err != nil {
		t.Fatalf("stat token: %v", err)
	}
	if fileInfo.Mode().Perm() != FileMode {
		t.Fatalf("token mode = %o, want %o", fileInfo.Mode().Perm(), FileMode)
	}
	token, err := Read(root, testExecutionID)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if token != "ghp_example_token_value" {
		t.Fatalf("Read returned %q, want the staged token", token)
	}
	// The scope is the normalized repository identity, so the host is lowercased and loses the default
	// port while the path keeps its case and loses the trailing slash and one `.git`.
	scope, err := ReadScope(root, testExecutionID)
	if err != nil {
		t.Fatalf("ReadScope: %v", err)
	}
	if want := (Scope{Host: "github.com", Path: "/Ora/Private-Repo"}); scope != want {
		t.Fatalf("ReadScope = %+v, want %+v", scope, want)
	}
}

func TestStageRefusesEmptyMaterialAndAnExistingDirectory(t *testing.T) {
	root := newRoot(t)
	if err := Stage(root, testExecutionID, "   ", "github.com", "/ora/private-repo"); err == nil {
		t.Fatal("Stage accepted a blank token")
	}
	if err := Stage(root, testExecutionID, "token", "  ", "/ora/private-repo"); err == nil {
		t.Fatal("Stage accepted a blank scope")
	}
	if err := Stage(root, testExecutionID, "token", "github.com", "/ora/private-repo"); err != nil {
		t.Fatalf("Stage: %v", err)
	}
	// Staging twice must not merge into the directory that already holds a live token: the second
	// caller would otherwise be able to leave its own value behind after the first clone's cleanup.
	if err := Stage(root, testExecutionID, "second-token", "github.com", "/ora/private-repo"); err == nil {
		t.Fatal("Stage merged into an existing execution directory")
	}
	token, err := Read(root, testExecutionID)
	if err != nil {
		t.Fatalf("Read after refused restage: %v", err)
	}
	if token != "token" {
		t.Fatalf("refused restage changed the material to %q", token)
	}
}

func TestReadRefusesAbsentWidenedAndNonRegularMaterial(t *testing.T) {
	root := newRoot(t)
	if _, err := Read(root, testExecutionID); !errors.Is(err, ErrAbsent) {
		t.Fatalf("Read of absent material error = %v, want ErrAbsent", err)
	}
	if err := Stage(root, testExecutionID, "token", "github.com", "/ora/private-repo"); err != nil {
		t.Fatalf("Stage: %v", err)
	}
	path := filepath.Join(root, testExecutionID, TokenFile)
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("widen token mode: %v", err)
	}
	if _, err := Read(root, testExecutionID); err == nil {
		t.Fatal("Read accepted material with wider permissions than FileMode")
	}
	// A symlink is refused even when it points at a file that would otherwise be readable: the
	// material must be the file this package created, not something it was redirected to.
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove token: %v", err)
	}
	target := filepath.Join(root, "elsewhere")
	if err := os.WriteFile(target, []byte("elsewhere"), FileMode); err != nil {
		t.Fatalf("write symlink target: %v", err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatalf("symlink token: %v", err)
	}
	if _, err := Read(root, testExecutionID); err == nil {
		t.Fatal("Read followed a symlinked material file")
	}
}

func TestReadTreatsBlankMaterialAsAbsent(t *testing.T) {
	root := newRoot(t)
	if err := Stage(root, testExecutionID, "token", "github.com", "/ora/private-repo"); err != nil {
		t.Fatalf("Stage: %v", err)
	}
	// An empty file is what a crash between create and write would leave. Reading it as "present but
	// empty" would hand Git an empty password; reading it as absent is the honest answer.
	if err := os.WriteFile(filepath.Join(root, testExecutionID, TokenFile), nil, FileMode); err != nil {
		t.Fatalf("truncate token: %v", err)
	}
	if _, err := Read(root, testExecutionID); !errors.Is(err, ErrAbsent) {
		t.Fatalf("Read of blank material error = %v, want ErrAbsent", err)
	}
}

func TestDestroyRemovesTheMaterialAndProvesItGone(t *testing.T) {
	root := newRoot(t)
	if err := Stage(root, testExecutionID, "token", "github.com", "/ora/private-repo"); err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if err := Destroy(root, testExecutionID); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, testExecutionID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("execution directory survived Destroy: %v", err)
	}
	if _, err := Read(root, testExecutionID); !errors.Is(err, ErrAbsent) {
		t.Fatalf("Read after Destroy error = %v, want ErrAbsent", err)
	}
	// Destroying what is not there is not an error: cleanup runs on paths where the clone may have
	// failed before staging, and a second cleanup must be as safe as the first.
	if err := Destroy(root, testExecutionID); err != nil {
		t.Fatalf("Destroy of absent material: %v", err)
	}
}

func TestDestroyLeavesNoTokenBytesAnywhereUnderTheRoot(t *testing.T) {
	root := newRoot(t)
	const secret = "ghp_do_not_survive_cleanup"
	if err := Stage(root, testExecutionID, secret, "github.com", "/ora/private-repo"); err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if err := Destroy(root, testExecutionID); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	// The cleanup obligation is about the bytes, not the directory entry, so the whole tree is
	// walked rather than only the path this package knows about.
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(content), secret) {
			t.Fatalf("token bytes survived cleanup in %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk materialization root: %v", err)
	}
}

func TestSweepRemovesEveryEntryUnderTheRoot(t *testing.T) {
	root := newRoot(t)
	if err := Stage(root, testExecutionID, "token", "github.com", "/ora/private-repo"); err != nil {
		t.Fatalf("Stage: %v", err)
	}
	// A leftover that is not an execution directory is exactly what a crashed older version or a
	// stray write would leave, and the root exists for nothing else.
	if err := os.WriteFile(filepath.Join(root, "stale"), []byte("stale"), FileMode); err != nil {
		t.Fatalf("write stale entry: %v", err)
	}
	removed, err := Sweep(root)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if removed != 2 {
		t.Fatalf("Sweep removed %d entries, want 2", removed)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read root after Sweep: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("root holds %d entries after Sweep", len(entries))
	}
	if _, err := Read(root, testExecutionID); !errors.Is(err, ErrAbsent) {
		t.Fatalf("Read after Sweep error = %v, want ErrAbsent", err)
	}
}

// TestNormalizeScopeReducesOnlyTheDecorations pins the whole normalization rule in one table: what is
// removed (the https default port, a leading slash, a trailing slash, one `.git`), what is preserved
// (a non-default port, the path's case), and what is never guessed at (percent-encoding, a query or
// fragment, a path that is a sub- or super-path of another). A refusal is part of the rule because a
// host or path with no identity at all must not be normalized into one.
func TestNormalizeScopeReducesOnlyTheDecorations(t *testing.T) {
	cases := []struct {
		name       string
		host       string
		path       string
		want       Scope
		wantRefuse bool
	}{
		{"host lowercased and default port dropped", "GitHub.com:443", "/Ora/Repo", Scope{"github.com", "/Ora/Repo"}, false},
		{"a non-default port is a different server and stays", "git.example.com:8443", "/ora/repo", Scope{"git.example.com:8443", "/ora/repo"}, false},
		{"a missing leading slash is added so both spellings agree", "git.example.com", "ora/repo", Scope{"git.example.com", "/ora/repo"}, false},
		{"one trailing slash is removed", "git.example.com", "/ora/repo/", Scope{"git.example.com", "/ora/repo"}, false},
		{"one .git suffix is removed", "git.example.com", "/ora/repo.git", Scope{"git.example.com", "/ora/repo"}, false},
		{"path case is preserved, because forge paths are case-sensitive", "git.example.com", "/Ora/Repo", Scope{"git.example.com", "/Ora/Repo"}, false},
		{"a sub-path is not collapsed into its parent", "git.example.com", "/ora/repo/sub", Scope{"git.example.com", "/ora/repo/sub"}, false},
		{"a super-path is not widened into its child", "git.example.com", "/ora", Scope{"git.example.com", "/ora"}, false},
		{"only one .git is stripped, so repo.git.git stays distinct from repo", "git.example.com", "/ora/repo.git.git", Scope{"git.example.com", "/ora/repo.git"}, false},
		{"percent-encoding is not decoded", "git.example.com", "/ora/repo%2Dname", Scope{"git.example.com", "/ora/repo%2Dname"}, false},
		{"a query is not a decoration", "git.example.com", "/ora/repo?ref=x", Scope{"git.example.com", "/ora/repo?ref=x"}, false},
		{"a fragment is not a decoration", "git.example.com", "/ora/repo#frag", Scope{"git.example.com", "/ora/repo#frag"}, false},
		{"an empty host is refused", "", "/ora/repo", Scope{}, true},
		{"a whitespace-only host is refused", "   ", "/ora/repo", Scope{}, true},
		{"an empty path is refused", "git.example.com", "", Scope{}, true},
		{"a slash-only path is refused", "git.example.com", "/", Scope{}, true},
		{"a path that is only a .git suffix is refused", "git.example.com", ".git", Scope{}, true},
		{"a newline inside the host is refused", "git.example\n.com", "/ora/repo", Scope{}, true},
		{"a newline inside the path is refused", "git.example.com", "/ora\n/repo", Scope{}, true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got, err := NormalizeScope(testCase.host, testCase.path)
			if testCase.wantRefuse {
				if err == nil {
					t.Fatalf("NormalizeScope(%q, %q) = %+v, want a refusal", testCase.host, testCase.path, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeScope(%q, %q) was refused: %v", testCase.host, testCase.path, err)
			}
			if got != testCase.want {
				t.Fatalf("NormalizeScope(%q, %q) = %+v, want %+v", testCase.host, testCase.path, got, testCase.want)
			}
		})
	}
}

// TestReadScopeRefusesOldFormatAndCorruptRecords is the compatibility boundary made executable: the
// staged scope changed from a bare host to a two-line record, and every pre-change or damaged file
// must be refused rather than read as a scope. A refusal here is what keeps material staged by an
// older build from being matched against a host alone.
func TestReadScopeRefusesOldFormatAndCorruptRecords(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{"the old bare-host format", "git.example.com"},
		{"the old bare-host format with a newline", "git.example.com\n"},
		{"a host field without a path", "host=git.example.com"},
		{"a path field without a host", "path=/ora/repo"},
		{"a repeated host field", "host=git.example.com\nhost=gitlab.example.com"},
		{"an unknown field", "host=git.example.com\npath=/ora/repo\nowner=someone"},
		{"a line without an equals sign", "host=git.example.com\n/ora/repo"},
		{"a host that normalizes to nothing", "host=\npath=/ora/repo"},
		{"a path that normalizes to nothing", "host=git.example.com\npath=/"},
		{"an empty record", ""},
		{"a blank record", "   "},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			root := newRoot(t)
			if err := Stage(root, testExecutionID, "token", "git.example.com", "/ora/repo"); err != nil {
				t.Fatalf("Stage: %v", err)
			}
			scopePath := filepath.Join(root, testExecutionID, ScopeFile)
			if err := os.WriteFile(scopePath, []byte(testCase.content), FileMode); err != nil {
				t.Fatalf("overwrite the scope record: %v", err)
			}
			if scope, err := ReadScope(root, testExecutionID); err == nil {
				t.Fatalf("ReadScope accepted a scope it must refuse: %+v (content %q)", scope, testCase.content)
			}
		})
	}
}

// TestStageRefusesAScopeItCannotNormalize proves the writer side refuses rather than persisting an
// empty or unnormalizable identity, so a scope the helper could read can never be a wildcard.
func TestStageRefusesAScopeItCannotNormalize(t *testing.T) {
	root := newRoot(t)
	if err := Stage(root, testExecutionID, "token", "git.example.com", ""); err == nil {
		t.Fatal("Stage accepted a credential with no repository path")
	}
	if err := Stage(root, testExecutionID, "token", "", "/ora/repo"); err == nil {
		t.Fatal("Stage accepted a credential with no host")
	}
}
