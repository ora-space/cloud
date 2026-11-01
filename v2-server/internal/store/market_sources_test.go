package store_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"github.com/wanglongan587/cloud/v2-server/internal/pluginmarket"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"testing"
	"time"
)

func TestCustomMarketKeepsSelectionsAndIsolatesSpaces(t *testing.T) {
	f := newSquadFixture(t)
	owner := f.login(t, "owner", "owner")
	member := f.login(t, "reader", "member")
	spaces, e := f.store.TenantSpaces(f.ctx, f.tenant, owner)
	if e != nil {
		t.Fatal(e)
	}
	a := spaces[0].ID
	var b string
	if e = f.db.QueryRow(f.ctx, `insert into v2_spaces(tenant_id,name,slug,created_by) values($1,'Other','other',$2) returning id`, f.tenant, owner).Scan(&b); e != nil {
		t.Fatal(e)
	}
	in := store.MarketSource{URL: "https://example.org/market", Branch: "main", Enabled: true}
	change := func(space, user, id, action string) error {
		return f.store.ChangeMarket(f.ctx, f.tenant, space, user, id, action, in)
	}
	if !errors.Is(change(a, member, "", "POST"), store.ErrForbidden) {
		t.Fatal("member write accepted")
	}
	if e := change(a, owner, "", "POST"); e != nil {
		t.Fatal(e)
	}
	list, e := f.store.MarketSources(f.ctx, f.tenant, a, member)
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
	}{{a, 1}, {b, 0}} {
		list, _, e := f.store.PluginCatalog(f.ctx, f.tenant, pair.space, member)
		if e != nil || len(list) != pair.n {
			t.Fatal(list, e)
		}
	}
	if !errors.Is(change(b, owner, id, "DELETE"), store.ErrNotFound) {
		t.Fatal("cross-space delete accepted")
	}
	in.Retrieval = pluginmarket.Retrieval{Mode: "s3_sigv4", Endpoint: "https://objects.example.org", Bucket: "market", Region: "test"}
	if e := change(a, owner, id, "PATCH"); !errors.Is(e, pluginmarket.ErrCredentials) {
		t.Fatal("missing credentials", e)
	}
	in.Retrieval = pluginmarket.Retrieval{}
	original := in.URL
	in.URL += ".git/"
	if e := change(a, owner, id, "PATCH"); e != nil {
		t.Fatal(e)
	}
	if list, _, e := f.store.PluginCatalog(f.ctx, f.tenant, a, member); e != nil || len(list) != 1 || list[0].ID != item.ID {
		t.Fatal("alias lost catalog", list, e)
	}
	in.URL = "https://example.org/other"
	if e := change(a, owner, "", "POST"); e != nil {
		t.Fatal(e)
	}
	in.URL += ".git"
	if e := change(a, owner, id, "PATCH"); !errors.Is(e, store.ErrConflict) {
		t.Fatal("duplicate", e)
	}
	in.URL = original
	selected, e := f.store.InstallSpacePlugin(f.ctx, f.tenant, a, owner, "plugins", "choose", store.PluginInstallInput{Identifier: item.ID, PluginVersion: item.Version})
	if e != nil {
		t.Fatal(e)
	}
	in.Enabled = false
	if e := change(a, owner, id, "PATCH"); e != nil {
		t.Fatal(e)
	}
	if list, _, e := f.store.PluginCatalog(f.ctx, f.tenant, a, member); e != nil || len(list) != 0 {
		t.Fatal(list, e)
	}
	in.Enabled = true
	in.URL = "https://example.org/replacement"
	if e := change(a, owner, id, "PATCH"); e != nil {
		t.Fatal(e)
	}
	if list, _, e := f.store.PluginCatalog(f.ctx, f.tenant, a, member); e != nil || len(list) != 0 {
		t.Fatal("old snapshot visible", list, e)
	}
	sources, e := f.store.MarketSources(f.ctx, f.tenant, a, owner)
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
	if e := change(a, owner, "", "POST"); e != nil {
		t.Fatal(e)
	}
	if e := change(a, owner, id, "DELETE"); e != nil {
		t.Fatal(e)
	}
	if _, e := f.store.RemoveSpacePlugin(f.ctx, f.tenant, a, owner, "plugins", "remove", store.PluginRemoveInput{Identifier: item.ID, Version: selected.Version}); e != nil {
		t.Fatal(e)
	}
}

func TestRemovingSourceClearsPlanCredentials(t *testing.T) {
	f := newSquadFixture(t)
	owner := f.login(t, "owner", "owner")
	spaces, e := f.store.TenantSpaces(f.ctx, f.tenant, owner)
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
	if e := f.store.ChangeMarket(f.ctx, f.tenant, space, owner, "", "POST", in); e != nil {
		t.Fatal(e)
	}
	sources, e := f.store.MarketSources(f.ctx, f.tenant, space, owner)
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
	if e := f.store.ChangeMarket(f.ctx, f.tenant, space, owner, namespace, "DELETE", store.MarketSource{}); e != nil {
		t.Fatal(e)
	}
	var cleared bool
	if e := f.db.QueryRow(f.ctx, `select credentials is null from v2_skill_plans where selection_id=$1`, selection.ID).Scan(&cleared); e != nil || !cleared {
		t.Fatalf("plan credentials cleared=%t err=%v", cleared, e)
	}
	list, e := f.store.SpacePlugins(f.ctx, f.tenant, space, owner)
	selected, found := findSpacePlugin(list, selection.ID)
	if e != nil || !found {
		t.Fatal("removing source deleted the selection", list, e)
	}
	if selected.Identifier != item.Identifier {
		t.Fatal(selected)
	}
}
