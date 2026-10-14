import { UnavailablePage } from '@/features/unavailable/unavailable-page'
import { workspacePaths } from '@/lib/paths'

/** Keeps standalone chat URLs readable and directs work to real issue Threads. */
export function ChatPage({ slug }: { slug: string }) {
  return (
    <UnavailablePage
      title="聊天"
      description="独立聊天暂未开放。请进入任务，在活动栏选择已安装的 Agent 发起任务，并在该任务的 Agent 会话中继续对话。"
      actions={[{ label: '前往任务', to: workspacePaths(slug).issues }]}
    />
  )
}
