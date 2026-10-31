import { MoreHorizontalIcon, SearchIcon, Trash2Icon, WorkflowIcon } from 'lucide-react'
import { useQuery } from '@tanstack/react-query'
import { useState } from 'react'
import { Button } from '@/components/ui/button'
import { Dialog, DialogClose, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSub,
  DropdownMenuSubContent,
  DropdownMenuSubTrigger,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { subtleScrollbar } from '@/lib/scrollbar'
import { cn } from '@/lib/utils'
import { createHttpWorkflowContracts } from '@/lib/workflows/data/contracts-client'
import type { GraphWorkflowRun } from '@/lib/workflows/runtime'
import { FadeText } from '../components/fade-text'
import { useChatStore } from '../store'
import { createDemoRun, deleteDemoRun, listPublishedWorkflows, type PublishedWorkflow, useDemoRuns } from './demo-runs'
import { isNodeWorking, runStatusLabel, runStatusTone } from './run-status-style'

/**
 * The "运行工作流" submenu of a project's +, as in desktop: published workflows only, under a
 * search box, in a list that scrolls once it is long. Picking one opens the run dialog.
 */
export function RunWorkflowSubmenu({ onPick }: { onPick: (workflow: PublishedWorkflow) => void }) {
  const [open, setOpen] = useState(false)
  return (
    <DropdownMenuSub onOpenChange={setOpen}>
      <DropdownMenuSubTrigger>
        <WorkflowIcon />
        运行工作流
      </DropdownMenuSubTrigger>
      <DropdownMenuSubContent className="flex w-72 flex-col p-0">{open && <PublishedWorkflowPicker onPick={onPick} />}</DropdownMenuSubContent>
    </DropdownMenuSub>
  )
}

/**
 * The library changes on the workflow page, so it is read each time the submenu opens and
 * dropped when it closes: an earlier answer is never shown as the current one, and a failure
 * shows no list at all.
 */
function PublishedWorkflowPicker({ onPick }: { onPick: (workflow: PublishedWorkflow) => void }) {
  const [query, setQuery] = useState('')
  const tenantId = useChatStore((s) => s.tenantId)
  const published = useQuery({
    queryKey: ['workflow-run-menu', tenantId],
    queryFn: ({ signal }) => listPublishedWorkflows(createHttpWorkflowContracts(tenantId!), signal),
    enabled: !!tenantId,
    gcTime: 0,
    retry: false,
  })
  const workflows = published.data ?? []
  const shown = workflows.filter((w) => w.name.toLowerCase().includes(query.trim().toLowerCase()))
  const note = published.isError
    ? undefined
    : published.isPending
      ? '正在读取工作流…'
      : shown.length === 0
        ? workflows.length
          ? '没有匹配的工作流'
          : '还没有已发布的工作流'
        : undefined
  return (
    <>
      <div className="flex items-center gap-1.5 border-b px-2.5">
        <SearchIcon className="size-3.5 shrink-0 text-muted-foreground" />
        <input
          aria-label="搜索工作流"
          placeholder="搜索工作流"
          className="h-8 min-w-0 flex-1 bg-transparent text-sm outline-none"
          value={query}
          onChange={(e) => setQuery(e.currentTarget.value)}
          // Typing must not drive the menu's type-ahead or close it.
          onKeyDown={(e) => e.stopPropagation()}
        />
      </div>
      <div role="group" aria-label="已发布的工作流" className={cn('max-h-72 overflow-y-auto p-1', subtleScrollbar)}>
        {published.isError && (
          <div role="alert" className="flex items-center justify-between gap-2 px-2 py-1.5 text-xs text-destructive">
            读取工作流失败
            <Button size="xs" variant="outline" disabled={published.isFetching} onClick={() => void published.refetch()}>
              重试
            </Button>
          </div>
        )}
        {note && <p className="px-2 py-1.5 text-xs text-muted-foreground">{note}</p>}
        {published.isSuccess &&
          shown.map((w) => (
            <DropdownMenuItem key={w.id} title={w.name} onClick={() => onPick(w)}>
              <FadeText text={w.name} className="flex-1" />
              <span className="text-xs text-muted-foreground">{w.version}</span>
            </DropdownMenuItem>
          ))}
      </div>
    </>
  )
}

/**
 * Names the run before it is created, as desktop's run dialog does; the run then opens pending,
 * and its Start button asks for the workflow's inputs.
 */
export function RunWorkflowDialog({ projectId, workflow, onClose }: { projectId: string; workflow?: PublishedWorkflow; onClose: () => void }) {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string>()
  const selectRun = useChatStore((s) => s.selectRun)
  const tenantId = useChatStore((s) => s.tenantId)
  return (
    <Dialog open={!!workflow} onOpenChange={(open) => !open && !busy && (setError(undefined), onClose())}>
      <DialogContent>
        {workflow && (
          <form
            className="contents"
            onSubmit={async (e) => {
              e.preventDefault()
              const name = new FormData(e.currentTarget).get('name') as string
              setBusy(true)
              try {
                if (!tenantId) throw new Error('还没有读到团队，请稍后重试')
                const run = await createDemoRun(createHttpWorkflowContracts(tenantId), projectId, workflow, name)
                selectRun(run.id)
                setError(undefined)
                onClose()
              } catch (err) {
                setError(err instanceof Error ? err.message : '创建运行失败')
              } finally {
                setBusy(false)
              }
            }}
          >
            <DialogHeader>
              <DialogTitle>运行工作流</DialogTitle>
              <DialogDescription>
                在项目里创建「{workflow.name}」{workflow.version} 的一次运行。创建后点「启动」填写输入。
              </DialogDescription>
            </DialogHeader>
            <div className="grid gap-1.5">
              <Label htmlFor="run-name">运行名称</Label>
              <Input id="run-name" name="name" defaultValue={workflow.name} autoFocus />
            </div>
            {error && <p className="text-sm text-destructive">{error}</p>}
            <DialogFooter>
              <DialogClose render={<Button variant="outline" type="button" />}>取消</DialogClose>
              <Button type="submit" disabled={busy}>
                创建运行
              </Button>
            </DialogFooter>
          </form>
        )}
      </DialogContent>
    </Dialog>
  )
}

/** A project's workflow runs, newest first, listed with its sessions as desktop does. */
export function ProjectRuns({ projectId }: { projectId: string }) {
  const runs = useDemoRuns((s) => s.runs)
  const list = Object.values(runs)
    .filter((r) => r.projectId === projectId)
    .sort((a, b) => b.createdAt.localeCompare(a.createdAt))
  return list.map((run) => <RunRow key={run.id} run={run} />)
}

function RunRow({ run }: { run: GraphWorkflowRun }) {
  const selected = useChatStore((s) => s.selectedRunId === run.id)
  const selectRun = useChatStore((s) => s.selectRun)
  const [confirming, setConfirming] = useState(false)
  const tone = runStatusTone(run.status)
  const label = runStatusLabel(run.status)
  return (
    <div className={cn('group/run flex h-8 items-center rounded-md pr-1 text-sm', selected ? 'bg-accent text-accent-foreground' : 'hover:bg-accent/60')}>
      <button
        type="button"
        className="flex h-full min-w-0 flex-1 items-center gap-2 px-2 text-left"
        aria-current={selected ? 'page' : undefined}
        aria-label={`工作流运行 ${run.name}（${label}）`}
        onClick={() => selectRun(run.id)}
      >
        <WorkflowIcon className="size-3.5 shrink-0 text-muted-foreground" />
        <FadeText text={run.name} className="flex-1" />
        <span title={label} className={cn('size-2 shrink-0 rounded-full', tone.dot, isNodeWorking(run.status) && 'animate-pulse')} />
      </button>
      <DropdownMenu>
        <DropdownMenuTrigger
          render={<Button variant="ghost" size="icon-xs" className="opacity-0 group-hover/run:opacity-100 aria-expanded:opacity-100" aria-label={`${run.name} 的更多操作`} />}
        >
          <MoreHorizontalIcon />
        </DropdownMenuTrigger>
        <DropdownMenuContent align="end" className="w-40">
          <DropdownMenuItem variant="destructive" onClick={() => setConfirming(true)}>
            <Trash2Icon />
            删除
          </DropdownMenuItem>
        </DropdownMenuContent>
      </DropdownMenu>
      <Dialog open={confirming} onOpenChange={setConfirming}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>删除运行"{run.name}"？</DialogTitle>
            <DialogDescription>正在进行的运行会先停止，运行记录会一起删除。</DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <DialogClose render={<Button variant="outline" />}>取消</DialogClose>
            <Button
              variant="destructive"
              onClick={() => {
                if (selected) selectRun(undefined)
                void deleteDemoRun(run.id)
                setConfirming(false)
              }}
            >
              删除
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  )
}
