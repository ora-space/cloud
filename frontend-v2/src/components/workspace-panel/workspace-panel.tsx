import { type UseQueryResult, useQuery, useQueryClient } from '@tanstack/react-query'
import { ArrowLeftIcon, ChevronRightIcon, FileIcon, FileTextIcon, FolderIcon, GitCompareArrowsIcon, PanelRightIcon, RefreshCwIcon, XIcon } from 'lucide-react'
import { type ReactNode, useEffect, useMemo, useRef, useState } from 'react'
import { Accordion, AccordionContent, AccordionItem, AccordionTrigger } from '@/components/ui/accordion'
import { Button } from '@/components/ui/button'
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from '@/components/ui/collapsible'
import { useHighlightedCode } from '@/hooks/use-highlighted-code'
import { codeOf } from '@/lib/api-client'
import { subtleScrollbar } from '@/lib/scrollbar'
import { cn } from '@/lib/utils'
import type { WorkspaceChange, WorkspaceChanges, WorkspaceEntry } from '@/lib/workspace-api/generated'
import { parsePatch } from './patch'
import { languageOf, relativePath, type WorkspaceReader, type WorkspaceSource, workspaceReader } from './source'

type Tab = 'changes' | 'files'

/** A request from outside the panel to show one file's diff; a new nonce repeats the same path. */
export interface OpenRequest {
  path: string
  nonce: number
}

/**
 * A side panel that shows an Agent's checkout read-only, as desktop's review pane does: what
 * changed against the commit it started from, each file's diff expandable in place, and every
 * file. The page bumps `revision` whenever the Agent may have written (a tool call finished, a
 * turn or node ended), and the panel reads the checkout again.
 */
export function WorkspacePanel({
  source,
  revision,
  request,
  onClose,
}: {
  source: WorkspaceSource
  revision: string
  request?: OpenRequest
  onClose: () => void
}) {
  const reader = useMemo(() => workspaceReader(source), [source])
  const queryClient = useQueryClient()
  const [tab, setTab] = useState<Tab>('changes')
  /** The file shown in full, over either tab. */
  const [file, setFile] = useState<string | null>(null)
  /** Changed files whose diff is expanded; several can be open at once. */
  const [expanded, setExpanded] = useState<string[]>([])
  const [reveal, setReveal] = useState<string | null>(null)
  const changes = useQuery({ queryKey: [...reader.key, 'changes'], queryFn: reader.changes, retry: false })

  useEffect(() => {
    void queryClient.invalidateQueries({ queryKey: reader.key })
  }, [queryClient, reader, revision])

  const showDiff = (path: string) => {
    setTab('changes')
    setFile(null)
    setExpanded((open) => (open.includes(path) ? open : [...open, path]))
    setReveal(path)
  }
  const [handled, setHandled] = useState<number | undefined>(undefined)
  if (request && request.nonce !== handled) {
    setHandled(request.nonce)
    showDiff(relativePath(request.path, changes.data?.files))
  }

  const listing = !file
  return (
    <aside aria-label="工作区" className="flex h-full min-h-0 w-full flex-col bg-background">
      <header className="flex h-12 shrink-0 items-center gap-1.5 border-b px-3">
        <div role="group" aria-label="工作区视图" className="inline-flex rounded-lg border p-0.5">
          {(['changes', 'files'] as const).map((t) => (
            <Button
              key={t}
              size="sm"
              variant={tab === t && listing ? 'secondary' : 'ghost'}
              className="h-7 gap-1.5 px-2.5 text-xs"
              aria-pressed={tab === t && listing}
              onClick={() => {
                setTab(t)
                setFile(null)
              }}
            >
              {t === 'changes' ? <GitCompareArrowsIcon className="size-3.5" /> : <FolderIcon className="size-3.5" />}
              {t === 'changes' ? '变更' : '文件'}
              {t === 'changes' && changes.data && changes.data.files.length > 0 && (
                <span className="font-mono text-[10px] text-muted-foreground tabular-nums">{changes.data.files.length}</span>
              )}
            </Button>
          ))}
        </div>
        <div className="ml-auto flex items-center gap-0.5">
          <Button size="icon-sm" variant="ghost" aria-label="刷新" onClick={() => void queryClient.invalidateQueries({ queryKey: reader.key })}>
            <RefreshCwIcon />
          </Button>
          <Button size="icon-sm" variant="ghost" aria-label="关闭工作区" onClick={onClose}>
            <XIcon />
          </Button>
        </div>
      </header>
      <div className="flex min-h-0 flex-1 flex-col">
        {file ? (
          <FileView
            reader={reader}
            path={file}
            changed={changes.data?.files.some((c) => c.path === file && c.status !== 'deleted') ?? false}
            onBack={() => setFile(null)}
            onShowChanges={() => showDiff(file)}
          />
        ) : tab === 'changes' ? (
          <ChangesList
            reader={reader}
            query={changes}
            expanded={expanded}
            onExpanded={setExpanded}
            reveal={reveal}
            onRevealed={() => setReveal(null)}
            onOpenFile={setFile}
          />
        ) : (
          <div className={cn('min-h-0 flex-1 overflow-y-auto', subtleScrollbar)}>
            <div role="tree" aria-label="工作区文件" className="flex flex-col p-2 text-sm">
              <Directory reader={reader} path="" depth={0} onOpen={setFile} />
            </div>
          </div>
        )}
      </div>
    </aside>
  )
}

/** The header button that shows or hides a page's workspace panel. */
export function WorkspaceToggle({ open, toggle }: { open: boolean; toggle: () => void }) {
  return (
    <Button size="sm" variant={open ? 'secondary' : 'ghost'} className="h-7 gap-1.5 px-2 text-xs" aria-pressed={open} aria-label="工作区" onClick={toggle}>
      <PanelRightIcon className="size-3.5" />
      <span className="hidden sm:inline">工作区</span>
    </Button>
  )
}

/** What a failed read means to the person, by the server's error code. */
function failureText(error: unknown): string {
  switch (codeOf(error)) {
    case 'workspace_not_ready':
      return 'Agent 还没有开始工作，工作区在第一轮开始后才会准备好。'
    case 'workspace_removed':
      return '这个工作区已被清理，无法再查看。'
    case 'binary_file':
      return '这是二进制文件，无法预览。'
    case 'too_large':
      return '内容太大，无法预览。'
    case 'path_not_found':
      return '文件不存在，可能已被删除。'
    case 'invalid_path':
      return '这个路径不在工作区内。'
    default:
      return '读取失败。'
  }
}

function Message({ children, error, compact, onRetry }: { children: ReactNode; error?: boolean; compact?: boolean; onRetry?: () => void }) {
  return (
    <div
      role={error ? 'alert' : 'status'}
      className={cn('flex flex-col items-center gap-3 text-center text-muted-foreground', compact ? 'px-3 py-3 text-xs' : 'px-6 py-10 text-sm')}
    >
      <p>{children}</p>
      {onRetry && (
        <Button size="sm" variant="outline" onClick={onRetry}>
          重试
        </Button>
      )}
    </div>
  )
}

/** Codes that describe the workspace, not a failure worth retrying. */
const settled = new Set(['workspace_not_ready', 'workspace_removed', 'binary_file', 'too_large', 'path_not_found', 'invalid_path'])

function Failure({ error, compact, onRetry }: { error: unknown; compact?: boolean; onRetry: () => void }) {
  const code = codeOf(error)
  return (
    <Message compact={compact} error={!code || !settled.has(code)} onRetry={code && settled.has(code) ? undefined : onRetry}>
      {failureText(error)}
    </Message>
  )
}

const statusMark: Record<WorkspaceChange['status'], { letter: string; label: string; className: string }> = {
  added: { letter: 'A', label: '新增', className: 'text-success' },
  modified: { letter: 'M', label: '修改', className: 'text-warning' },
  deleted: { letter: 'D', label: '删除', className: 'text-destructive' },
}

/** Every changed file as an accordion row; expanding one shows its diff, and several can be open. */
function ChangesList({
  reader,
  query,
  expanded,
  onExpanded,
  reveal,
  onRevealed,
  onOpenFile,
}: {
  reader: WorkspaceReader
  query: UseQueryResult<WorkspaceChanges>
  expanded: string[]
  onExpanded: (paths: string[]) => void
  reveal: string | null
  onRevealed: () => void
  onOpenFile: (path: string) => void
}) {
  const scroller = useRef<HTMLDivElement>(null)
  const files = query.data?.files
  useEffect(() => {
    if (!reveal || !files) return
    const row = [...(scroller.current?.querySelectorAll<HTMLElement>('[data-change-path]') ?? [])].find((el) => el.dataset.changePath === reveal)
    row?.scrollIntoView?.({ block: 'start' })
    onRevealed()
  }, [reveal, files, onRevealed])

  if (query.isPending) return <Message>正在读取变更…</Message>
  if (query.isError) return <Failure error={query.error} onRetry={() => void query.refetch()} />
  const { truncated } = query.data
  if (query.data.files.length === 0) return <Message>还没有文件改动。</Message>
  const added = query.data.files.reduce((n, c) => n + c.additions, 0)
  const removed = query.data.files.reduce((n, c) => n + c.deletions, 0)
  return (
    <div ref={scroller} className={cn('min-h-0 flex-1 overflow-y-auto', subtleScrollbar)}>
      <p className="px-3 py-2 text-xs text-muted-foreground">
        相对初始版本改动了 {query.data.files.length} 个文件 <span className="text-success">+{added}</span> <span className="text-destructive">−{removed}</span>
      </p>
      <Accordion multiple value={expanded} onValueChange={(value) => onExpanded(value as string[])} aria-label="改动的文件" className="border-t">
        {query.data.files.map((c) => {
          const name = c.path.split('/').at(-1)
          const dir = c.path.slice(0, -(name?.length ?? 0)).replace(/\/$/, '')
          const mark = statusMark[c.status]
          return (
            <AccordionItem key={c.path} value={c.path} data-change-path={c.path}>
              <AccordionTrigger
                title={c.path}
                className="min-w-0 items-center gap-2 rounded-none px-3 py-1.5 font-normal hover:bg-muted hover:no-underline focus-visible:ring-inset **:data-[slot=accordion-trigger-icon]:ml-0 **:data-[slot=accordion-trigger-icon]:size-3.5"
              >
                <span className={cn('w-3 shrink-0 font-mono text-[11px] font-semibold', mark.className)} aria-label={mark.label}>
                  {mark.letter}
                </span>
                <span className={cn('shrink-0 font-mono text-xs', c.status === 'deleted' && 'line-through')}>{name}</span>
                {dir && <span className="min-w-0 truncate font-mono text-[11px] text-muted-foreground">{dir}</span>}
                <span className="ml-auto shrink-0 font-mono text-[11px] tabular-nums">
                  {c.binary ? (
                    <span className="text-muted-foreground">二进制</span>
                  ) : (
                    <>
                      <span className="text-success">+{c.additions}</span> <span className="text-destructive">−{c.deletions}</span>
                    </>
                  )}
                </span>
              </AccordionTrigger>
              <AccordionContent className="pb-0">
                <DiffContent reader={reader} change={c} onOpenFile={onOpenFile} />
              </AccordionContent>
            </AccordionItem>
          )
        })}
      </Accordion>
      {truncated && <p className="px-3 py-2 text-xs text-muted-foreground">改动太多，只列出了前 {query.data.files.length} 个文件。</p>}
    </div>
  )
}

/** One file's diff, read when its row first opens, drawn straight into the row without a frame. */
function DiffContent({ reader, change, onOpenFile }: { reader: WorkspaceReader; change: WorkspaceChange; onOpenFile: (path: string) => void }) {
  const query = useQuery({ queryKey: [...reader.key, 'diff', change.path], queryFn: () => reader.diff(change.path), retry: false })
  const parsed = useMemo(() => (query.data ? parsePatch(query.data.patch) : undefined), [query.data])
  let body: ReactNode
  if (query.isPending) body = <Message compact>正在读取改动…</Message>
  else if (query.isError) body = <Failure compact error={query.error} onRetry={() => void query.refetch()} />
  else if (parsed!.binary) body = <Message compact>二进制文件有改动，无法预览。</Message>
  else if (parsed!.lines.length === 0) body = <Message compact>这个文件和初始版本相同。</Message>
  else {
    body = (
      <div role="region" aria-label={`${change.path} 的改动`} className="font-mono text-[11px] leading-5">
        {parsed!.lines.map((l, i) => (
          <div
            key={i}
            className={cn(
              'flex px-3',
              l.op === '+' && 'bg-success-soft',
              l.op === '-' && 'bg-destructive/10',
              l.op === '@' && 'bg-muted/60 text-muted-foreground',
            )}
          >
            <span className={cn('w-4 shrink-0 select-none', l.op === '+' ? 'text-success' : l.op === '-' ? 'text-destructive' : 'text-muted-foreground')}>
              {l.op === '+' || l.op === '-' ? l.op : ''}
            </span>
            <span className="min-w-0 flex-1 break-all whitespace-pre-wrap">{l.text || ' '}</span>
          </div>
        ))}
      </div>
    )
  }
  return (
    <div className="border-t">
      {change.status !== 'deleted' && (
        <div className="flex justify-end px-2 py-1">
          <Button size="sm" variant="ghost" className="h-6 px-2 text-xs text-muted-foreground" onClick={() => onOpenFile(change.path)}>
            <FileTextIcon />
            查看文件
          </Button>
        </div>
      )}
      {body}
    </div>
  )
}

/** One directory level, read when it is first shown; folders below load as they open. */
function Directory({ reader, path, depth, onOpen }: { reader: WorkspaceReader; path: string; depth: number; onOpen: (path: string) => void }) {
  const query = useQuery({ queryKey: [...reader.key, 'entries', path], queryFn: () => reader.entries(path), retry: false })
  const indent = { paddingLeft: `${depth * 14 + 4}px` }
  if (query.isPending) {
    return (
      <p className="py-1 text-xs text-muted-foreground" style={indent}>
        正在读取…
      </p>
    )
  }
  if (query.isError) {
    return depth === 0 ? (
      <Failure error={query.error} onRetry={() => void query.refetch()} />
    ) : (
      <p className="py-1 text-xs text-destructive" style={indent}>
        {failureText(query.error)}
      </p>
    )
  }
  if (depth === 0 && query.data.entries.length === 0) return <Message>工作区是空的。</Message>
  return (
    <>
      {query.data.entries.map((e) => (
        <EntryRow key={e.path} entry={e} reader={reader} depth={depth} onOpen={onOpen} />
      ))}
      {query.data.truncated && (
        <p className="py-1 text-xs text-muted-foreground" style={indent}>
          目录太大，只列出了一部分。
        </p>
      )}
    </>
  )
}

function EntryRow({ entry, reader, depth, onOpen }: { entry: WorkspaceEntry; reader: WorkspaceReader; depth: number; onOpen: (path: string) => void }) {
  const indent = { paddingLeft: `${depth * 14 + 4}px` }
  if (entry.kind === 'file') {
    return (
      <button
        type="button"
        role="treeitem"
        className="flex w-full min-w-0 items-center gap-1.5 rounded py-0.5 pr-1 text-left outline-none hover:bg-muted focus-visible:bg-muted"
        style={{ paddingLeft: `${depth * 14 + 22}px` }}
        onClick={() => onOpen(entry.path)}
      >
        <FileIcon className="size-3.5 shrink-0 text-muted-foreground" />
        <span className="truncate font-mono text-xs">{entry.name}</span>
      </button>
    )
  }
  return (
    <Collapsible>
      <CollapsibleTrigger
        role="treeitem"
        className="group/folder flex w-full min-w-0 items-center gap-1.5 rounded py-0.5 pr-1 text-left outline-none hover:bg-muted focus-visible:bg-muted"
        style={indent}
      >
        <ChevronRightIcon className="size-3.5 shrink-0 transition-transform group-aria-expanded/folder:rotate-90" />
        <FolderIcon className="size-3.5 shrink-0 text-muted-foreground" />
        <span className="truncate font-mono text-xs">{entry.name}</span>
      </CollapsibleTrigger>
      <CollapsibleContent>
        <Directory reader={reader} path={entry.path} depth={depth + 1} onOpen={onOpen} />
      </CollapsibleContent>
    </Collapsible>
  )
}

/** One file's current content, filling the panel below its path, without a frame. */
function FileView({
  reader,
  path,
  changed,
  onBack,
  onShowChanges,
}: {
  reader: WorkspaceReader
  path: string
  changed: boolean
  onBack: () => void
  onShowChanges: () => void
}) {
  const query = useQuery({ queryKey: [...reader.key, 'file', path], queryFn: () => reader.file(path), retry: false })
  return (
    <>
      <div className="flex min-w-0 shrink-0 items-center gap-1.5 border-b px-2 py-1">
        <Button size="icon-sm" variant="ghost" aria-label="返回列表" onClick={onBack}>
          <ArrowLeftIcon />
        </Button>
        <span className="min-w-0 flex-1 truncate font-mono text-xs" title={path}>
          {path}
        </span>
        {changed && (
          <Button size="sm" variant="ghost" className="h-7 px-2 text-xs" onClick={onShowChanges}>
            <GitCompareArrowsIcon />
            查看改动
          </Button>
        )}
      </div>
      {query.isPending ? (
        <Message>正在读取文件…</Message>
      ) : query.isError ? (
        <Failure error={query.error} onRetry={() => void query.refetch()} />
      ) : query.data.content === '' ? (
        <Message>空文件。</Message>
      ) : (
        <FileText path={path} content={query.data.content} />
      )}
    </>
  )
}

function FileText({ path, content }: { path: string; content: string }) {
  const lines = useHighlightedCode(content, languageOf(path))
  return (
    <div role="region" aria-label={`${path} 的内容`} className={cn('min-h-0 flex-1 overflow-auto py-2 font-mono text-xs leading-5', subtleScrollbar)}>
      <div className="grid min-w-max grid-cols-[auto_1fr]">
        {lines.map((line, i) => (
          <div key={i} className="contents">
            <span className="select-none px-3 text-right text-muted-foreground/60 tabular-nums">{i + 1}</span>
            <span className="pr-4 whitespace-pre">{line}</span>
          </div>
        ))}
      </div>
    </div>
  )
}
