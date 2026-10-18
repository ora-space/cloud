import { useState, type FormEvent } from 'react'
import type { GetApiV1TenantsTidAgentSessionsParams } from '@/api/generated.schemas'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { useSessionProjects } from './api'
import { sessionStatuses } from './presentation'

function formText(data: FormData, key: string): string {
  const value = data.get(key)
  return typeof value === 'string' ? value.trim() : ''
}

function formFilters(data: FormData): GetApiV1TenantsTidAgentSessionsParams {
  const result: GetApiV1TenantsTidAgentSessionsParams = {}
  for (const key of ['projectId', 'agent', 'status'] as const) {
    const value = formText(data, key)
    if (value) result[key] = value
  }
  for (const key of ['from', 'to'] as const) {
    const value = formText(data, key)
    if (value) result[key] = new Date(value).toISOString()
  }
  return result
}

/** Server-side filter form; date bounds use the browser's local timezone. */
export function SessionFilters({
  tid,
  onApply,
}: {
  tid: string
  onApply: (value: GetApiV1TenantsTidAgentSessionsParams) => void
}) {
  const projects = useSessionProjects(tid)
  const [error, setError] = useState('')
  const submit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault()
    const filters = formFilters(new FormData(event.currentTarget))
    if (filters.from && filters.to && filters.from >= filters.to) {
      setError('结束时间必须晚于开始时间')
      return
    }
    setError('')
    onApply(filters)
  }
  return (
    <form onSubmit={submit} className="rounded-xl border bg-card p-4">
      <div className="grid gap-3 sm:grid-cols-2 xl:grid-cols-5">
        <label className="space-y-1 text-xs">
          项目
          <select
            name="projectId"
            disabled={projects.isPending}
            className="h-9 w-full rounded-md border bg-background px-2"
          >
            <option value="">全部项目</option>
            {projects.data?.map((project) => (
              <option key={project.id} value={project.id}>
                {project.name}
              </option>
            ))}
          </select>
        </label>
        <label className="space-y-1 text-xs">
          Agent 插件
          <Input name="agent" placeholder="如 official/ora-space.opencode" />
        </label>
        <label className="space-y-1 text-xs">
          运行状态
          <select name="status" className="h-9 w-full rounded-md border bg-background px-2">
            <option value="">全部状态</option>
            {Object.entries(sessionStatuses).map(([value, label]) => (
              <option key={value} value={value}>
                {label}
              </option>
            ))}
          </select>
        </label>
        <label className="space-y-1 text-xs">
          开始时间（含）
          <Input name="from" type="datetime-local" />
        </label>
        <label className="space-y-1 text-xs">
          结束时间（不含）
          <Input name="to" type="datetime-local" />
        </label>
      </div>
      {projects.isError && (
        <p className="mt-2 text-xs text-destructive">
          项目选项加载失败。
          <button type="button" onClick={() => void projects.refetch()} className="underline">
            重试
          </button>
        </p>
      )}
      <div className="mt-3 flex items-center gap-2">
        <Button size="sm" type="submit">
          应用筛选
        </Button>
        <Button
          size="sm"
          variant="ghost"
          type="reset"
          onClick={() => {
            setError('')
            onApply({})
          }}
        >
          重置
        </Button>
        {error && (
          <p role="alert" className="text-xs text-destructive">
            {error}
          </p>
        )}
      </div>
    </form>
  )
}
