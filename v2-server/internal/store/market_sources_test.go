package store_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"github.com/wanglongan587/cloud/v2-server/internal/pluginmarket"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/store/storetest"
	"testing"
	"time"
)

func TestCustomMarketKeepsSelectionsAndIsolatesSpaces(t *testing.T) {
	f := newSquadFixture(t)
	// Disabling the official source must leave custom Git snapshots and S3 credentials usable.
	f.store.DisableOfficialMarket(true)
	owner := f.login(t, "owner", "owner")
	spaces, e := f.store.TenantSpaces(f.ctx, storetest.Access(t, f.store, f.tenant, owner))
	if e != nil {
		t.Fatal(e)
	}
	a := spaces[0].ID
	var b string
	if e = f.db.QueryRow(f.ctx, `insert into v2_spaces(tenant_id,name,slug,created_by) values($1,'Other','other',$2) returning id`, f.tenant, owner).Scan(&b); e != nil {
		t.Fatal(e)
	}
	in := store.MarketSource{URL: "https://example.org/market", Branch: "main", Enabled: true}
	change := func(space, id, action string) error {
		return f.store.ChangeMarket(f.ctx, f.tenant, space, id, action, in)
	}
	if e := change(a, "", "POST"); e != nil {
		t.Fatal(e)
	}
	list, e := f.store.MarketSources(f.ctx, f.tenant, a)
	if e != nil || len(list) != 1 {
		t.Fatal(list, e)
	}
	id := list[0].Namespace
	item := store.PluginCatalogEntry{ID: id + "/test", SourceNamespace: id, Identifier: "test", Version: "1.0.0", Kind: "skill", Title: "Test", MarketplaceVisible: true, IndexedAt: time.Now()}
	url, digest := "https://example.org/pkg.orax", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	item.URL = &url
	item.SHA256 = &digest
	agent := store.PluginCatalogEntry{ID: id + "/agent", SourceNamespace: id, Identifier: "agent", Version: "0.3.18", Kind: "agent", Title: "Agent", MarketplaceVisible: true, IndexedAt: time.Now()}
	raw, _ := json.Marshal([]store.PluginCatalogEntry{item, agent})
	if _, e := f.db.Exec(f.ctx, `update v2_market_sources set snapshot=$2,synced_at=now() where namespace=$1`, id, raw); e != nil {
		t.Fatal(e)
	}
	for _, pair := range []struct {
		space string
		n     int
	}{{a, 2}, {b, 0}} {
		list, _, e := f.store.PluginCatalog(f.ctx, f.tenant, pair.space)
		if e != nil || len(list) != pair.n {
			t.Fatal(list, e)
		}
	}
	if !errors.Is(change(b, id, "DELETE"), store.ErrNotFound) {
		t.Fatal("cross-space delete accepted")
	}
	in.Retrieval = pluginmarket.Retrieval{Mode: "s3_sigv4", Endpoint: "https://objects.example.org", Bucket: "market", Region: "test"}
	if e := change(a, id, "PATCH"); !errors.Is(e, pluginmarket.ErrCredentials) {
		t.Fatal("missing credentials", e)
	}
	in.Retrieval = pluginmarket.Retrieval{}
	original := in.URL
	in.URL += ".git/"
	if e := change(a, id, "PATCH"); e != nil {
		t.Fatal(e)
	}
	if list, _, e := f.store.PluginCatalog(f.ctx, f.tenant, a); e != nil || len(list) != 2 {
		t.Fatal("alias lost catalog", list, e)
	} else {
		found := false
		for _, entry := range list {
			if entry.ID == item.ID {
				found = true
			}
		}
		if !found {
			t.Fatal("alias lost original entry", list)
		}
	}
	in.URL = "https://example.org/other"
	if e := change(a, "", "POST"); e != nil {
		t.Fatal(e)
	}
	in.URL += ".git"
	if e := change(a, id, "PATCH"); !errors.Is(e, store.ErrConflict) {
		t.Fatal("duplicate", e)
	}
	in.URL = original
	selected, e := f.store.InstallSpacePlugin(f.ctx, f.tenant, a, owner, "plugins", "choose", store.PluginInstallInput{Identifier: item.ID, PluginVersion: item.Version})
	if e != nil {
		t.Fatal(e)
	}
	in.Enabled = false
	if e := change(a, id, "PATCH"); e != nil {
		t.Fatal(e)
	}
	if list, _, e := f.store.PluginCatalog(f.ctx, f.tenant, a); e != nil || len(list) != 0 {
		t.Fatal(list, e)
	}
	in.Enabled = true
	in.URL = "https://example.org/replacement"
	if e := change(a, id, "PATCH"); e != nil {
		t.Fatal(e)
	}
	if list, _, e := f.store.PluginCatalog(f.ctx, f.tenant, a); e != nil || len(list) != 0 {
		t.Fatal("old snapshot visible", list, e)
	}
	sources, e := f.store.MarketSources(f.ctx, f.tenant, a)
	if e != nil || len(sources) != 2 {
		t.Fatal(sources, e)
	}
	var replacement string
	for _, source := range sources {
		if source.URL == in.URL {
			replacement = source.Namespace
			if source.SyncedAt != nil {
				t.Fatal(source)
			}
		}
	}
	if replacement == "" || replacement == id {
		t.Fatal("identity did not change", sources)
	}
	in.URL = original
	if e := change(a, "", "POST"); e != nil {
		t.Fatal(e)
	}
	if e := change(a, id, "DELETE"); e != nil {
		t.Fatal(e)
	}
	if _, e := f.store.RemoveSpacePlugin(f.ctx, f.tenant, a, owner, "plugins", "remove", store.PluginRemoveInput{Identifier: item.ID, Version: selected.Version}); e != nil {
		t.Fatal(e)
	}
}

func TestRemovingSourceClearsPlanCredentials(t *testing.T) {
	f := newSquadFixture(t)
	owner := f.login(t, "owner", "owner")
	spaces, e := f.store.TenantSpaces(f.ctx, storetest.Access(t, f.store, f.tenant, owner))
	if e != nil {
		t.Fatal(e)
	}
	space := spaces[0].ID
	t.Setenv("V2_MARKET_CREDENTIAL_KEY", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)))
	in := store.MarketSource{
		URL:       "https://example.org/private-market",
		Branch:    "main",
		Enabled:   true,
		Retrieval: pluginmarket.Retrieval{Mode: "s3_sigv4", Endpoint: "https://objects.example.org", Bucket: "market", Region: "us-east-1"},
		Credentials: &pluginmarket.Credentials{
			AccessKey: "fixture-access",
			SecretKey: "fixture-secret",
		},
	}
	if e := f.store.ChangeMarket(f.ctx, f.tenant, space, "", "POST", in); e != nil {
		t.Fatal(e)
	}
	sources, e := f.store.MarketSources(f.ctx, f.tenant, space)
	if e != nil || len(sources) != 1 {
		t.Fatal(sources, e)
	}
	namespace := sources[0].Namespace
	locator := "skills/private.orax"
	digest := "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	item := store.PluginCatalogEntry{ID: namespace + "/private", SourceNamespace: namespace, Identifier: "private", Version: "1.0.0", Kind: "skill", Title: "Private", MarketplaceVisible: true, IndexedAt: time.Now(), URL: &locator, SHA256: &digest}
	raw, _ := json.Marshal([]store.PluginCatalogEntry{item})
	if _, e := f.db.Exec(f.ctx, `update v2_market_sources set snapshot=$2,synced_at=now() where namespace=$1`, namespace, raw); e != nil {
		t.Fatal(e)
	}
	selection, e := f.store.InstallSpacePlugin(f.ctx, f.tenant, space, owner, "plugins", "private-install", store.PluginInstallInput{Identifier: item.ID})
	if e != nil {
		t.Fatal(e)
	}
	var stored bool
	if e := f.db.QueryRow(f.ctx, `select credentials is not null from v2_skill_plans where selection_id=$1`, selection.ID).Scan(&stored); e != nil || !stored {
		t.Fatalf("plan credentials stored=%t err=%v", stored, e)
	}
	if e := f.store.ChangeMarket(f.ctx, f.tenant, space, namespace, "DELETE", store.MarketSource{}); e != nil {
		t.Fatal(e)
	}
	var cleared bool
	if e := f.db.QueryRow(f.ctx, `select credentials is null from v2_skill_plans where selection_id=$1`, selection.ID).Scan(&cleared); e != nil || !cleared {
		t.Fatalf("plan credentials cleared=%t err=%v", cleared, e)
	}
	list, e := f.store.SpacePlugins(f.ctx, f.tenant, space)
	selected, found := findSpacePlugin(list, selection.ID)
	if e != nil || !found {
		t.Fatal("removing source deleted the selection", list, e)
	}
	if selected.Identifier != item.Identifier {
		t.Fatal(selected)
	}
}

func TestMarketSourceGitCredentialsAreSealedKeptAndCleared(t *testing.T) {
	f := newSquadFixture(t)
	owner := f.login(t, "owner", "owner")
	spaces, e := f.store.TenantSpaces(f.ctx, storetest.Access(t, f.store, f.tenant, owner))
	if e != nil {
		t.Fatal(e)
	}
	space := spaces[0].ID
	t.Setenv("V2_MARKET_CREDENTIAL_KEY", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32)))
	git := &pluginmarket.GitCredentials{Username: "intranet-bot", Password: "intranet-token"}
	in := store.MarketSource{URL: "https://git.example.com/market", Branch: "main", Enabled: true, GitCredentials: git}
	if e := f.store.ChangeMarket(f.ctx, f.tenant, space, "", "POST", in); e != nil {
		t.Fatal(e)
	}
	sources, e := f.store.MarketSources(f.ctx, f.tenant, space)
	if e != nil || len(sources) != 1 || !sources[0].GitConfigured {
		t.Fatalf("git credentials not reported as configured: %+v %v", sources, e)
	}
	if sources[0].GitCredentials != nil || sources[0].Credentials != nil {
		t.Fatalf("read-back leaked credentials: %+v", sources[0])
	}
	var stored []byte
	if e := f.db.QueryRow(f.ctx, `select git_credentials from v2_market_sources where namespace=$1`, sources[0].Namespace).Scan(&stored); e != nil || len(stored) == 0 {
		t.Fatalf("git credentials not stored encrypted: %v", e)
	}
	if bytes.Contains(stored, []byte("intranet-token")) || bytes.Contains(stored, []byte("intranet-bot")) {
		t.Fatal("plaintext git credentials in the database column")
	}

	// Editing without touching the auth keeps the saved credentials.
	keep := store.MarketSource{URL: "https://git.example.com/market", Branch: "develop", Enabled: true}
	if e := f.store.ChangeMarket(f.ctx, f.tenant, space, sources[0].Namespace, "PATCH", keep); e != nil {
		t.Fatal(e)
	}
	after, e := f.store.MarketSources(f.ctx, f.tenant, space)
	if e != nil || len(after) != 1 || !after[0].GitConfigured {
		t.Fatalf("credentials lost on unrelated edit: %+v %v", after, e)
	}

	// A new pair replaces the saved one.
	replace := store.MarketSource{URL: "https://git.example.com/market", Branch: "main", Enabled: true, GitCredentials: &pluginmarket.GitCredentials{Username: "new-bot", Password: "new-token"}}
	if e := f.store.ChangeMarket(f.ctx, f.tenant, space, after[0].Namespace, "PATCH", replace); e != nil {
		t.Fatal(e)
	}
	replaced, e := f.store.MarketSources(f.ctx, f.tenant, space)
	if e != nil || len(replaced) != 1 || !replaced[0].GitConfigured {
		t.Fatalf("credentials not replaced: %+v %v", replaced, e)
	}

	// An explicit clear drops them.
	clear := store.MarketSource{URL: "https://git.example.com/market", Branch: "main", Enabled: true, ClearGitCredentials: true}
	if e := f.store.ChangeMarket(f.ctx, f.tenant, space, replaced[0].Namespace, "PATCH", clear); e != nil {
		t.Fatal(e)
	}
	final, e := f.store.MarketSources(f.ctx, f.tenant, space)
	if e != nil || len(final) != 1 || final[0].GitConfigured {
		t.Fatalf("credentials not cleared: %+v %v", final, e)
	}
}

func TestMarketSourceGitCredentialsRequireNonEmptyValues(t *testing.T) {
	f := newSquadFixture(t)
	owner := f.login(t, "owner", "owner")
	spaces, e := f.store.TenantSpaces(f.ctx, storetest.Access(t, f.store, f.tenant, owner))
	if e != nil {
		t.Fatal(e)
	}
	space := spaces[0].ID
	t.Setenv("V2_MARKET_CREDENTIAL_KEY", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32)))
	in := store.MarketSource{URL: "https://git.example.com/market", Branch: "main", Enabled: true, GitCredentials: &pluginmarket.GitCredentials{Username: "", Password: ""}}
	if e := f.store.ChangeMarket(f.ctx, f.tenant, space, "", "POST", in); !errors.Is(e, pluginmarket.ErrCredentials) {
		t.Fatalf("empty git credentials accepted: %v", e)
	}
}

// Git credentials are sealed to the source namespace, which follows the URL. Moving a source to
// another repository must not keep a secret that no sync can open, so the edit has to replace or
// drop it; the rejected edit leaves the source untouched.
func TestMarketSourceURLChangeNeedsNewGitCredentials(t *testing.T) {
	f := newSquadFixture(t)
	owner := f.login(t, "owner", "owner")
	spaces, e := f.store.TenantSpaces(f.ctx, storetest.Access(t, f.store, f.tenant, owner))
	if e != nil {
		t.Fatal(e)
	}
	space := spaces[0].ID
	t.Setenv("V2_MARKET_CREDENTIAL_KEY", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32)))
	git := &pluginmarket.GitCredentials{Username: "intranet-bot", Password: "intranet-token"}
	in := store.MarketSource{URL: "https://git.example.com/market", Branch: "main", Enabled: true, GitCredentials: git}
	if e := f.store.ChangeMarket(f.ctx, f.tenant, space, "", "POST", in); e != nil {
		t.Fatal(e)
	}
	only := func(t *testing.T) store.MarketSource {
		t.Helper()
		sources, e := f.store.MarketSources(f.ctx, f.tenant, space)
		if e != nil || len(sources) != 1 {
			t.Fatalf("sources = %+v, %v", sources, e)
		}
		return sources[0]
	}
	before := only(t)

	moved := store.MarketSource{URL: "https://git.example.com/other-market", Branch: "main", Enabled: true}
	if e := f.store.ChangeMarket(f.ctx, f.tenant, space, before.Namespace, "PATCH", moved); !errors.Is(e, pluginmarket.ErrCredentials) {
		t.Fatalf("moving a source while keeping its Git credentials: %v, want ErrCredentials", e)
	}
	if got := only(t); got.Namespace != before.Namespace || got.URL != before.URL || !got.GitConfigured {
		t.Fatalf("rejected edit changed the source: %+v", got)
	}

	// The same URL keeps its credentials when the edit does not mention them.
	same := store.MarketSource{URL: before.URL, Branch: "main", Enabled: false}
	if e := f.store.ChangeMarket(f.ctx, f.tenant, space, before.Namespace, "PATCH", same); e != nil {
		t.Fatal(e)
	}
	if got := only(t); !got.GitConfigured {
		t.Fatalf("an edit without a URL change dropped the credentials: %+v", got)
	}

	cleared := moved
	cleared.ClearGitCredentials = true
	if e := f.store.ChangeMarket(f.ctx, f.tenant, space, before.Namespace, "PATCH", cleared); e != nil {
		t.Fatal(e)
	}
	after := only(t)
	if after.Namespace == before.Namespace || after.GitConfigured {
		t.Fatalf("moving with clear: %+v", after)
	}

	back := store.MarketSource{URL: before.URL, Branch: "main", Enabled: true, GitCredentials: git}
	if e := f.store.ChangeMarket(f.ctx, f.tenant, space, after.Namespace, "PATCH", back); e != nil {
		t.Fatal(e)
	}
	if got := only(t); got.Namespace != before.Namespace || !got.GitConfigured {
		t.Fatalf("moving with new credentials: %+v", got)
	}
}

func TestOfficialMarketCapabilityUsesConfiguration(t *testing.T) {
	s := &store.Store{}
	if !s.OfficialMarketEnabled() {
		t.Fatal("development default must enable official source")
	}
	s.DisableOfficialMarket(true)
	if s.OfficialMarketEnabled() {
		t.Fatal("disabled official source reported enabled")
	}
	s.DisableOfficialMarket(false)
	if !s.OfficialMarketEnabled() {
		t.Fatal("re-enabled official source reported disabled")
	}
}
