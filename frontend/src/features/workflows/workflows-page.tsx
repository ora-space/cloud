import { Plus, Search } from 'lucide-react'
import { useMemo, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { useNavigate, useParams } from 'react-router-dom'
import type { Workflow as CloudWorkflow } from '@/api/generated.schemas'
import { PageHeader } from '@/components/layout/page-header'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Skeleton } from '@/components/ui/skeleton'
import { useWorkflows } from '@/features/workflows/api'
import {
  WorkflowCreateDialog,
  WorkflowDeleteDialog,
  WorkflowRenameDialog,
} from '@/features/workflows/workflow-dialogs'
import { WorkflowRow } from '@/features/workflows/workflow-row'
import { workspacePaths } from '@/lib/paths'

const SKELETON_KEYS = ['one', 'two', 'three', 'four']

/** Case-insensitive substring match over the fields a member can see. */
function matches(workflow: CloudWorkflow, query: string): boolean {
  const needle = query.trim().toLowerCase()
  if (needle === '') return true
  return (
    workflow.name.toLowerCase().includes(needle) ||
    workflow.description.toLowerCase().includes(needle)
  )
}

/**
 * The list's body: a skeleton while loading, then rows, then one of two empty
 * states — "nothing yet" for an empty tenant, "nothing matches" for a search
 * that filtered everything out. They read differently on purpose.
 */
function WorkflowListBody({
  list,
  workflows,
  query,
  hrefFor,
  runsHrefFor,
  runHrefFor,
  onRename,
  onDelete,
}: {
  list: ReturnType<typeof useWorkflows>
  workflows: CloudWorkflow[]
  query: string
  hrefFor: (id: string) => string
  runsHrefFor: (id: string) => string
  runHrefFor: (workflowId: string, runId: string) => string
  onRename: (workflow: CloudWorkflow) => void
  onDelete: (workflow: CloudWorkflow) => void
}) {
  const { t } = useTranslation()

  if (list.isPending) {
    return (
      <div className="space-y-2">
        {SKELETON_KEYS.map((key) => (
          <Skeleton key={key} className="h-16 w-full" />
        ))}
      </div>
    )
  }
  if (list.isError) {
    return <p className="text-sm text-muted-foreground">{t('workflows.errors.unknown')}</p>
  }
  if (workflows.length === 0) {
    const searched = query.trim() !== ''
    return (
      <div className="rounded-lg border border-dashed p-8 text-center">
        <p className="text-sm font-medium">
          {t(searched ? 'workflows.list.noMatchTitle' : 'workflows.list.emptyTitle')}
        </p>
        <p className="mt-1 text-xs text-muted-foreground">
          {t(searched ? 'workflows.list.noMatchDescription' : 'workflows.list.emptyDescription')}
        </p>
      </div>
    )
  }
  return (
    <div className="space-y-2">
      {workflows.map((workflow) => (
        <WorkflowRow
          key={workflow.id}
          workflow={workflow}
          href={hrefFor(workflow.id)}
          runsHref={runsHrefFor(workflow.id)}
          runHref={(runId) => runHrefFor(workflow.id, runId)}
          onRename={onRename}
          onDelete={onDelete}
        />
      ))}
    </div>
  )
}

/**
 * The tenant's workflow surface: the list a member opens to reach the editor.
 *
 * `slug` is the tenant id, because workflows are tenant-owned rather than
 * space-scoped — the same scope `CloudScope` resolves for the issue list. The
 * workspace slug used to build links therefore comes from the route, not from
 * the prop.
 *
 * @param props.slug - Tenant id the list is scoped to.
 */
export function WorkflowsPage({ slug }: { slug: string }) {
  const { t } = useTranslation()
  const { workspaceSlug } = useParams<{ workspaceSlug: string }>()
  const paths = workspacePaths(workspaceSlug ?? slug)
  const navigate = useNavigate()
  const [query, setQuery] = useState('')
  const [createOpen, setCreateOpen] = useState(false)
  const [renaming, setRenaming] = useState<CloudWorkflow | null>(null)
  const [deleting, setDeleting] = useState<CloudWorkflow | null>(null)

  const list = useWorkflows(slug)
  const workflows = useMemo(
    () => (list.data?.items ?? []).filter((workflow) => matches(workflow, query)),
    [list.data, query],
  )

  return (
    <div className="flex h-full flex-col">
      <PageHeader
        title={t('workflows.list.title')}
        actions={
          <div className="flex items-center gap-2">
            <div className="relative">
              <Search className="absolute left-2 top-1/2 size-3.5 -translate-y-1/2 text-muted-foreground" />
              <Input
                value={query}
                onChange={(event) => setQuery(event.target.value)}
                placeholder={t('workflows.list.search')}
                className="h-8 w-48 pl-7"
                aria-label={t('workflows.list.search')}
              />
            </div>
            <Button size="sm" onClick={() => setCreateOpen(true)}>
              <Plus className="size-3.5" />
              {t('workflows.list.new')}
            </Button>
          </div>
        }
      />
      <div className="flex-1 overflow-y-auto p-4">
        <WorkflowListBody
          list={list}
          workflows={workflows}
          query={query}
          hrefFor={paths.workflowDetail}
          runsHrefFor={paths.workflowRuns}
          runHrefFor={paths.workflowRun}
          onRename={setRenaming}
          onDelete={setDeleting}
        />
      </div>

      <WorkflowCreateDialog
        tenantId={slug}
        open={createOpen}
        onOpenChange={setCreateOpen}
        onCreated={(workflowId) => void navigate(paths.workflowDetail(workflowId))}
      />
      <WorkflowRenameDialog
        tenantId={slug}
        workflow={renaming}
        onOpenChange={(open) => {
          if (!open) setRenaming(null)
        }}
      />
      <WorkflowDeleteDialog
        tenantId={slug}
        workflow={deleting}
        onOpenChange={(open) => {
          if (!open) setDeleting(null)
        }}
      />
    </div>
  )
}
