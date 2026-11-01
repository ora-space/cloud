package pluginmarket

import (
	"net/netip"
	"testing"
)

func TestRejectInternalMarketplaces(t *testing.T) {
	for _, s := range []string{"file:///etc", "https://key@github.com/repo", "https://example.org:22/repo", "https://example.org/repo?token=x"} {
		if _, e := ValidatePublicSource(s, "main"); e == nil {
			t.Fatal(s)
		}
	}
	for _, s := range []string{"127.0.0.1", "10.0.0.1", "169.254.169.254", "::1", "::ffff:192.168.0.2", "64:ff9b::a00:1", "100.64.0.2"} {
		if publicIP(netip.MustParseAddr(s)) {
			t.Fatal(s)
		}
	}
	if !publicIP(netip.MustParseAddr("140.82.112.3")) {
		t.Fatal("public Git host rejected")
	}
}

func TestNamespaceMatchesDesktop(t *testing.T) {
	u, e := ValidatePublicSource("https://GitHub.com/Acme/Awesome-Plugins.git/", "main")
	if e != nil || Namespace(u) != "awesome-plugins.2aa64f48" {
		t.Fatal(u, e)
	}
}
