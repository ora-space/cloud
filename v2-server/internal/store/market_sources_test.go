package store_test

import (
	"encoding/json"
	"errors"
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
	if e := change(a, owner, id, "DELETE"); e != nil {
		t.Fatal(e)
	}
	if _, e := f.store.RemoveSpacePlugin(f.ctx, f.tenant, a, owner, "plugins", "remove", store.PluginRemoveInput{Identifier: item.ID, Version: selected.Version}); e != nil {
		t.Fatal(e)
	}
}
