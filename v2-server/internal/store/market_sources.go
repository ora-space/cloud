package store

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wanglongan587/cloud/v2-server/internal/pluginmarket"
	"time"
)

type MarketSource struct {
	Retrieval pluginmarket.Retrieval `json:"retrieval"`
	// Only input structs ever carry credentials. MarketSources never selects the encrypted column.
	Credentials *pluginmarket.Credentials `json:"credentials,omitempty"`

	Namespace string     `json:"namespace"`
	URL       string     `json:"url"`
	Branch    string     `json:"branch"`
	Enabled   bool       `json:"enabled"`
	SyncedAt  *time.Time `json:"syncedAt"`
	Failure   string     `json:"failure"`
}

func (s *Store) MarketSources(ctx context.Context, tenant, space, user string) ([]MarketSource, error) {
	if _, e := requireSpaceAccess(ctx, s.pool, tenant, space, user); e != nil {
		return nil, e
	}
	rows, e := s.pool.Query(ctx, `select namespace,url,branch,enabled,synced_at,failure,retrieval from v2_market_sources where space_id=$1 order by namespace`, space)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []MarketSource{}
	for rows.Next() {
		var m MarketSource
		if e := rows.Scan(&m.Namespace, &m.URL, &m.Branch, &m.Enabled, &m.SyncedAt, &m.Failure, &m.Retrieval); e != nil {
			return nil, e
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
func (s *Store) ChangeMarket(ctx context.Context, tenant, space, user, id, action string, in MarketSource) error {
	role, e := requireSpaceAccess(ctx, s.pool, tenant, space, user)
	if e != nil {
		return e
	}
	if !isAdministrator(role) {
		return ErrForbidden
	}
	if id == "official" {
		if action == "sync" {
			return nil
		}
		return ErrForbidden
	}
	switch action {
	case "POST", "PATCH":
		u, e := pluginmarket.ValidatePublicSource(in.URL, in.Branch)
		if e != nil {
			return e
		}
		if e := in.Retrieval.Validate(); e != nil {
			return e
		}
		if in.Retrieval.Mode == "" {
			in.Retrieval.Mode = "direct_https"
		}
		newID := pluginmarket.Namespace(u)
		config, e := json.Marshal(in.Retrieval)
		if e != nil {
			return e
		}
		var sealed []byte
		if in.Retrieval.Mode == "s3_sigv4" && in.Credentials != nil {
			sealed, e = pluginmarket.SealCredentials(*in.Credentials, space+"/"+newID+"/"+in.Retrieval.Endpoint+"/"+in.Retrieval.Bucket)
			if e != nil {
				return e
			}
		} else if in.Retrieval.Mode == "s3_sigv4" && action == "POST" {
			return pluginmarket.ErrCredentials
		}
		if action == "POST" {
			tag, e := s.pool.Exec(ctx, `insert into v2_market_sources(space_id,namespace,url,branch,enabled,retrieval,credentials) values($1,$2,$3,$4,$5,$6,$7) on conflict do nothing`, space, newID, in.URL, in.Branch, in.Enabled, config, sealed)
			if e == nil && tag.RowsAffected() == 0 {
				return ErrConflict
			}
			return e
		}
		tx, e := s.pool.Begin(ctx)
		if e != nil {
			return e
		}
		defer tx.Rollback(ctx)
		var reusable bool
		e = tx.QueryRow(ctx, `select coalesce(credentials is not null and namespace=$3 and retrieval->>'endpoint'=$4 and retrieval->>'bucket'=$5,false) from v2_market_sources where space_id=$1 and namespace=$2 for update`, space, id, newID, in.Retrieval.Endpoint, in.Retrieval.Bucket).Scan(&reusable)
		if errors.Is(e, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if e != nil {
			return e
		}
		if in.Retrieval.Mode == "s3_sigv4" && sealed == nil && !reusable {
			return pluginmarket.ErrCredentials
		}
		_, e = tx.Exec(ctx, `update v2_market_sources set namespace=$8,url=$3,branch=$4,enabled=$5,retrieval=$6,
			credentials=case when ($6::jsonb)->>'mode'='s3_sigv4' then coalesce($7,credentials) end,
			snapshot=case when namespace=$8 then snapshot else '[]'::jsonb end,
			synced_at=case when namespace=$8 then synced_at end,
			failure=case when namespace=$8 then failure else '' end,
			revision=revision+1 where space_id=$1 and namespace=$2`, space, id, in.URL, in.Branch, in.Enabled, config, sealed, newID)
		var pgErr *pgconn.PgError
		if errors.As(e, &pgErr) && pgErr.Code == "23505" {
			return ErrConflict
		}
		if e != nil {
			return e
		}
		return tx.Commit(ctx)
	case "DELETE":
		tx, e := s.pool.Begin(ctx)
		if e != nil {
			return e
		}
		defer tx.Rollback(ctx)
		if _, e := tx.Exec(ctx, `update v2_skill_plans p set credentials=null from v2_space_agents a where p.selection_id=a.id and a.space_id=$1 and a.source_namespace=$2`, space, id); e != nil {
			return e
		}
		tag, e := tx.Exec(ctx, `delete from v2_market_sources where space_id=$1 and namespace=$2`, space, id)
		if e == nil && tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		if e != nil {
			return e
		}
		return tx.Commit(ctx)
	case "sync":
		var m MarketSource
		var revision int64
		e = s.pool.QueryRow(ctx, `update v2_market_sources set revision=revision+1 where space_id=$1 and namespace=$2 and enabled returning url,branch,revision,retrieval`, space, id).Scan(&m.URL, &m.Branch, &revision, &m.Retrieval)
		if errors.Is(e, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if e != nil {
			return e
		}
		entries, fetchErr := pluginmarket.ReadPublic(ctx, pluginmarket.Source{Namespace: id, URL: m.URL, Branch: m.Branch}, m.Retrieval.UseProxy)
		if fetchErr != nil {
			_, e = s.pool.Exec(ctx, `update v2_market_sources set failure='同步失败，请检查仓库地址和分支。' where space_id=$1 and namespace=$2 and revision=$3`, space, id, revision)
			if e != nil {
				return e
			}
			return fetchErr
		}
		now := time.Now().UTC()
		list := []PluginCatalogEntry{}
		for _, source := range entries {
			list = append(list, catalogEntry(source, now))
		}
		raw, e := json.Marshal(list)
		if e != nil {
			return e
		}
		tag, e := s.pool.Exec(ctx, `update v2_market_sources set snapshot=$4,synced_at=$5,failure='' where space_id=$1 and namespace=$2 and revision=$3`, space, id, revision, raw, now)
		if e == nil && tag.RowsAffected() == 0 {
			return ErrConflict
		}
		return e
	}
	return ErrNotFound
}
