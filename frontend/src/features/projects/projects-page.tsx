import { format } from 'date-fns'
import { Plus } from 'lucide-react'
import { useState } from 'react'
import { Link, useNavigate } from 'react-router-dom'
import { ActorAvatar } from '@/components/common/actor-avatar'
import { PageHeader } from '@/components/layout/page-header'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Skeleton } from '@/components/ui/skeleton'
import { CreateProjectDialog } from '@/features/projects/create-project-dialog'
import { ProjectIcon } from '@/features/projects/components/project-icon'
import { useProjects } from '@/features/projects/api'
import { useProjectProvisioning } from '@/features/projects/provisioning'
import { projectStatusBadge } from '@/features/projects/status'
import { useCurrentSpace } from '@/features/spaces/current-space'
import { workspacePaths } from '@/lib/paths'
import { actorById, db } from '@/mocks/data/store'
import type { Project } from '@/mocks/data/types'

const SKELETON_KEYS = ['one', 'two', 'three', 'four', 'five', 'six']

export function ProjectsPage({ slug }: { slug: string }) {
  const { data: projects, isPending } = useProjects(slug)
  const p = workspacePaths(slug)
  const navigate = useNavigate()
  const { tenantId, space } = useCurrentSpace()
  const ready = space?.slug === slug
  const [createOpen, setCreateOpen] = useState(false)

  return (
    <div className="flex h-full flex-col">
      <PageHeader
        title="项目"
        actions={
          ready && (
            <Button size="sm" onClick={() => setCreateOpen(true)}>
              <Plus className="size-3.5" />
              新建项目
            </Button>
          )
        }
      />
      {ready && (
        <CreateProjectDialog
          open={createOpen}
          onOpenChange={setCreateOpen}
          onCreated={(projectId) => void navigate(p.projectDetail(projectId))}
        />
      )}
      <div className="flex-1 overflow-y-auto p-4">
        {isPending && (
          <div className="grid grid-cols-1 gap-3 sm:grid-cols-2 lg:grid-cols-3">
            {SKELETON_KEYS.map((key) => (
              <Skeleton key={key} className="h-32 w-full" />
            ))}
          </div>
        )}
        <div className="grid grid-cols-1 gap-3 sm:grid-cols-2 lg:grid-cols-3">
          {projects?.map((project) => (
            <ProjectCard
              key={project.id}
              slug={slug}
              project={project}
              tenantId={ready ? tenantId : undefined}
            />
          ))}
        </div>
      </div>
    </div>
  )
}

/**
 * One project card, with the status badge a member decides from. It is its own
 * component because the badge needs the project's provisioning phase, and a hook
 * cannot be called once per item in a list: the card owns the one query its own
 * project needs.
 */
function ProjectCard({
  slug,
  project,
  tenantId,
}: {
  slug: string
  project: Project
  tenantId: string | undefined
}) {
  const lead = actorById(project.leadId)
  // Only a project that is still being created can have a provisioning answer to
  // show; `planned` is the UI status a `provisioning` lifecycle maps to. An
  // active project is settled, and asking about it would cost two requests per
  // card for an answer that cannot change.
  const creating = project.status === 'planned' && tenantId !== undefined
  const { phase } = useProjectProvisioning(
    creating ? tenantId : undefined,
    creating ? project.id : undefined,
  )
  const statusBadge = projectStatusBadge(project.status, phase)
  const issueCount = db.issues.filter((i) => i.projectId === project.id).length
  const doneCount = db.issues.filter(
    (i) => i.projectId === project.id && i.status === 'done',
  ).length

  return (
    <Link
      to={workspacePaths(slug).projectDetail(project.id)}
      className="flex flex-col gap-3 rounded-lg border p-4 hover:bg-muted/50"
    >
      <div className="flex items-center gap-2">
        <ProjectIcon project={project} />
        <span className="min-w-0 flex-1 truncate text-sm font-medium">{project.title}</span>
      </div>
      <p className="line-clamp-2 text-xs text-muted-foreground">{project.description}</p>
      <div className="mt-auto flex items-center justify-between text-xs text-muted-foreground">
        <Badge variant={statusBadge.variant}>{statusBadge.label}</Badge>
        <span>
          {doneCount}/{issueCount} 个任务
        </span>
      </div>
      <div className="flex items-center justify-between">
        <ActorAvatar actor={lead} size="sm" />
        {project.targetDate && (
          <span className="text-xs text-muted-foreground">
            {format(new Date(project.targetDate), 'M月d日')}
          </span>
        )}
      </div>
    </Link>
  )
}
