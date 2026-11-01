package store

import "context"

type SkillProgress struct {
	ProjectID        string `json:"projectId"`
	Name             string `json:"name"`
	State            string `json:"state"`
	InstalledVersion string `json:"installedVersion"`
	Failure          string `json:"failure"`
}

func (s *Store) SkillProgress(ctx context.Context, tenant, space, user, namespace, id string) ([]SkillProgress, error) {
	if _, e := requireSpaceAccess(ctx, s.pool, tenant, space, user); e != nil {
		return nil, e
	}
	rows, e := s.pool.Query(ctx, `select p.id,coalesce(p.data->>'Name',p.id),
 case when r.revision=a.version then r.state when a.desired_state='removed' and r.project_id is null then 'removed' else 'waiting' end,
 coalesce(r.installed_version,''),case when r.revision=a.version then coalesce(r.failure,'') else '' end
 from v2_projects p join v2_space_agents a on a.tenant_id=p.tenant_id and a.space_id=p.plugin_space_id
 left join v2_skill_results r on r.selection_id=a.id and r.project_id=p.id
 where a.tenant_id=$1 and a.space_id=$2 and a.source_namespace=$3 and a.identifier=$4 and a.kind='skill'
 order by p.data->>'Name',p.id`, tenant, space, namespace, id)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	items := []SkillProgress{}
	for rows.Next() {
		var p SkillProgress
		if e := rows.Scan(&p.ProjectID, &p.Name, &p.State, &p.InstalledVersion, &p.Failure); e != nil {
			return nil, e
		}
		items = append(items, p)
	}
	return items, rows.Err()
}
