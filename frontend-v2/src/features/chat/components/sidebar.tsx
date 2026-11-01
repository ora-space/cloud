import {
  type CollisionDetection,
  DndContext,
  DragOverlay,
  KeyboardSensor,
  type Modifier,
  PointerSensor,
  closestCenter,
  pointerWithin,
  useSensor,
  useSensors,
} from '@dnd-kit/core'
import { SortableContext, sortableKeyboardCoordinates, useSortable, verticalListSortingStrategy } from '@dnd-kit/sortable'
import {
  ChevronRightIcon,
  FolderIcon,
  GripVerticalIcon,
  MoreHorizontalIcon,
  PanelLeftCloseIcon,
  PanelLeftOpenIcon,
  PencilIcon,
  PlusIcon,
  SquarePenIcon,
  Trash2Icon,
} from 'lucide-react'
import { useState } from 'react'
import { SidebarResizeHandle } from '@/components/sidebar-resize-handle'
import { Button } from '@/components/ui/button'
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from '@/components/ui/collapsible'
import { Dialog, DialogClose, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuSeparator, DropdownMenuTrigger } from '@/components/ui/dropdown-menu'
import { Input } from '@/components/ui/input'
import { cn } from '@/lib/utils'
import { isResponding } from '../model/conversation'
import { emptyConversation } from '../model/types'
import { type Project, type Session, useChatStore } from '../store'
import { AgentActivityDots } from './agent-activity-dots'
import { FadeText } from './fade-text'
import type { PublishedWorkflow } from '../workflow-run/server-runs'
import { ProjectRuns, RunWorkflowDialog, RunWorkflowSubmenu } from '../workflow-run/run-sidebar'
import { subtleScrollbar } from '@/lib/scrollbar'

/** A dragged project moves only up and down. */
const vertical: Modifier = ({ transform }) => ({ ...transform, x: 0 })

/** The project under the pointer, else the nearest one (keyboard drags have no pointer). */
const pointerFirst: CollisionDetection = (args) => {
  const hits = pointerWithin(args)
  return hits.length ? hits : closestCenter(args)
}

/**
 * Chat history grouped by project, each project listing its conversations.
 * Projects reorder by dragging the handle (or with the keyboard: focus it, space, arrows): only
 * the project's row follows the pointer, its sessions stay greyed in place, and a line marks
 * where it will land. The column's right edge drags to resize it, or to fold it away.
 * "新建对话", and the same item in each project's + menu, open the new chat view; a session shows
 * up here once its first prompt is sent. The + menu also runs a published workflow in the project.
 */
export function Sidebar() {
  const projects = useChatStore((s) => s.projects)
  const sidebar = useChatStore((s) => s.sidebar)
  const newChatOpen = useChatStore((s) => !s.selectedId && !s.selectedRunId)
  const { moveProject, setSidebar, startNewChat } = useChatStore.getState()
  const [activeId, setActiveId] = useState<string>()
  const [overId, setOverId] = useState<string>()
  const [scrolled, setScrolled] = useState(false)
  const sensors = useSensors(
    useSensor(PointerSensor, { activationConstraint: { distance: 4 } }),
    useSensor(KeyboardSensor, { coordinateGetter: sortableKeyboardCoordinates }),
  )
  const end = () => (setActiveId(undefined), setOverId(undefined))
  const index = (id?: string) => projects.findIndex((p) => p.id === id)
  const active = projects.find((p) => p.id === activeId)

  return (
    <>
      {sidebar.open && (
        <aside aria-label="聊天记录" style={{ width: sidebar.width }} className="flex shrink-0 flex-col border-r bg-muted/20">
          <div className="flex h-12 items-center justify-between pr-2 pl-4">
            <h1 className="text-sm font-semibold">对话</h1>
            <Button variant="ghost" size="icon-xs" aria-label="收起对话列表" onClick={() => setSidebar({ open: false })}>
              <PanelLeftCloseIcon />
            </Button>
          </div>
          {/* Stays put above the scrolling history; the rule shows once the list scrolls under it. */}
          <div className={cn('border-b px-2 pb-2 transition-colors', scrolled ? 'border-border' : 'border-transparent')}>
            <Button
              variant="ghost"
              // pl-5.5 lines the icon up with the projects' chevrons (after their drag handle).
              className="h-9 w-full justify-start gap-2 pr-2 pl-5.5 text-[13px] font-medium"
              aria-current={newChatOpen ? 'page' : undefined}
              onClick={() => startNewChat()}
            >
              <SquarePenIcon className="size-4 text-muted-foreground" />
              新建对话
            </Button>
          </div>
          <nav
            className={cn('flex min-h-0 flex-1 flex-col gap-2 overflow-y-auto px-2 pt-1 pb-4', subtleScrollbar)}
            onScroll={(e) => setScrolled(e.currentTarget.scrollTop > 0)}
          >
            <DndContext
              sensors={sensors}
              collisionDetection={pointerFirst}
              onDragStart={({ active }) => setActiveId(String(active.id))}
              onDragOver={({ over }) => setOverId(over ? String(over.id) : undefined)}
              onDragCancel={end}
              onDragEnd={({ active, over }) => {
                if (over && active.id !== over.id) moveProject(String(active.id), String(over.id))
                end()
              }}
            >
              <SortableContext items={projects.map((p) => p.id)} strategy={verticalListSortingStrategy}>
                {projects.map((p) => (
                  <ProjectGroup
                    key={p.id}
                    project={p}
                    // moveProject puts the project where the target was: below it when moving down.
                    dropLine={activeId && overId === p.id && overId !== activeId ? (index(overId) > index(activeId) ? 'after' : 'before') : undefined}
                  />
                ))}
              </SortableContext>
              <DragOverlay modifiers={[vertical]} dropAnimation={null} style={{ height: 'auto' }}>
                {active && <LiftedProject project={active} />}
              </DragOverlay>
            </DndContext>
          </nav>
        </aside>
      )}
      <SidebarResizeHandle label="调整对话列表宽度" layout={sidebar} onChange={setSidebar} />
    </>
  )
}

/** Brings the history column back after it was folded away; renders nothing while it shows. */
export function ShowSidebarButton() {
  const open = useChatStore((s) => s.sidebar.open)
  if (open) return null
  return (
    <Button variant="ghost" size="icon-sm" aria-label="展开对话列表" onClick={() => useChatStore.getState().setSidebar({ open: true })}>
      <PanelLeftOpenIcon />
    </Button>
  )
}

function ProjectGroup({ project, dropLine }: { project: Project; dropLine?: 'before' | 'after' }) {
  const sessions = useChatStore((s) => s.sessions).filter((s) => s.projectId === project.id)
  const startNewChat = useChatStore((s) => s.startNewChat)
  // Sortable for the drop targets and keyboard moves only: nothing shifts while dragging.
  const { attributes, listeners, setNodeRef, setActivatorNodeRef, isDragging } = useSortable({ id: project.id })
  const [runWorkflow, setRunWorkflow] = useState<PublishedWorkflow>()

  return (
    <section ref={setNodeRef} className="relative rounded-md" aria-label={`项目 ${project.name}`} data-dragging={isDragging || undefined}>
      {dropLine && (
        <div aria-hidden className={cn('absolute inset-x-1 h-0.5 rounded-full bg-primary', dropLine === 'before' ? '-top-[5px]' : '-bottom-[5px]')} />
      )}
      <Collapsible defaultOpen className={cn(isDragging && 'opacity-40 grayscale')}>
        <div className="group/project flex h-8 items-center gap-0.5 rounded-md pr-1 hover:bg-accent/60">
          <button
            ref={setActivatorNodeRef}
            type="button"
            aria-label={`拖动以调整 ${project.name} 的顺序`}
            className="flex h-full w-5 shrink-0 cursor-grab items-center justify-center text-muted-foreground opacity-0 group-hover/project:opacity-100 focus-visible:opacity-100 active:cursor-grabbing"
            {...attributes}
            {...listeners}
          >
            <GripVerticalIcon className="size-3.5" />
          </button>
          <CollapsibleTrigger className="group/trigger flex min-w-0 flex-1 items-center gap-1.5 text-left text-sm font-medium">
            <ChevronRightIcon className="size-3.5 shrink-0 text-muted-foreground transition-transform group-aria-expanded/trigger:rotate-90" />
            <FolderIcon className="size-4 shrink-0 text-muted-foreground" />
            <FadeText text={project.name} />
          </CollapsibleTrigger>
          <DropdownMenu>
            <DropdownMenuTrigger render={<Button variant="ghost" size="icon-xs" aria-label={`在 ${project.name} 新建对话`} />}>
              <PlusIcon />
            </DropdownMenuTrigger>
            <DropdownMenuContent align="end" className="w-52">
              <DropdownMenuItem onClick={() => startNewChat({ projectId: project.id })}>
                <SquarePenIcon />
                新建对话
              </DropdownMenuItem>
              <DropdownMenuSeparator />
              <RunWorkflowSubmenu onPick={setRunWorkflow} />
            </DropdownMenuContent>
          </DropdownMenu>
        </div>
        <CollapsibleContent>
          <div className="mt-0.5 mb-1 ml-[1.9rem] flex flex-col pl-2">
            {sessions.length === 0 && <p className="px-2 py-1 text-xs text-muted-foreground">还没有会话</p>}
            <ProjectRuns projectId={project.id} />
            {sessions.map((s) => (
              <SessionRow key={s.id} session={s} />
            ))}
          </div>
        </CollapsibleContent>
      </Collapsible>
      <RunWorkflowDialog projectId={project.id} workflow={runWorkflow} onClose={() => setRunWorkflow(undefined)} />
    </section>
  )
}

/** The project row that follows the pointer while the project is dragged. */
function LiftedProject({ project }: { project: Project }) {
  return (
    <div className="flex h-8 cursor-grabbing items-center gap-1.5 rounded-md bg-background pr-2 text-sm font-medium shadow-md ring-1 ring-border">
      <GripVerticalIcon className="ml-0.5 size-3.5 shrink-0 text-muted-foreground" />
      <ChevronRightIcon className="size-3.5 shrink-0 rotate-90 text-muted-foreground" />
      <FolderIcon className="size-4 shrink-0 text-muted-foreground" />
      <FadeText text={project.name} />
    </div>
  )
}

function SessionRow({ session }: { session: Session }) {
  const selected = useChatStore((s) => s.selectedId === session.id && !s.selectedRunId)
  const responding = useChatStore((s) => isResponding(s.conversations[session.id] ?? emptyConversation))
  const unread = useChatStore((s) => !!s.unread[session.id])
  const { select, renameSession, deleteSession } = useChatStore.getState()
  const [editing, setEditing] = useState(false)
  const [confirming, setConfirming] = useState(false)

  if (editing) {
    return (
      <form
        className="px-1 py-0.5"
        onSubmit={(e) => {
          e.preventDefault()
          renameSession(session.id, new FormData(e.currentTarget).get('title') as string)
          setEditing(false)
        }}
      >
        <Input
          name="title"
          aria-label="会话名称"
          defaultValue={session.title}
          autoFocus
          className="h-7 text-sm"
          onBlur={(e) => (renameSession(session.id, e.currentTarget.value), setEditing(false))}
          onKeyDown={(e) => e.key === 'Escape' && setEditing(false)}
        />
      </form>
    )
  }
  return (
    <div className={cn('group/session flex h-8 items-center rounded-md pr-1 text-sm', selected ? 'bg-accent text-accent-foreground' : 'hover:bg-accent/60')}>
      <button
        type="button"
        className="flex h-full min-w-0 flex-1 items-center gap-2 px-2 text-left"
        aria-current={selected ? 'page' : undefined}
        onClick={() => select(session.id)}
        onDoubleClick={() => setEditing(true)}
      >
        <FadeText text={session.title} className="flex-1" />
        {responding ? <AgentActivityDots label="回复中" className="text-muted-foreground" /> : unread && <span className="size-2 rounded-full bg-primary" aria-label="有新回复" />}
      </button>
      <DropdownMenu>
        <DropdownMenuTrigger
          render={<Button variant="ghost" size="icon-xs" className="opacity-0 group-hover/session:opacity-100 aria-expanded:opacity-100" aria-label={`${session.title} 的更多操作`} />}
        >
          <MoreHorizontalIcon />
        </DropdownMenuTrigger>
        <DropdownMenuContent align="end" className="w-40">
          <DropdownMenuItem onClick={() => setEditing(true)}>
            <PencilIcon />
            重命名
          </DropdownMenuItem>
          <DropdownMenuSeparator />
          <DropdownMenuItem variant="destructive" onClick={() => setConfirming(true)}>
            <Trash2Icon />
            删除
          </DropdownMenuItem>
        </DropdownMenuContent>
      </DropdownMenu>
      <Dialog open={confirming} onOpenChange={setConfirming}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>删除"{session.title}"？</DialogTitle>
            <DialogDescription>会话和它的聊天记录都会删除，无法恢复。</DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <DialogClose render={<Button variant="outline" />}>取消</DialogClose>
            <Button variant="destructive" onClick={() => (deleteSession(session.id), setConfirming(false))}>
              删除
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  )
}
