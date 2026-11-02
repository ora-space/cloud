import {
  Blocks,
  BookOpen,
  Bot,
  Monitor,
  MousePointer2,
  Plug,
  Puzzle,
  Workflow,
  Wrench,
  type LucideIcon,
} from 'lucide-react'
import type { PluginCatalogEntry } from '@/api/generated.schemas'

interface CatalogKind {
  label: string
  groupDescription: string
  icon: LucideIcon
}

const catalogKinds: Record<string, CatalogKind> = {
  agent: {
    label: 'Agent',
    groupDescription: '可直接运行任务的智能体',
    icon: Bot,
  },
  webview: {
    label: 'WebView',
    groupDescription: '嵌入工作空间的可交互界面',
    icon: Monitor,
  },
  skill: {
    label: '技能',
    groupDescription: '为 Agent 补充可复用的工作方法',
    icon: BookOpen,
  },
  mcp: {
    label: 'MCP',
    groupDescription: '连接外部工具与数据服务',
    icon: Plug,
  },
  workflow: {
    label: '工作流',
    groupDescription: '编排多个步骤与 Agent 协作',
    icon: Workflow,
  },
  hook: {
    label: 'Hooks',
    groupDescription: '扩展生命周期中的自动化行为',
    icon: MousePointer2,
  },
  workbench: {
    label: '工作台',
    groupDescription: '提供专用的工作空间界面',
    icon: Wrench,
  },
  pack: {
    label: '合集',
    groupDescription: '组合安装多个相关插件',
    icon: Blocks,
  },
}

export const pluginKindOrder = Object.keys(catalogKinds)

export function pluginKind(entry: Pick<PluginCatalogEntry, 'kind'>) {
  return (
    catalogKinds[entry.kind] ?? {
      label: entry.kind,
      groupDescription: '其他插件',
      icon: Puzzle,
    }
  )
}
