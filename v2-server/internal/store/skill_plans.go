package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5"
	"github.com/wanglongan587/cloud/v2-server/internal/pluginmarket"
	"runtime"
)

func freezeSkill(ctx context.Context, tx pgx.Tx, selection SpacePlugin, entry PluginCatalogEntry) error {
	target := map[string]string{"amd64": "x86_64-unknown-linux-gnu", "arm64": "aarch64-unknown-linux-gnu"}[runtime.GOARCH]
	var locator, digest string
	if entry.URL != nil && entry.SHA256 != nil {
		locator, digest = *entry.URL, *entry.SHA256
	} else {
		raw, e := json.Marshal(entry.Targets)
		if e != nil {
			return e
		}
		var targets []pluginmarket.ReleaseTarget
		if e := json.Unmarshal(raw, &targets); e != nil {
			return e
		}
		for _, v := range targets {
			if v.Target == target {
				locator, digest = v.URL, v.SHA256
				break
			}
		}
	}
	if locator == "" || digest == "" || target == "" {
		return ErrPluginVersionUnavailable
	}
	config := []byte("{}")
	var credentials []byte
	if entry.SourceNamespace != "official" {
		if e := tx.QueryRow(ctx, `SELECT retrieval,credentials FROM v2_market_sources WHERE space_id=$1 AND namespace=$2 AND enabled FOR SHARE`, selection.SpaceID, entry.SourceNamespace).Scan(&config, &credentials); errors.Is(e, pgx.ErrNoRows) {
			return ErrPluginNotFound
		} else if e != nil {
			return e
		}
	}
	_, e := tx.Exec(ctx, `INSERT INTO v2_skill_plans(selection_id,locator,digest,target,retrieval,credentials) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(selection_id) DO UPDATE SET locator=excluded.locator,digest=excluded.digest,target=excluded.target,retrieval=excluded.retrieval,credentials=excluded.credentials`, selection.ID, locator, digest, target, config, credentials)
	return e
}

// SkillJob freezes a single project command. No caller-facing DTO includes its locator or credentials.
type SkillJob struct {
	Project, Selection, Space, Namespace, Identifier, Version, Desired, Locator, Digest, Previous string
	Retrieval                                                                                     pluginmarket.Retrieval
	Credentials                                                                                   []byte
	Revision                                                                                      int64
}

// SkillJobOutcome distinguishes a completed operation from terminal and retryable failures.
type SkillJobOutcome int

const (
	// SkillJobSucceeded records the Node's confirmed install or removal.
	SkillJobSucceeded SkillJobOutcome = iota
	// SkillJobFailed records an operation that will not change without user action.
	SkillJobFailed
	// SkillJobRetry records a temporary failure and schedules another attempt with backoff.
	SkillJobRetry
)

func (s *Store) SkillJobs(ctx context.Context) ([]SkillJob, error) {
	rows, e := s.pool.Query(ctx, `select p.id,a.id,a.space_id,a.source_namespace,a.identifier,a.desired_version,a.desired_state,
 f.locator,f.digest,f.retrieval,f.credentials,a.version,coalesce(r.installed_version,'')
 from v2_projects p join v2_spaces sp on sp.id=p.plugin_space_id and sp.tenant_id=p.tenant_id and sp.archived_at is null
 join v2_space_agents a on a.space_id=sp.id and a.tenant_id=p.tenant_id join v2_skill_plans f on f.selection_id=a.id
 left join v2_skill_results r on r.selection_id=a.id and r.project_id=p.id
 where (r.revision is distinct from a.version or r.state='applying'
  or (r.state='failed' and r.retry_at is not null and r.retry_at <= now())) and
 (a.desired_state='installed' or r.installed_version is not null or r.state in ('applying','failed'))
 order by p.id,a.id`)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var jobs []SkillJob
	for rows.Next() {
		var j SkillJob
		if e := rows.Scan(&j.Project, &j.Selection, &j.Space, &j.Namespace, &j.Identifier, &j.Version, &j.Desired, &j.Locator, &j.Digest, &j.Retrieval, &j.Credentials, &j.Revision, &j.Previous); e != nil {
			return nil, e
		}
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}
func (s *Store) StartSkillJob(ctx context.Context, j SkillJob) (bool, error) {
	tag, e := s.pool.Exec(ctx, `insert into v2_skill_results(selection_id,project_id,revision,state) select a.id,p.id,a.version,'applying' from v2_space_agents a join v2_projects p on p.plugin_space_id=a.space_id and p.tenant_id=a.tenant_id where a.id=$1 and a.version=$2 and p.id=$3 on conflict(selection_id,project_id) do update set revision=excluded.revision,state='applying',failure=null,retry_at=null,retry_count=case when v2_skill_results.revision=excluded.revision then v2_skill_results.retry_count else 0 end`, j.Selection, j.Revision, j.Project)
	return tag.RowsAffected() == 1, e
}
func (s *Store) FinishSkillJob(ctx context.Context, j SkillJob, outcome SkillJobOutcome) error {
	state := "installed"
	var version any = j.Version
	var failure any
	if j.Desired == "removed" {
		state = "removed"
		version = nil
	}
	switch outcome {
	case SkillJobSucceeded:
		_, e := s.pool.Exec(ctx, `update v2_skill_results set state=$4,installed_version=$5,failure=null,retry_count=0,retry_at=null where selection_id=$1 and project_id=$2 and revision=$3`, j.Selection, j.Project, j.Revision, state, version)
		return e
	case SkillJobRetry:
		state = "failed"
		failure = "下载或项目节点暂时不可用，将自动重试。"
		version = j.Previous
		if j.Previous == "" {
			version = nil
		}
		_, e := s.pool.Exec(ctx, `update v2_skill_results set state=$4,installed_version=$5,failure=$6,
 retry_count=retry_count+1,
 retry_at=now()+(least(300,5*(1<<least(retry_count,6)))*interval '1 second')
 where selection_id=$1 and project_id=$2 and revision=$3`, j.Selection, j.Project, j.Revision, state, version, failure)
		return e
	case SkillJobFailed:
		state = "failed"
		failure = "项目节点未确认操作，请重新尝试。"
		version = j.Previous
		if j.Previous == "" {
			version = nil
		}
		_, e := s.pool.Exec(ctx, `update v2_skill_results set state=$4,installed_version=$5,failure=$6,retry_at=null where selection_id=$1 and project_id=$2 and revision=$3`, j.Selection, j.Project, j.Revision, state, version, failure)
		return e
	default:
		return fmt.Errorf("unknown skill job outcome %d", outcome)
	}
}

// Re-evaluate against all bound projects, including ones created while the service was offline.
func (s *Store) RefreshSkillStates(ctx context.Context) error {
	_, e := s.pool.Exec(ctx, `with totals as (
 select a.id,a.desired_state,a.desired_version,count(p.id) as n,
 count(*) filter(where r.revision=a.version and r.state='installed') as installed,
 count(*) filter(where r.revision=a.version and r.state='failed' and r.retry_at is null) as failed,
 count(*) filter(where r.revision=a.version and r.state='failed' and r.retry_at is not null) as retrying,
 count(*) filter(where r.state='applying' or r.installed_version is not null or (r.state='failed' and r.revision<>a.version)) as outstanding
 from v2_space_agents a join v2_skill_plans f on f.selection_id=a.id
 left join v2_projects p on p.plugin_space_id=a.space_id and p.tenant_id=a.tenant_id
 left join v2_skill_results r on r.selection_id=a.id and r.project_id=p.id group by a.id
 ), states as (select *,case when failed>0 then 'failed' when desired_state='removed' and outstanding=0 then 'removed' when desired_state='removed' then 'removing' when retrying>0 then 'failed' when n>0 and installed=n then 'installed' else 'pending' end as state,
 case when failed>0 then '部分项目操作失败，请查看项目状态。' when retrying>0 then '部分项目暂时失败，将自动重试。' end as install_error from totals)
 update v2_space_agents a set observed_state=t.state,observed_version=case when t.state='installed' then t.desired_version when t.state='removed' then null else a.observed_version end,install_error=t.install_error from states t where t.id=a.id and (a.observed_state is distinct from t.state or a.install_error is distinct from t.install_error)`)
	return e
}
