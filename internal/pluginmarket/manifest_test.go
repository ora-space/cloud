package pluginmarket

import (
	"strings"
	"testing"
)

func mustParse(t *testing.T, source string) *Manifest {
	t.Helper()
	m, e := parseManifest([]byte(source))
	if e != nil {
		t.Fatalf("parseManifest: %v", e)
	}
	return m
}

func mustReject(t *testing.T, source, wantField string) {
	t.Helper()
	_, e := parseManifest([]byte(source))
	if e == nil {
		t.Fatalf("expected rejection, got success")
	}
	field := ""
	if me, ok := e.(*ManifestError); ok {
		field = me.Field
	}
	if field != wantField {
		t.Fatalf("expected field %q rejection, got %v", wantField, e)
	}
}

const baseTemplate = "resolver = 1\nidentifier = \"hello-world\"\ntitle = \"Hello\"\nkind = \"agent\"\nversion = \"1.2.3\"\ndescription = \"A friendly agent.\"\nurl = \"https://example.invalid/hello-1.2.3.orax\"\nsha256 = \"%s\"\n"

// validBase renders the template with a real 64-hex digest.
func validBase() string { return strings.ReplaceAll(baseTemplate, "%s", digest) }

const digest = "abababababababababababababababababababababababababababababababab"

func TestParseManifestAcceptsAllEightKinds(t *testing.T) {
	for _, kind := range []string{"workbench", "agent", "webview", "skill", "mcp", "hook", "pack", "workflow"} {
		source := validBase()
		if kind == "pack" {
			source = "resolver = 1\nidentifier = \"all-in-one\"\nkind = \"pack\"\nversion = \"1.0.0\"\ndescription = \"A pack.\"\n[pack]\nmembers = [\"a\", \"b\"]\n"
		}
		source = strings.Replace(source, "kind = \"agent\"", "kind = \""+kind+"\"", 1)
		m := mustParse(t, source)
		if m.Kind != kind {
			t.Fatalf("kind = %q, want %q", m.Kind, kind)
		}
	}
}

func TestParseManifestUniversalRelease(t *testing.T) {
	m := mustParse(t, validBase())
	if m.URL != "https://example.invalid/hello-1.2.3.orax" || m.SHA256 != strings.Repeat("ab", 32) {
		t.Fatalf("universal release not parsed: %+v", m)
	}
	if m.Targets != nil {
		t.Fatalf("universal release must not carry targets: %+v", m.Targets)
	}
}

func TestParseManifestTargetedRelease(t *testing.T) {
	source := strings.Replace(validBase(), "url = \"https://example.invalid/hello-1.2.3.orax\"\nsha256 = \""+strings.Repeat("ab", 32)+"\"\n", "", 1)
	source = strings.Replace(source, "kind = \"agent\"", "kind = \"hook\"", 1)
	source += "[[targets]]\ntarget = \"x86_64-unknown-linux-gnu\"\nurl = \"https://example.invalid/hello-linux.orax\"\nsha256 = \"" + strings.Repeat("cd", 32) + "\"\n"
	m := mustParse(t, source)
	if len(m.Targets) != 1 || m.Targets[0].Target != "x86_64-unknown-linux-gnu" {
		t.Fatalf("targeted release not parsed: %+v", m)
	}
}

func TestParseManifestTitleFallsBackToIdentifier(t *testing.T) {
	m := mustParse(t, strings.Replace(validBase(), "title = \"Hello\"\n", "", 1))
	if m.Title != "hello-world" {
		t.Fatalf("title fallback = %q", m.Title)
	}
}

func TestParseManifestMarketplaceVisibleDefault(t *testing.T) {
	if m := mustParse(t, validBase()); !m.MarketplaceVisible {
		t.Fatal("marketplace_visible must default to true")
	}
	if m := mustParse(t, validBase()+"marketplace_visible = false\n"); m.MarketplaceVisible {
		t.Fatal("marketplace_visible = false must be honored")
	}
}

func TestParseManifestRejectsInvalidInput(t *testing.T) {
	digest := strings.Repeat("ab", 32)
	cases := []struct {
		name   string
		source string
		field  string
	}{
		{"oversized manifest", strings.Repeat("a", MaxManifestBytes+1), ""},
		{"resolver two", strings.Replace(validBase(), "resolver = 1", "resolver = 2", 1), "resolver"},
		{"resolver missing", strings.Replace(validBase(), "resolver = 1\n", "", 1), "resolver"},
		{"unknown kind", strings.Replace(validBase(), "kind = \"agent\"", "kind = \"daemon\"", 1), "kind"},
		{"bad semver", strings.Replace(validBase(), "version = \"1.2.3\"", "version = \"v1\"", 1), "version"},
		{"leading zero semver", strings.Replace(validBase(), "version = \"1.2.3\"", "version = \"01.2.3\"", 1), "version"},
		{"title too long", strings.Replace(validBase(), "title = \"Hello\"", "title = \""+strings.Repeat("t", 129)+"\"", 1), "title"},
		{"title control char", strings.Replace(validBase(), "title = \"Hello\"", "title = \"He\x01llo\"", 1), ""}, // TOML itself rejects control characters in basic strings.
		{"title leading space", strings.Replace(validBase(), "title = \"Hello\"", "title = \" Hello\"", 1), "title"},
		{"description too long", strings.Replace(validBase(), "description = \"A friendly agent.\"", "description = \""+strings.Repeat("d", 1001)+"\"", 1), "description"},
		{"description missing", strings.Replace(validBase(), "description = \"A friendly agent.\"\n", "", 1), "description"},
		{"license too long", validBase() + "license = \"" + strings.Repeat("l", 257) + "\"\n", "license"},
		{"license non ascii", validBase() + "license = \"版権\"\n", "license"},
		{"sha256 short", strings.Replace(validBase(), "sha256 = \""+digest+"\"", "sha256 = \"abcd\"", 1), "sha256"},
		{"url and targets together", strings.Replace(validBase(), "kind = \"agent\"", "kind = \"hook\"", 1) + "[[targets]]\ntarget = \"x86_64-unknown-linux-gnu\"\nurl = \"https://example.invalid/t.orax\"\nsha256 = \"" + digest + "\"\n", "targets"},
		{"half universal release", strings.Replace(validBase(), "sha256 = \""+digest+"\"\n", "", 1), "url"},
		{"pack with url", "resolver = 1\nidentifier = \"p\"\nkind = \"pack\"\nversion = \"1.0.0\"\ndescription = \"A pack.\"\nurl = \"https://example.invalid/p.orax\"\nsha256 = \"" + digest + "\"\n", "url"},
		{"pack with targets", "resolver = 1\nidentifier = \"p\"\nkind = \"pack\"\nversion = \"1.0.0\"\ndescription = \"A pack.\"\n[[targets]]\ntarget = \"x86_64-unknown-linux-gnu\"\nurl = \"https://example.invalid/p.orax\"\nsha256 = \"" + digest + "\"\n", "targets"},
		{"unknown target triple", strings.Replace(strings.Replace(validBase(), "kind = \"agent\"", "kind = \"hook\"", 1), "url = \"https://example.invalid/hello-1.2.3.orax\"\nsha256 = \""+digest+"\"\n", "", 1) + "[[targets]]\ntarget = \"not-a-real-triple\"\nurl = \"https://example.invalid/p.orax\"\nsha256 = \"" + digest + "\"\n", "targets[0].target"},
		{"duplicate target triple", strings.Replace(strings.Replace(validBase(), "kind = \"agent\"", "kind = \"hook\"", 1), "url = \"https://example.invalid/hello-1.2.3.orax\"\nsha256 = \""+digest+"\"\n", "", 1) + "[[targets]]\ntarget = \"x86_64-unknown-linux-gnu\"\nurl = \"https://example.invalid/p.orax\"\nsha256 = \"" + digest + "\"\n[[targets]]\ntarget = \"x86_64-unknown-linux-gnu\"\nurl = \"https://example.invalid/q.orax\"\nsha256 = \"" + digest + "\"\n", "targets[1].target"},
		{"targets on skill kind", strings.Replace(validBase(), "kind = \"agent\"", "kind = \"skill\"", 1) + "[[targets]]\ntarget = \"x86_64-unknown-linux-gnu\"\nurl = \"https://example.invalid/p.orax\"\nsha256 = \"" + digest + "\"\n", "targets"},
		{"identifier uppercase", strings.Replace(validBase(), "identifier = \"hello-world\"", "identifier = \"Hello\"", 1), "identifier"},
		{"identifier three segments", strings.Replace(validBase(), "identifier = \"hello-world\"", "identifier = \"a.b.c\"", 1), "identifier"},
		{"identifier bad slug", strings.Replace(validBase(), "identifier = \"hello-world\"", "identifier = \"-x\"", 1), "identifier"},
		{"non https url", strings.Replace(validBase(), "url = \"https://example.invalid/hello-1.2.3.orax\"", "url = \"http://example.invalid/hello.orax\"", 1), "url"},
		{"s3 scheme url", strings.Replace(validBase(), "url = \"https://example.invalid/hello-1.2.3.orax\"", "url = \"s3://bucket/hello.orax\"", 1), "url"},
		{"object key traversal", strings.Replace(validBase(), "url = \"https://example.invalid/hello-1.2.3.orax\"", "url = \"../secret\"", 1), "url"},
		{"homepage with query", validBase() + "homepage = \"https://example.invalid/?q=1\"\n", "homepage"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mustReject(t, tc.source, tc.field)
		})
	}
}

func TestParseManifestAcceptsObjectKeyLocator(t *testing.T) {
	m := mustParse(t, strings.Replace(validBase(), "url = \"https://example.invalid/hello-1.2.3.orax\"", "url = \"releases/hello-1.2.3.orax\"", 1))
	if m.URL != "releases/hello-1.2.3.orax" {
		t.Fatalf("object key locator = %q", m.URL)
	}
}
