import { useState } from 'react'
import {
  PRIORITY_ORDER,
  priorityLabelText,
  parseIssuePriority,
  statusLabelText,
} from '@/components/common/issue-badges'
import { Button } from '@/components/ui/button'
import {
  Dialog,
  DialogClose,
  DialogContent,
  DialogFooter,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
} from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'
import { Textarea } from '@/components/ui/textarea'
import { useCreateIssue, useIssues } from '@/features/issues/api'
import { IssueProjectSelect } from './issue-project-select'
import { columnLabel, issueNumber } from '@/features/issues/present'
import type { IssuePriority, IssueStatusColumn, TenantMember } from '@/features/issues/types'

/** Canonical status keys used before the catalog loads (or when it is empty). */
const FALLBACK_STATUS_KEYS = [
  'backlog',
  'todo',
  'in_progress',
  'in_review',
  'blocked',
  'done',
  'cancelled',
]

/** Stable defaults so optional props don't allocate a fresh array per render. */
const EMPTY_STATUSES: IssueStatusColumn[] = []
const EMPTY_MEMBERS: TenantMember[] = []

/** Creates a task with optional project association; each opening starts a fresh draft after submission. */
// oxlint-disable-next-line max-lines-per-function -- this dialog owns one cohesive create-issue form and its reset lifecycle.
export function CreateIssueDialog({
  slug,
  statuses = EMPTY_STATUSES,
  members = EMPTY_MEMBERS,
  defaultStatus = 'backlog',
  trigger,
}: {
  slug: string
  statuses?: IssueStatusColumn[]
  members?: TenantMember[]
  defaultStatus?: string
  trigger?: React.ReactElement
}) {
  const [open, setOpen] = useState(false)
  const [title, setTitle] = useState('')
  const [description, setDescription] = useState('')
  const [status, setStatus] = useState(defaultStatus)
  const [priority, setPriority] = useState<IssuePriority>('none')
  const [assigneeId, setAssigneeId] = useState<string>('none')
  const [parentIssueId, setParentIssueId] = useState<string>('none')
  const [projectRef, setProjectRef] = useState('none')
  const createIssue = useCreateIssue(slug)
  const { data: issues = [] } = useIssues(slug)

  const statusKeys = statuses.length > 0 ? statuses.map((s) => s.key) : FALLBACK_STATUS_KEYS

  function reset() {
    setTitle('')
    setDescription('')
    setStatus(defaultStatus)
    setPriority('none')
    setAssigneeId('none')
    setParentIssueId('none')
    setProjectRef('none')
  }

  function handleSubmit(e: React.FormEvent) {
    e.preventDefault()
    if (!title.trim()) return
    createIssue.mutate(
      {
        title,
        description,
        status,
        priority,
        ...(assigneeId !== 'none' ? { assigneeType: 'user' as const, assigneeId } : {}),
        ...(parentIssueId !== 'none' ? { parentIssueId } : {}),
        ...(projectRef !== 'none' ? { projectRef } : {}),
      },
      {
        onSuccess: () => {
          setOpen(false)
          reset()
        },
      },
    )
  }

  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger render={trigger ?? <Button size="sm">新建任务</Button>} />
      <DialogContent className="sm:max-w-lg">
        <form onSubmit={handleSubmit}>
          <DialogHeader>
            <DialogTitle>新建任务</DialogTitle>
          </DialogHeader>
          <div className="space-y-3 py-4">
            <Input
              placeholder="任务标题"
              value={title}
              onChange={(e) => setTitle(e.target.value)}
              autoFocus
              required
            />
            <Textarea
              placeholder="添加描述…"
              value={description}
              onChange={(e) => setDescription(e.target.value)}
              rows={3}
            />
            <IssueProjectSelect value={projectRef} onChange={setProjectRef} />
            <div className="flex flex-wrap gap-2">
              <Select
                value={status}
                onValueChange={(v) => {
                  if (v !== null) setStatus(v)
                }}
              >
                <SelectTrigger className="w-36">
                  <SelectValue>
                    {(value: unknown) =>
                      statusLabelText(typeof value === 'string' ? value : status)
                    }
                  </SelectValue>
                </SelectTrigger>
                <SelectContent>
                  {statusKeys.map((key) => (
                    <SelectItem key={key} value={key}>
                      {columnLabel(statuses, key)}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
              <Select
                value={priority}
                onValueChange={(v) => {
                  const nextPriority = parseIssuePriority(v)
                  if (nextPriority) setPriority(nextPriority)
                }}
              >
                <SelectTrigger className="w-36">
                  <SelectValue>
                    {(value: unknown) => priorityLabelText(parseIssuePriority(value) ?? priority)}
                  </SelectValue>
                </SelectTrigger>
                <SelectContent>
                  {PRIORITY_ORDER.map((pr) => (
                    <SelectItem key={pr} value={pr}>
                      {priorityLabelText(pr)}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
              <Select value={assigneeId} onValueChange={(v) => setAssigneeId(v ?? 'none')}>
                <SelectTrigger className="w-40">
                  <SelectValue placeholder="负责人">
                    {(value: unknown) =>
                      value === 'none'
                        ? '未分配'
                        : (members.find((m) => m.userId === value)?.displayName ?? '负责人')
                    }
                  </SelectValue>
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="none">未分配</SelectItem>
                  {members.map((m) => (
                    <SelectItem key={m.userId} value={m.userId}>
                      {m.displayName}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
            <Select value={parentIssueId} onValueChange={(v) => setParentIssueId(v ?? 'none')}>
              <SelectTrigger className="w-full">
                <SelectValue placeholder="父任务">
                  {(value: unknown) =>
                    value === 'none'
                      ? '无父任务'
                      : (issues.find((i) => i.id === value)?.title ?? '父任务')
                  }
                </SelectValue>
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="none">无父任务</SelectItem>
                {issues.map((i) => (
                  <SelectItem key={i.id} value={i.id}>
                    {issueNumber(i)} {i.title}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>
          <DialogFooter>
            <DialogClose
              render={
                <Button type="button" variant="outline">
                  取消
                </Button>
              }
            />
            <Button type="submit" disabled={!title.trim() || createIssue.isPending}>
              {createIssue.isPending ? '创建中…' : '创建任务'}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}
