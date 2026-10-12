package cloneauth

import (
	"errors"
	"testing"
)

func TestCanonicalRepositoryFoldsSpellingsOfOneRepositoryAndSeparatesTwo(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"plain", "https://github.com/acme/widgets", "https://github.com/acme/widgets"},
		{"trailing slash", "https://github.com/acme/widgets/", "https://github.com/acme/widgets"},
		{"dot git suffix", "https://github.com/acme/widgets.git", "https://github.com/acme/widgets"},
		{"dot git and slash", "https://github.com/acme/widgets.git/", "https://github.com/acme/widgets"},
		{"host case", "https://GitHub.COM/acme/widgets", "https://github.com/acme/widgets"},
		{"surrounding whitespace", "  https://github.com/acme/widgets  ", "https://github.com/acme/widgets"},
		{"explicit port is part of the identity", "https://github.com:8443/acme/widgets", "https://github.com:8443/acme/widgets"},
		{"path case is preserved", "https://github.com/Acme/Widgets", "https://github.com/Acme/Widgets"},
		{"nested group path", "https://gitlab.example.com/group/sub/widgets.git", "https://gitlab.example.com/group/sub/widgets"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got, err := CanonicalRepository(testCase.raw)
			if err != nil {
				t.Fatalf("CanonicalRepository(%q): %v", testCase.raw, err)
			}
			if got != testCase.want {
				t.Fatalf("CanonicalRepository(%q) = %q, want %q", testCase.raw, got, testCase.want)
			}
		})
	}
}

func TestCanonicalRepositoryRefusesAnythingItCannotBind(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"empty", ""},
		{"blank", "   "},
		{"ssh", "ssh://git@github.com/acme/widgets.git"},
		{"scp-like", "git@github.com:acme/widgets.git"},
		{"plain http", "http://github.com/acme/widgets"},
		{"git protocol", "git://github.com/acme/widgets"},
		{"file", "file:///srv/git/widgets.git"},
		{"no host", "https:///acme/widgets"},
		{"no path", "https://github.com"},
		{"root path only", "https://github.com/"},
		{"user info", "https://user:token@github.com/acme/widgets"},
		{"query", "https://github.com/acme/widgets?ref=main"},
		{"fragment", "https://github.com/acme/widgets#readme"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := CanonicalRepository(testCase.raw); !errors.Is(err, ErrRepository) {
				t.Fatalf("CanonicalRepository(%q) error = %v, want ErrRepository", testCase.raw, err)
			}
		})
	}
}
