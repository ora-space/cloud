package cloneauth

import (
	"errors"
	"testing"
)

func TestNormalizeRepositoryAccepts(t *testing.T) {
	cases := []struct{ raw, want string }{
		{"https://github.com/a/b", "https://github.com/a/b"},
		{"https://GitHub.com:443/a/b.git/", "https://github.com/a/b"},
		{"https://github.com/a/b.git", "https://github.com/a/b"},
		{"https://github.com/a/b/", "https://github.com/a/b"},
		{"HTTPS://github.com/a/b", "https://github.com/a/b"},
		{"  https://github.com/a/b  ", "https://github.com/a/b"},
		{"https://github.com/Org/Repo", "https://github.com/Org/Repo"},
		{"https://git.example.com:8443/group/sub/repo.git", "https://git.example.com:8443/group/sub/repo"},
		{"https://gitlab.example.com/group/sub-group/repo_1.x~y", "https://gitlab.example.com/group/sub-group/repo_1.x~y"},
		{"https://[2001:DB8::1]/a/b", "https://[2001:db8::1]/a/b"},
		{"https://[2001:db8::1]:443/a/b", "https://[2001:db8::1]/a/b"},
		{"https://10.0.0.1/a", "https://10.0.0.1/a"},
		{"https://h/a/.git/b", "https://h/a/.git/b"},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			got, err := NormalizeRepository(tc.raw)
			if err != nil {
				t.Fatalf("NormalizeRepository(%q) failed: %v", tc.raw, err)
			}
			if got != tc.want {
				t.Fatalf("NormalizeRepository(%q) = %q, want %q", tc.raw, got, tc.want)
			}
			again, err := NormalizeRepository(got)
			if err != nil || again != got {
				t.Fatalf("NormalizeRepository is not idempotent on %q: %q, %v", got, again, err)
			}
		})
	}
}

func TestNormalizeRepositoryEqualities(t *testing.T) {
	groups := [][]string{
		{"https://GitHub.com:443/a/b.git/", "https://github.com/a/b", "https://github.com/a/b.git", "https://GITHUB.COM/a/b/"},
		{"https://h:8443/x/y", "https://H:8443/x/y.git"},
	}
	for _, group := range groups {
		want, err := NormalizeRepository(group[0])
		if err != nil {
			t.Fatalf("NormalizeRepository(%q): %v", group[0], err)
		}
		for _, raw := range group[1:] {
			if got, err := NormalizeRepository(raw); err != nil || got != want {
				t.Fatalf("NormalizeRepository(%q) = %q, %v; want %q", raw, got, err, want)
			}
		}
	}
	distinct := [][2]string{
		{"https://github.com/a/b", "https://github.com/A/B"},
		{"https://github.com/a/b", "https://github.com:8443/a/b"},
		{"https://github.com/a/b", "https://github.com/a/b/c"},
		{"https://github.com/a/b", "https://github.com/a"},
	}
	for _, pair := range distinct {
		left, _ := NormalizeRepository(pair[0])
		right, _ := NormalizeRepository(pair[1])
		if left == right {
			t.Fatalf("%q and %q normalized to the same %q", pair[0], pair[1], left)
		}
	}
}

func TestNormalizeRepositoryRejects(t *testing.T) {
	cases := []struct{ name, raw string }{
		{"empty", ""},
		{"http", "http://github.com/a/b"},
		{"ssh", "ssh://git@github.com/a/b"},
		{"scp-like", "git@github.com:a/b.git"},
		{"file", "file:///srv/a/b"},
		{"opaque", "https:github.com/a/b"},
		{"userinfo", "https://user@github.com/a/b"},
		{"userinfo with password", "https://user:token@github.com/a/b"},
		{"empty userinfo", "https://@github.com/a/b"},
		{"query", "https://github.com/a/b?x=1"},
		{"empty query", "https://github.com/a/b?"},
		{"fragment", "https://github.com/a/b#main"},
		{"empty fragment", "https://github.com/a/b#"},
		{"no host", "https:///a/b"},
		{"no path", "https://github.com"},
		{"root path", "https://github.com/"},
		{"only .git", "https://github.com/.git"},
		{"dot-dot segment", "https://github.com/a/../b"},
		{"dot segment", "https://github.com/a/./b"},
		{"double slash", "https://github.com/a//b"},
		{"two trailing slashes", "https://github.com/a/b//"},
		{"trailing .git segment", "https://github.com/a/.git"},
		{"double .git suffix", "https://github.com/a/b.git.git"},
		{"percent-encoded slash", "https://github.com/a%2Fb"},
		{"percent-encoded slash lowercase", "https://github.com/a%2fb"},
		{"percent-encoded dot", "https://github.com/a/%2E%2E/b"},
		{"any percent escape", "https://github.com/a/%41"},
		{"percent in host", "https://git%68ub.com/a/b"},
		{"backslash", "https://github.com/a\\b"},
		{"space", "https://github.com/a b"},
		{"newline", "https://github.com/a\nb"},
		{"non-ascii path", "https://github.com/a/ü"},
		{"non-ascii host", "https://gïthub.com/a/b"},
		{"empty port", "https://github.com:/a/b"},
		{"zero port", "https://github.com:0/a/b"},
		{"port with leading zero", "https://github.com:0443/a/b"},
		{"port out of range", "https://github.com:65536/a/b"},
		{"non-numeric port", "https://github.com:https/a/b"},
		{"trailing dot host", "https://github.com./a/b"},
		{"empty label", "https://github..com/a/b"},
		{"label with leading hyphen", "https://-github.com/a/b"},
		{"underscore host", "https://git_hub.com/a/b"},
		{"unclosed ipv6", "https://[2001:db8::1/a/b"},
		{"ipv4 in brackets", "https://[10.0.0.1]/a/b"},
		{"quote in path", "https://github.com/a/\"b"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeRepository(tc.raw)
			if !errors.Is(err, ErrRepository) {
				t.Fatalf("NormalizeRepository(%q) = %q, %v; want ErrRepository", tc.raw, got, err)
			}
		})
	}
}
