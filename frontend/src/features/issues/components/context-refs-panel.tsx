import { Plus, X } from 'lucide-react'
import { useState } from 'react'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'
import { useCreateContextRef, useDeleteContextRef, useContextRefs } from '@/features/issues/api'
import type { ContextRef } from '@/features/issues/types'

const REF_TYPE_OPTIONS: { value: ContextRef['refType']; label: string }[] = [
  { value: 'parent_issue', label: '父任务' },
  { value: 'run', label: '运行' },
  { value: 'timeline_message', label: '时间线消息' },
  { value: 'project', label: '项目' },
  { value: 'workspace', label: '工作区' },
  { value: 'acceptance_criteria', label: '验收标准' },
]

function refTypeLabel(refType: ContextRef['refType']): string {
  return REF_TYPE_OPTIONS.find((o) => o.value === refType)?.label ?? refType
}

/** Runtime guard so select values (strings) narrow safely into the refType union. */
function isRefType(value: unknown): value is ContextRef['refType'] {
  return typeof value === 'string' && REF_TYPE_OPTIONS.some((o) => o.value === value)
}

/**
 * List/add/remove of issue context refs — reference-only pointers, not hydrated
 * resources. No smart resolution: the ref id is stored and shown verbatim.
 */
export function ContextRefsPanel({ slug, issueId }: { slug: string; issueId: string }) {
  const query = useContextRefs(slug, issueId)
  const refs = query.isError
    ? undefined
    : query.data?.filter((ref) => ref.refType !== 'pull_request')
  const createRef = useCreateContextRef(slug, issueId)
  const deleteRef = useDeleteContextRef(slug, issueId)
  const [refType, setRefType] = useState<ContextRef['refType']>('parent_issue')
  const [refId, setRefId] = useState('')

  function add() {
    const id = refId.trim()
    if (!id) return
    createRef.mutate({ refType, refId: id }, { onSuccess: () => setRefId('') })
  }

  return (
    <div className="space-y-2">
      <div className="flex gap-1.5">
        <Select value={refType} onValueChange={(v) => isRefType(v) && setRefType(v)}>
          <SelectTrigger className="w-32">
            <SelectValue>
              {(value: unknown) => refTypeLabel(isRefType(value) ? value : refType)}
            </SelectValue>
          </SelectTrigger>
          <SelectContent>
            {REF_TYPE_OPTIONS.map((o) => (
              <SelectItem key={o.value} value={o.value}>
                {o.label}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
        <Input
          placeholder="引用 ID"
          value={refId}
          onChange={(e) => setRefId(e.target.value)}
          className="min-w-0 flex-1"
        />
        <Button
          size="icon"
          variant="ghost"
          className="size-9 shrink-0"
          onClick={add}
          disabled={!refId.trim() || createRef.isPending}
          aria-label="添加引用"
        >
          <Plus className="size-4" />
        </Button>
      </div>
      {query.isError && <p role="alert">上下文引用加载失败</p>}
      {refs && refs.length === 0 && <p className="text-xs text-muted-foreground">无上下文引用。</p>}
      {refs?.map((ref) => (
        <div key={ref.id} className="flex items-center gap-2 text-sm">
          <span className="shrink-0 text-xs text-muted-foreground">
            {refTypeLabel(ref.refType)}
          </span>
          <span className="min-w-0 flex-1 truncate font-mono text-xs">{ref.refId}</span>
          <Button
            size="icon"
            variant="ghost"
            className="size-6 shrink-0"
            onClick={() => deleteRef.mutate(ref.id)}
            aria-label="移除引用"
          >
            <X className="size-3.5" />
          </Button>
        </div>
      ))}
    </div>
  )
}
