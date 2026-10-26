import { useState } from 'react'
import { Button } from '@/components/ui/button'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
} from '@/components/ui/dialog'
import { useCurrentSpace } from '@/features/spaces/current-space'
import {
  agentConfigurationFailure,
  useAgentConfiguration,
  type AgentConfiguration,
  type AgentConfigurationScope,
} from './api'
import { AgentConfigurationEditor } from './editor'

/** Opens the current user's configuration independently of space administrator capabilities. */
export function AgentConfigurationDialog({ pluginId }: { pluginId: string }) {
  const { space } = useCurrentSpace()
  const [open, setOpen] = useState(false)
  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger
        render={
          <Button variant="outline" size="sm">
            我的配置
          </Button>
        }
      />
      <DialogContent className="max-h-[90dvh] overflow-y-auto sm:max-w-2xl">
        <DialogHeader>
          <DialogTitle>我的配置</DialogTitle>
          <DialogDescription>{pluginId} · 连接与选项仅对当前账号可见。</DialogDescription>
        </DialogHeader>
        {open && space && <ConfigurationContent scope={{ spaceId: space.id, pluginId }} />}
      </DialogContent>
    </Dialog>
  )
}

function ConfigurationContent({ scope }: { scope: AgentConfigurationScope }) {
  const query = useAgentConfiguration(scope, true)
  async function readLatest(): Promise<AgentConfiguration> {
    const result = await query.refetch()
    if (result.error) throw result.error
    if (!result.data) throw new Error('agent_configuration_unavailable')
    return result.data
  }
  if (!query.data) {
    if (query.isError) return <p role="alert">{agentConfigurationFailure(query.error)}</p>
    return <p role="status">正在读取个人配置…</p>
  }
  return (
    <AgentConfigurationEditor
      scope={scope}
      context={query.data}
      onSaved={readLatest}
      onReload={readLatest}
    />
  )
}
