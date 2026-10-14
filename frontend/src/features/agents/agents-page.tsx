import { UnavailablePage } from '@/features/unavailable/unavailable-page'
import { workspacePaths } from '@/lib/paths'

/** Explains the unavailable Agent management entry without inventing execution identities. */
export function AgentsPage({ slug }: { slug: string }) {
  const paths = workspacePaths(slug)
  return (
    <UnavailablePage
      title="智能体管理"
      description="独立智能体管理暂未开放。可以在插件页安装 Agent 插件，然后在任务活动栏的“提及 / 任务”中选择真实已安装的 Agent。"
      actions={[
        { label: '管理插件', to: paths.plugins },
        { label: '前往任务', to: paths.issues },
      ]}
    />
  )
}
