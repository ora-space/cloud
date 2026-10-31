package pluginmarket

import "testing"

func TestParseOfficialMarketplaceManifests(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, source, identifier, kind, version string
		targets                                 int
	}{
		{
			name: "skill",
			source: `resolver = 1
title = "Matt Pocock Skills"
identifier = "mattpocock.skills"
kind = "skill"
version = "1.3.1"
description = "Matt Pocock's skills for real engineering"
homepage = "https://github.com/mattpocock/skills"
license = "MIT"
url = "https://github.com/ora-space/mattpocock-skill/releases/download/v1.3.1/mattpocock.skills-v1.3.1.orax"
sha256 = "c3de8a2c6863a1864901fc55e7eb42e22fe0829fb958249de59c5d4e715def64"
`,
			identifier: "mattpocock.skills", kind: "skill", version: "1.3.1",
		},
		{
			name: "agent targets",
			source: `resolver = 1
title = "OpenCode"
identifier = "ora-space.opencode"
kind = "agent"
version = "0.6.8"
description = "Ora Space OpenCode Agent"
homepage = "https://github.com/ora-space/opencode-agent"
license = "Apache-2.0"
[[targets]]
target = "x86_64-unknown-linux-gnu"
url = "https://github.com/ora-space/opencode-agent/releases/download/v0.6.8/linux.orax"
sha256 = "6ff11cbe9d6118f058d0efe4e3ffd6c93e429016df691335fc76c02789c5bdec"
`,
			identifier: "ora-space.opencode", kind: "agent", version: "0.6.8", targets: 1,
		},
		{
			name: "workflow",
			source: `resolver = 1
title = "Workflow Examples"
identifier = "ora-space.workflow-examples"
kind = "workflow"
version = "0.1.0"
description = "Official workflow examples"
license = "Apache-2.0"
url = "https://raw.githubusercontent.com/ora-space/marketplace/af3551ac/example.orax"
sha256 = "ba0b074b69828dd3bcc4c8bad7576962b50ac1717b707e16a736dc7d3e3bf18c"
`,
			identifier: "ora-space.workflow-examples", kind: "workflow", version: "0.1.0",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manifest, e := parseManifest([]byte(test.source))
			if e != nil {
				t.Fatal(e)
			}
			if manifest.Identifier != test.identifier || manifest.Kind != test.kind ||
				manifest.Version != test.version || len(manifest.Targets) != test.targets {
				t.Fatalf("manifest = %+v", manifest)
			}
		})
	}
}

func TestParseRejectsInvalidMarketplaceListing(t *testing.T) {
	t.Parallel()
	for name, source := range map[string]string{
		"target on skill": `resolver=1
identifier="bad.skill"
kind="skill"
version="1.0.0"
description="bad"
[[targets]]
target="x86_64-unknown-linux-gnu"
url="https://example.com/a.orax"
sha256="aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
`,
		"release on pack": `resolver=1
identifier="bad.pack"
kind="pack"
version="1.0.0"
description="bad"
url="https://example.com/a.orax"
sha256="aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
`,
		"invalid identifier": `resolver=1
identifier="Bad Identifier"
kind="skill"
version="1.0.0"
description="bad"
`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, e := parseManifest([]byte(source)); e == nil {
				t.Fatal("expected validation failure")
			}
		})
	}
}
