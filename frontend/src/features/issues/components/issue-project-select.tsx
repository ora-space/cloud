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

/** Optional project choice from the current real space; never substitutes demo projects. */
export function IssueProjectSelect({
  value,
  onChange,
}: {
  value: string
  onChange: (value: string) => void
}) {
  const id = useId()
  const { space } = useCurrentSpace()
  const projects = useProjects(space?.slug ?? '')
  const selected = projects.data?.find((project) => project.id === value)
  return (
    <div className="space-y-1.5">
      <Label htmlFor={id}>项目</Label>
      <Select
        value={value}
        onValueChange={(next) => {
          if (
            next !== null &&
            (next === 'none' || projects.data?.some((project) => project.id === next))
          )
            onChange(next)
        }}
      >
        <SelectTrigger id={id} className="w-full">
          <SelectValue>{selected?.title ?? '未关联项目'}</SelectValue>
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
        <p className="text-xs text-destructive">项目列表加载失败，可保留未关联项目</p>
      )}
    </div>
  )
}
