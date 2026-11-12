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
	// Only input structs ever carry credentials. MarketSources never selects the encrypted columns.
	Credentials *pluginmarket.Credentials `json:"credentials,omitempty"`
	// GitCredentials is write-only: the HTTPS username/token pair used to clone the repository.
	GitCredentials *pluginmarket.GitCredentials `json:"gitCredentials,omitempty"`
	// ClearGitCredentials asks an edit to drop the saved Git credentials.
	ClearGitCredentials bool `json:"clearGitCredentials,omitempty"`
	// GitConfigured reports whether a source has saved Git credentials; the secret is never read out.
	GitConfigured bool `json:"gitConfigured,omitempty"`

	Namespace string     `json:"namespace"`
	URL       string     `json:"url"`
	Branch    string     `json:"branch"`
	Enabled   bool       `json:"enabled"`
	SyncedAt  *time.Time `json:"syncedAt"`
	Failure   string     `json:"failure"`
}

func (s *Store) MarketSources(ctx context.Context, tenant, space string) ([]MarketSource, error) {
	if e := requireSpace(ctx, s.pool, tenant, space); e != nil {
		return nil, e
	}
	rows, e := s.pool.Query(ctx, `select namespace,url,branch,enabled,synced_at,failure,retrieval,git_credentials is not null from v2_market_sources where space_id=$1 order by namespace`, space)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []MarketSource{}
	for rows.Next() {
		var m MarketSource
		if e := rows.Scan(&m.Namespace, &m.URL, &m.Branch, &m.Enabled, &m.SyncedAt, &m.Failure, &m.Retrieval, &m.GitConfigured); e != nil {
			return nil, e
		}
		if m.Namespace != "official" || !s.officialDisabled {
			out = append(out, m)
		}
	}
	return out, rows.Err()
}
func (s *Store) ChangeMarket(ctx context.Context, tenant, space, id, action string, in MarketSource) error {
	if e := requireSpace(ctx, s.pool, tenant, space); e != nil {
		return e
	}
	if id == "official" {
		if s.officialDisabled {
			return ErrNotFound
		}
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
		var gitSealed []byte
		if in.GitCredentials != nil {
			gitSealed, e = pluginmarket.SealGitCredentials(*in.GitCredentials, space+"/"+newID)
			if e != nil {
				return e
			}
		}
		if action == "POST" {
			tag, e := s.pool.Exec(ctx, `insert into v2_market_sources(space_id,namespace,url,branch,enabled,retrieval,credentials,git_credentials) values($1,$2,$3,$4,$5,$6,$7,$8) on conflict do nothing`, space, newID, in.URL, in.Branch, in.Enabled, config, sealed, gitSealed)
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
		var reusable, gitStranded bool
		e = tx.QueryRow(ctx, `select coalesce(credentials is not null and namespace=$3 and retrieval->>'endpoint'=$4 and retrieval->>'bucket'=$5,false), git_credentials is not null and namespace<>$3 from v2_market_sources where space_id=$1 and namespace=$2 for update`, space, id, newID, in.Retrieval.Endpoint, in.Retrieval.Bucket).Scan(&reusable, &gitStranded)
		if errors.Is(e, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if e != nil {
			return e
		}
		if in.Retrieval.Mode == "s3_sigv4" && sealed == nil && !reusable {
			return pluginmarket.ErrCredentials
		}
		// Saved Git credentials are sealed to the source namespace, which follows the URL. A new URL
		// could not open them, and they were meant for the old repository anyway, so the edit must
		// supply new credentials or drop the saved ones instead of keeping a secret no sync can use.
		if gitStranded && gitSealed == nil && !in.ClearGitCredentials {
			return pluginmarket.ErrCredentials
		}
		_, e = tx.Exec(ctx, `update v2_market_sources set namespace=$8,url=$3,branch=$4,enabled=$5,retrieval=$6,
			credentials=case when ($6::jsonb)->>'mode'='s3_sigv4' then coalesce($7,credentials) end,
			git_credentials=case when $9 then null when $10::bytea is not null then $10 else git_credentials end,
			snapshot=case when namespace=$8 then snapshot else '[]'::jsonb end,
			synced_at=case when namespace=$8 then synced_at end,
			failure=case when namespace=$8 then failure else '' end,
			revision=revision+1 where space_id=$1 and namespace=$2`, space, id, in.URL, in.Branch, in.Enabled, config, sealed, newID, in.ClearGitCredentials, gitSealed)
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
		var gitSealed []byte
		e := s.pool.QueryRow(ctx, `update v2_market_sources set revision=revision+1 where space_id=$1 and namespace=$2 and enabled returning url,branch,revision,retrieval,git_credentials`, space, id).Scan(&m.URL, &m.Branch, &revision, &m.Retrieval, &gitSealed)
		if errors.Is(e, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if e != nil {
			return e
		}
		var gitCredentials *pluginmarket.GitCredentials
		if len(gitSealed) > 0 {
			creds, e := pluginmarket.OpenGitCredentials(gitSealed, space+"/"+id)
			if e != nil {
				return e
			}
			gitCredentials = &creds
		}
		entries, fetchErr := pluginmarket.ReadPublic(ctx, pluginmarket.Source{Namespace: id, URL: m.URL, Branch: m.Branch}, m.Retrieval.UseProxy, gitCredentials)
		if fetchErr != nil {
			failure := "同步失败，请检查仓库地址和分支。"
			if errors.Is(fetchErr, pluginmarket.ErrGitAuth) {
				failure = "同步失败：仓库认证失败，请检查 Git 用户名和令牌。"
			}
			_, e = s.pool.Exec(ctx, `update v2_market_sources set failure=$4 where space_id=$1 and namespace=$2 and revision=$3`, space, id, revision, failure)
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
