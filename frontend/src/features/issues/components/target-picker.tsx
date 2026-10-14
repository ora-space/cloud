import { AtSign } from 'lucide-react'
import { useState } from 'react'
import { ActorAvatar } from '@/components/common/actor-avatar'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { useCollaborationTargets } from '@/features/issues/api'
import type { CollaborationTargetSummary, CommentTargetInput } from '@/features/issues/types'

/** Compact mode labels shown beside each pickable target. */
const MODE_LABEL: Record<string, string> = {
  mention: '提及',
  task: '任务',
  form: '表单',
}

/** Avatar actor for a target; workflow never renders as a selectable actor. */
function targetAvatar(target: CollaborationTargetSummary) {
  if (target.type === 'agent' || target.type === 'team') {
    return { name: target.displayName, type: target.type }
  }
  return { name: target.displayName, type: 'user' as const }
}

/** One suggestion in the open picker; the mode label comes from the server descriptor. */
function SuggestionRow({
  target,
  onAdd,
}: {
  target: CollaborationTargetSummary
  onAdd: () => void
}) {
  return (
    <button
      type="button"
      onClick={onAdd}
      className="flex w-full items-center gap-2 rounded-md px-2 py-1.5 text-left text-sm hover:bg-muted"
    >
      <ActorAvatar actor={targetAvatar(target)} size="sm" />
      <span className="min-w-0 flex-1">
        <span className="block truncate font-medium">{target.displayName}</span>
        {target.description ? (
          <span className="block truncate text-xs text-muted-foreground">{target.description}</span>
        ) : null}
      </span>
      <span className="shrink-0 text-xs text-muted-foreground">
        {MODE_LABEL[target.interactionDescriptor.mode]}
      </span>
    </button>
  )
}

/**
 * The `@` collaboration picker. Selecting a target only **stages** it: the selected targets are handed
 * to the parent, which renders each one as an editable row. Nothing reaches the server from here —
 * a target is recorded only when its own submit (or a workflow's 确认执行) is pressed.
 *
 * Controlled component: the parent owns the staged targets, this widget only adds to them.
 */
export function TargetPicker({
  slug,
  targets,
  onChange,
}: {
  slug: string
  targets: CommentTargetInput[]
  onChange: (targets: CommentTargetInput[]) => void
}) {
  const query = useCollaborationTargets(slug)
  const available = query.data ?? []
  const [open, setOpen] = useState(false)
  const [search, setSearch] = useState('')

  const stagedIds = new Set(targets.map((target) => target.id))
  const term = search.trim().toLowerCase()
  const visible = available.filter(
    (target) => !stagedIds.has(target.id) && target.displayName.toLowerCase().includes(term),
  )

  function add(target: CollaborationTargetSummary) {
    // Form Mode carries no task; agent/team tasks and mention notes are typed in the staged row.
    onChange([...targets, { type: target.type, id: target.id }])
    setSearch('')
    setOpen(false)
  }

  return (
    <div className="space-y-1.5">
      {!open ? (
        <Button type="button" variant="outline" size="sm" onClick={() => setOpen(true)}>
          <AtSign className="size-4" />
          提及 / 任务
        </Button>
      ) : (
        <div className="rounded-md border p-1.5">
          <Input
            value={search}
            onChange={(event) => setSearch(event.target.value)}
            placeholder="搜索协作对象…"
            className="mb-1.5 h-7"
            autoFocus
          />
          {query.isPending && (
            <p role="status" className="px-2 py-1 text-xs text-muted-foreground">
              正在加载协作对象…
            </p>
          )}
          {query.isError && (
            <div className="space-y-1 px-2 py-1">
              <p role="alert" className="text-xs text-destructive">
                协作对象加载失败，请重试。
              </p>
              <Button
                type="button"
                variant="outline"
                size="sm"
                disabled={query.isFetching}
                onClick={() => {
                  void query.refetch()
                }}
              >
                重试加载
              </Button>
            </div>
          )}
          {!query.isPending && !query.isError && available.length === 0 && (
            <p className="px-2 py-1 text-xs text-muted-foreground">暂无协作对象。</p>
          )}
          {!query.isPending && !query.isError && available.length > 0 && visible.length === 0 && (
            <p className="px-2 py-1 text-xs text-muted-foreground">没有匹配的协作对象。</p>
          )}
          {!query.isError &&
            visible.map((target) => (
              <SuggestionRow key={target.id} target={target} onAdd={() => add(target)} />
            ))}
          <div className="mt-1 border-t pt-1">
            <Button
              type="button"
              variant="ghost"
              size="sm"
              className="w-full"
              onClick={() => setOpen(false)}
            >
              收起
            </Button>
          </div>
        </div>
      )}
    </div>
  )
}
