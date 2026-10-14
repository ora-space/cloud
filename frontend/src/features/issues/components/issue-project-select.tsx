import { useId } from 'react'
import { Label } from '@/components/ui/label'
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'
import { useProjects } from '@/features/projects/api'
import { useCurrentSpace } from '@/features/spaces/current-space'

function projectLabel({
  value,
  title,
  pending,
  failed,
}: {
  value: string
  title: string | undefined
  pending: boolean
  failed: boolean
}) {
  if (title !== undefined) return title
  if (value === 'none' || value === '') return '未关联项目'
  if (failed) return '已关联项目（列表加载失败）'
  if (pending) return '已关联项目（加载中）'
  return `已关联项目（${value}，当前空间不可用）`
}

/** Real-space project choice; an unresolved stored reference remains visible and is never cleared. */
export function IssueProjectSelect({
  value,
  onChange,
  disabled = false,
  id: suppliedId,
}: {
  value: string
  onChange: (value: string) => void
  disabled?: boolean
  id?: string
}) {
  const generatedId = useId()
  const id = suppliedId ?? generatedId
  const { space, isPending: scopePending } = useCurrentSpace()
  const projects = useProjects(space?.slug ?? '')
  const pending = scopePending || projects.isPending
  const selected = projects.data?.find((project) => project.id === value)
  const label = projectLabel({ value, title: selected?.title, pending, failed: projects.isError })
  return (
    <div className="space-y-1.5">
      <Label htmlFor={id}>项目</Label>
      <Select
        value={value}
        disabled={disabled || pending || projects.isError}
        onValueChange={(next) => {
          if (
            next !== null &&
            (next === 'none' || projects.data?.some((project) => project.id === next))
          )
            onChange(next)
        }}
      >
        <SelectTrigger id={id} className="w-full">
          <SelectValue>{label}</SelectValue>
        </SelectTrigger>
        <SelectContent alignItemWithTrigger={false}>
          <SelectItem value="none">未关联项目</SelectItem>
          {projects.data?.map((project) => (
            <SelectItem key={project.id} value={project.id}>
              {project.title}
            </SelectItem>
          ))}
        </SelectContent>
      </Select>
      {projects.isError && (
        <p role="alert" className="text-xs text-destructive">
          项目列表加载失败，关联状态已保留，请刷新后重试。
        </p>
      )}
    </div>
  )
}
