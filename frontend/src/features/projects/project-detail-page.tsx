import { RepositoryPanel } from '@/features/repositories/repository-panel'
import { format } from 'date-fns'
import { useState } from 'react'
import { useNavigate, useParams } from 'react-router-dom'
import { ActorAvatar } from '@/components/common/actor-avatar'
import { PageHeader } from '@/components/layout/page-header'
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
  AlertDialogTrigger,
} from '@/components/ui/alert-dialog'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Skeleton } from '@/components/ui/skeleton'
import { ProjectIcon } from '@/features/projects/components/project-icon'
import {
  useCloudProject,
  useDeleteProject,
  useProject,
  useUpdateProject,
} from '@/features/projects/api'
import { PROJECT_STATUS_LABELS, PROJECT_STATUS_VARIANT } from '@/features/projects/status'
import { useIssues, useMembers } from '@/features/issues/api'
import { IssueRow } from '@/features/issues/components/issue-row'
import { memberNameById } from '@/features/issues/present'
import { normalizeSpaceRole, type SpaceRole } from '@/features/spaces/api'
import { useCurrentSpace } from '@/features/spaces/current-space'
import { workspacePaths } from '@/lib/paths'
import { actorById, db } from '@/mocks/data/store'
import type { Project } from '@/mocks/data/types'

export function ProjectDetailPage({ slug }: { slug: string }) {
  const { projectId } = useParams<{ projectId: string }>()
  const { data: project, isPending } = useProject(slug, projectId)
  const { tenantId, space } = useCurrentSpace()
  const cloudMode = space?.slug === slug
  const detail = useCloudProject(tenantId, cloudMode ? projectId : undefined)
  const [renameOpen, setRenameOpen] = useState(false)
  const [renameDraft, setRenameDraft] = useState('')
  const role = cloudMode ? normalizeSpaceRole(space.role) : 'member'
  const version = detail.data?.version ?? 0

  return (
    <div className="flex h-full flex-col">
      <ProjectChrome
        slug={slug}
        projectId={projectId}
        project={project}
        cloudMode={cloudMode}
        role={role}
        version={version}
        renameOpen={renameOpen}
        renameDraft={renameDraft}
        onRenameDraftChange={setRenameDraft}
        onRenameOpenChange={setRenameOpen}
      />
      {isPending || !project ? (
        <div className="space-y-3 p-6">
          <Skeleton className="h-6 w-1/2" />
          <Skeleton className="h-20 w-full" />
        </div>
      ) : (
        <ProjectDetailBody slug={slug} project={project} />
      )}
    </div>
  )
}

/**
 * Page header with the rename/delete actions plus the rename dialog, extracted
 * so the cloud-mode gating does not inflate ProjectDetailPage's complexity.
 */
function ProjectChrome({
  slug,
  projectId,
  project,
  cloudMode,
  role,
  version,
  renameOpen,
  renameDraft,
  onRenameDraftChange,
  onRenameOpenChange,
}: {
  slug: string
  projectId: string | undefined
  project: Project | undefined
  cloudMode: boolean
  role: SpaceRole
  version: number
  renameOpen: boolean
  renameDraft: string
  onRenameDraftChange: (title: string) => void
  onRenameOpenChange: (open: boolean) => void
}) {
  const p = workspacePaths(slug)
  return (
    <>
      <PageHeader
        title={project?.title ?? '项目'}
        breadcrumb={{ label: '项目', to: p.projects }}
        actions={
          cloudMode &&
          project && (
            <ProjectActions
              slug={slug}
              projectId={project.id}
              projectTitle={project.title}
              version={version}
              canDelete={canDeleteProject(role)}
              onRename={() => {
                onRenameDraftChange(project.title)
                onRenameOpenChange(true)
              }}
            />
          )
        }
      />
      {cloudMode && projectId && project && (
        <RenameProjectDialog
          projectId={projectId}
          title={renameDraft}
          onTitleChange={onRenameDraftChange}
          version={version}
          open={renameOpen}
          onOpenChange={onRenameOpenChange}
        />
      )}
    </>
  )
}

/** True for roles allowed to delete projects in the space. */
function canDeleteProject(role: SpaceRole): boolean {
  return role === 'admin'
}

/**
 * Project header, status line and the attached-issue list below the page
 * chrome. The cloud issues list has no project filter (the backend only takes a
 * free-text query), so cloud mode filters the tenant board client-side by
 * `projectRef`; demo mode reads the mock store's per-project issues.
 */
function ProjectDetailBody({ slug, project }: { slug: string; project: Project }) {
  const { tenantId } = useCurrentSpace()
  const cloudMode = tenantId != null
  const { data: issues = [] } = useIssues(tenantId ?? '', '')
  const { data: members = [] } = useMembers(tenantId ?? '')
  const memberNames = memberNameById(members)
  const cloudIssues = issues.filter((issue) => issue.projectRef === project.id)
  const mockIssues = db.issues.filter((issue) => issue.projectId === project.id)
  const lead = actorById(project.leadId)

  return (
    <div className="flex-1 overflow-y-auto">
      <div className="space-y-3 border-b p-6">
        <div className="flex items-center gap-2">
          <ProjectIcon project={project} />
          <h1 className="text-xl font-semibold">{project.title}</h1>
        </div>
        <p className="text-sm text-muted-foreground">{project.description}</p>
        <div className="flex flex-wrap items-center gap-4 pt-2 text-sm">
          <Badge variant={PROJECT_STATUS_VARIANT[project.status]}>
            {PROJECT_STATUS_LABELS[project.status]}
          </Badge>
          <div className="flex items-center gap-1.5 text-muted-foreground">
            <ActorAvatar actor={lead} size="sm" />
            {lead?.name}
          </div>
          {project.targetDate && (
            <span className="text-muted-foreground">
              目标日期：{format(new Date(project.targetDate), 'yyyy年M月d日')}
            </span>
          )}
        </div>
      </div>
      {cloudMode && <RepositoryPanel projectId={project.id} />}
      {cloudMode ? (
        <div>
          {cloudIssues.length === 0 && (
            <p className="p-8 text-center text-sm text-muted-foreground">该项目下暂无任务。</p>
          )}
          {cloudIssues.map((issue) => (
            <IssueRow key={issue.id} issue={issue} slug={slug} members={memberNames} />
          ))}
        </div>
      ) : (
        <div>
          {mockIssues.length === 0 && (
            <p className="p-8 text-center text-sm text-muted-foreground">该项目下暂无任务。</p>
          )}
          {mockIssues.map((issue) => (
            <div key={issue.id} className="flex items-center gap-2 border-b px-6 py-2.5 text-sm">
              <span className="shrink-0 text-xs text-muted-foreground">{issue.identifier}</span>
              <span className="truncate">{issue.title}</span>
            </div>
          ))}
        </div>
      )}
    </div>
  )
}

/** Rename button plus the admin/owner delete confirmation. */
function ProjectActions({
  slug,
  projectId,
  projectTitle,
  version,
  canDelete,
  onRename,
}: {
  slug: string
  projectId: string
  projectTitle: string
  version: number
  canDelete: boolean
  onRename: () => void
}) {
  const navigate = useNavigate()
  const p = workspacePaths(slug)
  const deleteProject = useDeleteProject()

  async function confirmDelete() {
    try {
      await deleteProject.mutateAsync({ id: projectId, version })
      void navigate(p.projects)
    } catch {
      // the error code is rendered by the dialog
    }
  }

  return (
    <div className="flex items-center gap-2">
      <Button size="sm" variant="outline" onClick={onRename}>
        重命名
      </Button>
      {canDelete && (
        <AlertDialog>
          <AlertDialogTrigger
            render={
              <Button size="sm" variant="destructive">
                删除项目
              </Button>
            }
          />
          <AlertDialogContent>
            <AlertDialogHeader>
              <AlertDialogTitle>删除「{projectTitle}」？</AlertDialogTitle>
              <AlertDialogDescription>
                删除会异步回收运行资源，可能需要等待正在核验的发布结束。
              </AlertDialogDescription>
            </AlertDialogHeader>
            <AlertDialogFooter>
              <AlertDialogCancel>取消</AlertDialogCancel>
              <AlertDialogAction onClick={() => void confirmDelete()}>确认删除</AlertDialogAction>
            </AlertDialogFooter>
            {deleteProject.error?.response?.data?.code && (
              <p className="text-xs text-destructive">
                删除失败：{deleteProject.error?.response?.data?.code}
              </p>
            )}
          </AlertDialogContent>
        </AlertDialog>
      )}
    </div>
  )
}

/** Rename dialog guarded by the cloud optimistic version. */
function RenameProjectDialog({
  projectId,
  title,
  onTitleChange,
  version,
  open,
  onOpenChange,
}: {
  projectId: string
  title: string
  onTitleChange: (title: string) => void
  version: number
  open: boolean
  onOpenChange: (open: boolean) => void
}) {
  const updateProject = useUpdateProject()

  async function save() {
    try {
      await updateProject.mutateAsync({ id: projectId, title: title.trim(), version })
      onOpenChange(false)
    } catch {
      // the error code is rendered below the input
    }
  }

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-sm">
        <DialogHeader>
          <DialogTitle>重命名项目</DialogTitle>
          <DialogDescription>修改立即生效；并发修改返回版本冲突。</DialogDescription>
        </DialogHeader>
        <div className="space-y-4">
          <div className="space-y-1.5">
            <Label htmlFor="rename-project-name">名称</Label>
            <Input
              id="rename-project-name"
              value={title}
              onChange={(e) => onTitleChange(e.target.value)}
            />
          </div>
          {updateProject.error?.response?.data?.code && (
            <p className="text-xs text-destructive">
              保存失败：{updateProject.error?.response?.data?.code}
            </p>
          )}
          <Button
            className="w-full"
            disabled={updateProject.isPending || title.trim() === ''}
            onClick={() => void save()}
          >
            {updateProject.isPending ? '保存中…' : '保存'}
          </Button>
        </div>
      </DialogContent>
    </Dialog>
  )
}
