package server

import (
	"errors"
	"github.com/wanglongan587/cloud/v2-server/internal/pluginmarket"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"net/http"
)

func (h *handler) marketSources(w http.ResponseWriter, r *http.Request, u store.User) {
	t, s := r.PathValue("tid"), r.PathValue("sid")
	if !squadUUID.MatchString(t) || !squadUUID.MatchString(s) {
		fault(w, 404, "not_found")
		return
	}
	if r.Method == "GET" {
		items, e := h.store.MarketSources(r.Context(), t, s, u.ID)
		if e != nil {
			pluginError(w, "list markets", e)
			return
		}
		writeJSON(w, 200, map[string]any{"items": items})
		return
	}
	var in store.MarketSource
	action := r.Method
	if r.Pattern == "POST /api/v1/tenants/{tid}/spaces/{sid}/plugins/sources/{source}/sync" {
		action = "sync"
	} else if action != "DELETE" && !decodeStrict(w, r, &in) {
		return
	}
	e := h.store.ChangeMarket(r.Context(), t, s, u.ID, r.PathValue("source"), action, in)
	if e == nil && action == "sync" && r.PathValue("source") == "official" {
		if h.opts.RefreshOfficial == nil {
			fault(w, 503, "official_sync_unavailable")
			return
		}
		e = h.opts.RefreshOfficial(r.Context())
	}
	switch {
	case e == nil:
		w.WriteHeader(204)
	case errors.Is(e, pluginmarket.ErrCredentials):
		fault(w, 400, "market_credentials_unavailable")
	case errors.Is(e, pluginmarket.ErrSource):
		fault(w, 400, "invalid_market_source")
	case errors.Is(e, store.ErrNotFound):
		fault(w, 404, "not_found")
	case action == "sync" && !errors.Is(e, store.ErrForbidden) && !errors.Is(e, store.ErrConflict):
		fault(w, 502, "market_sync_failed")
	default:
		pluginError(w, "market source", e)
	}
}
