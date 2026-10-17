package credentialmaterial

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	testRepository = "https://git.example.com/ora/private-repo"
	testUsername   = "x-access-token"
	testPassword   = "ghp_generated-test-token"
)

func testScope(t *testing.T) Scope {
	t.Helper()
	scope, err := NewScope(testRepository)
	if err != nil {
		t.Fatalf("NewScope: %v", err)
	}
	return scope
}

func stage(t *testing.T) (string, *Material) {
	t.Helper()
	root := t.TempDir()
	material, err := Stage(root, testScope(t), testUsername, testPassword)
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	t.Cleanup(func() { _ = material.Destroy() })
	return root, material
}

func TestNewScope(t *testing.T) {
	scope, err := NewScope("https://Git.Example.com:443/ora/private-repo.git/")
	if err != nil {
		t.Fatalf("NewScope: %v", err)
	}
	if want := (Scope{Host: "git.example.com", Path: "/ora/private-repo"}); scope != want {
		t.Fatalf("NewScope = %+v, want %+v", scope, want)
	}
	if scope.Repository() != testRepository {
		t.Fatalf("Repository() = %q", scope.Repository())
	}
	withPort, err := NewScope("https://git.example.com:8443/a/b")
	if err != nil || withPort.Host != "git.example.com:8443" || withPort.Path != "/a/b" {
		t.Fatalf("NewScope with port = %+v, %v", withPort, err)
	}
	if _, err := NewScope("http://git.example.com/a"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("NewScope(http) = %v, want ErrInvalid", err)
	}
}

func TestStageLayoutAndModes(t *testing.T) {
	root, material := stage(t)
	dir := material.Dir()
	if filepath.Dir(dir) != root {
		t.Fatalf("material directory %q is not a direct child of the root %q", dir, root)
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode().Perm() != DirectoryMode {
		t.Fatalf("directory mode = %v, %v; want a 0700 directory", info.Mode(), err)
	}
	for name, want := range map[string]string{ScopeFile: testRepository, UsernameFile: testUsername, PasswordFile: testPassword} {
		path := filepath.Join(dir, name)
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != FileMode {
			t.Fatalf("%s mode = %v, %v; want a 0600 regular file", name, info.Mode(), err)
		}
		content, _ := os.ReadFile(path)
		if string(content) != want {
			t.Fatalf("%s holds %q, want %q", name, content, want)
		}
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 3 {
		t.Fatalf("material directory holds %d entries, want exactly 3", len(entries))
	}
	scope, err := ReadScope(dir)
	if err != nil || scope != testScope(t) {
		t.Fatalf("ReadScope = %+v, %v", scope, err)
	}
	username, password, err := ReadSecret(dir)
	if err != nil || username != testUsername || password != testPassword {
		t.Fatalf("ReadSecret = %q, %q, %v", username, password, err)
	}
}

func TestStageIgnoresTheUmaskAndUsesDistinctDirectories(t *testing.T) {
	root := t.TempDir()
	first, err := Stage(root, testScope(t), testUsername, testPassword)
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	t.Cleanup(func() { _ = first.Destroy() })
	second, err := Stage(root, testScope(t), testUsername, testPassword)
	if err != nil {
		t.Fatalf("second Stage: %v", err)
	}
	t.Cleanup(func() { _ = second.Destroy() })
	if first.Dir() == second.Dir() {
		t.Fatal("two stagings share one directory")
	}
}

func TestStageRefuses(t *testing.T) {
	scope := testScope(t)
	cases := []struct {
		name     string
		root     string
		scope    Scope
		username string
		password string
		want     error
	}{
		{"relative root", "material", scope, testUsername, testPassword, ErrUnsafePath},
		{"unclean root", "/tmp/../tmp", scope, testUsername, testPassword, ErrUnsafePath},
		{"empty root", "-", scope, testUsername, testPassword, ErrUnsafePath},
		{"empty scope", "", Scope{}, testUsername, testPassword, ErrInvalid},
		{"unnormalized host", "", Scope{Host: "Git.Example.com", Path: "/ora/private-repo"}, testUsername, testPassword, ErrInvalid},
		{"default port kept", "", Scope{Host: "git.example.com:443", Path: "/ora/private-repo"}, testUsername, testPassword, ErrInvalid},
		{"git suffix kept", "", Scope{Host: "git.example.com", Path: "/ora/private-repo.git"}, testUsername, testPassword, ErrInvalid},
		{"relative path", "", Scope{Host: "git.example.com", Path: "ora/private-repo"}, testUsername, testPassword, ErrInvalid},
		{"host carrying a path", "", Scope{Host: "git.example.com/ora", Path: "/private-repo"}, testUsername, testPassword, ErrInvalid},
		{"empty username", "", scope, "", testPassword, ErrInvalid},
		{"empty password", "", scope, testUsername, "", ErrInvalid},
		{"newline in password", "", scope, testUsername, "a\nusername=evil", ErrInvalid},
		{"carriage return in username", "", scope, "a\rb", testPassword, ErrInvalid},
		{"NUL in password", "", scope, testUsername, "a\x00b", ErrInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// "" means a fresh temporary root and "-" the literal empty string.
			root := tc.root
			switch root {
			case "":
				root = t.TempDir()
			case "-":
				root = ""
			}
			material, err := Stage(root, tc.scope, tc.username, tc.password)
			if !errors.Is(err, tc.want) {
				if material != nil {
					_ = material.Destroy()
				}
				t.Fatalf("Stage = %v, want %v", err, tc.want)
			}
			if filepath.IsAbs(root) && filepath.Clean(root) == root {
				if entries, _ := os.ReadDir(root); len(entries) != 0 {
					t.Fatalf("a refused Stage left %d entries under the root", len(entries))
				}
			}
		})
	}
	if _, err := Stage(filepath.Join(t.TempDir(), "absent"), scope, testUsername, testPassword); err == nil {
		t.Fatal("Stage under a missing root succeeded")
	}
}

func TestDestroyRemovesAndIsIdempotent(t *testing.T) {
	root, material := stage(t)
	if err := material.Destroy(); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if _, err := os.Lstat(material.Dir()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("material directory still present after Destroy: %v", err)
	}
	if entries, _ := os.ReadDir(root); len(entries) != 0 {
		t.Fatalf("root holds %d entries after Destroy", len(entries))
	}
	if err := material.Destroy(); err != nil {
		t.Fatalf("a second Destroy failed: %v", err)
	}
	if _, _, err := ReadSecret(material.Dir()); !errors.Is(err, ErrAbsent) {
		t.Fatalf("ReadSecret after Destroy = %v, want ErrAbsent", err)
	}
}

// TestDestroyReportsMaterialItCannotRemove makes the material directory read-only so its files cannot
// be unlinked, and checks that the failure is reported instead of claimed as success.
func TestDestroyReportsMaterialItCannotRemove(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions, so the removal cannot be made to fail")
	}
	_, material := stage(t)
	if err := os.Chmod(material.Dir(), 0o500); err != nil {
		t.Fatalf("make the material directory read-only: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(material.Dir(), DirectoryMode) })
	if err := material.Destroy(); err == nil {
		t.Fatal("Destroy reported success while the material is still on disk")
	}
	if _, err := os.Lstat(filepath.Join(material.Dir(), PasswordFile)); err != nil {
		t.Fatalf("the test did not keep the password on disk: %v", err)
	}
}

func TestSweep(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 2; i++ {
		if _, err := Stage(root, testScope(t), testUsername, testPassword); err != nil {
			t.Fatalf("Stage: %v", err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "leftover.tmp"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write leftover: %v", err)
	}
	if err := os.Symlink("/etc", filepath.Join(root, "link")); err != nil {
		t.Fatalf("create symlink: %v", err)
	}
	removed, err := Sweep(root)
	if err != nil || removed != 4 {
		t.Fatalf("Sweep = %d, %v; want 4, nil", removed, err)
	}
	if entries, _ := os.ReadDir(root); len(entries) != 0 {
		t.Fatalf("root holds %d entries after Sweep", len(entries))
	}
	if _, err := os.Stat("/etc"); err != nil {
		t.Fatalf("Sweep followed a symlink out of the root: %v", err)
	}
	if removed, err := Sweep(root); err != nil || removed != 0 {
		t.Fatalf("Sweep of an empty root = %d, %v", removed, err)
	}
	if _, err := Sweep("relative"); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("Sweep(relative) = %v, want ErrUnsafePath", err)
	}
}

func TestReadRefusesTamperedMaterial(t *testing.T) {
	cases := map[string]func(t *testing.T, dir string){
		"widened password mode": func(t *testing.T, dir string) {
			mustChmod(t, filepath.Join(dir, PasswordFile), 0o644)
		},
		"widened directory mode": func(t *testing.T, dir string) { mustChmod(t, dir, 0o755) },
		"password replaced by a symlink": func(t *testing.T, dir string) {
			target := filepath.Join(t.TempDir(), "elsewhere")
			if err := os.WriteFile(target, []byte("other"), 0o600); err != nil {
				t.Fatal(err)
			}
			mustRemove(t, filepath.Join(dir, PasswordFile))
			if err := os.Symlink(target, filepath.Join(dir, PasswordFile)); err != nil {
				t.Fatal(err)
			}
		},
		"password replaced by a directory": func(t *testing.T, dir string) {
			mustRemove(t, filepath.Join(dir, PasswordFile))
			if err := os.Mkdir(filepath.Join(dir, PasswordFile), 0o600); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, tamper := range cases {
		t.Run(name, func(t *testing.T) {
			_, material := stage(t)
			tamper(t, material.Dir())
			t.Cleanup(func() { _ = os.Chmod(material.Dir(), DirectoryMode) })
			if _, _, err := ReadSecret(material.Dir()); !errors.Is(err, ErrTampered) {
				t.Fatalf("ReadSecret = %v, want ErrTampered", err)
			}
		})
	}
}

func TestReadScopeRefusesDamagedScope(t *testing.T) {
	for name, content := range map[string]string{
		"bare host":        "git.example.com",
		"unnormalized":     "https://git.example.com/ora/private-repo.git",
		"trailing newline": testRepository + "\n",
		"two lines":        testRepository + "\nhttps://other.example/x",
		"empty":            "",
	} {
		t.Run(name, func(t *testing.T) {
			_, material := stage(t)
			path := filepath.Join(material.Dir(), ScopeFile)
			if err := os.WriteFile(path, []byte(content), FileMode); err != nil {
				t.Fatal(err)
			}
			if _, err := ReadScope(material.Dir()); err == nil {
				t.Fatalf("ReadScope accepted %q", content)
			}
		})
	}
	if _, err := ReadScope("relative/dir"); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("ReadScope(relative) = %v, want ErrUnsafePath", err)
	}
	if _, err := ReadScope(filepath.Join(t.TempDir(), "absent")); !errors.Is(err, ErrAbsent) {
		t.Fatalf("ReadScope(absent) = %v, want ErrAbsent", err)
	}
}

func TestErrorsDoNotCarryTheSecret(t *testing.T) {
	_, err := Stage(t.TempDir(), testScope(t), testUsername, testPassword+"\n")
	if err == nil || strings.Contains(err.Error(), testPassword) {
		t.Fatalf("Stage error = %v; want a refusal that does not echo the secret", err)
	}
}

func mustChmod(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func mustRemove(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
}
