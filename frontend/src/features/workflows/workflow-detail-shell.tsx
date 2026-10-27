import type { ReactNode } from 'react'
import { useTranslation } from 'react-i18next'
import { NavLink, useParams } from 'react-router-dom'
import { PageHeader } from '@/components/layout/page-header'
import type { TranslationKey } from '@/i18n/i18n-instance'
import { workspacePaths } from '@/lib/paths'
import { cn } from '@/lib/utils'

/** One tab of the workflow detail page: its route and the key naming it. */
interface WorkflowTabLink {
  id: string
  to: string
  labelKey: TranslationKey
}

/**
 * The frame every workflow tab shares: the header, the three tab links, and the body.
 *
 * The tabs are real routes rather than local state, so each one has a URL a member can share,
 * bookmark or reach from a run's breadcrumb — the discoverability this page exists to provide.
 * They also keep the three surfaces apart by construction: the editor is unmounted while a member
 * browses runs, so no draft state leaks into a history read. The editor guards navigation until
 * any outstanding autosave completes, including navigation through these tabs.
 *
 * The current tab is decided by the router, not by a prop: one source for the highlight keeps the
 * header from disagreeing with the address bar after a navigation.
 *
 * @param props.title - Name of the workflow, or the list title while it is still resolving.
 * @param props.workflowId - Workflow whose tabs these are.
 * @param props.children - Body of the tab the route selected.
 */
export function WorkflowDetailShell({
  title,
  workflowId,
  children,
}: {
  title: ReactNode
  workflowId: string
  children: ReactNode
}) {
  const { t } = useTranslation()
  const { workspaceSlug } = useParams<{ workspaceSlug?: string }>()
  const paths = workspacePaths(workspaceSlug ?? '')
  const tabs: WorkflowTabLink[] = [
    { id: 'editor', to: paths.workflowDetail(workflowId), labelKey: 'workflows.tabs.editor' },
    { id: 'runs', to: paths.workflowRuns(workflowId), labelKey: 'workflows.run.history' },
    { id: 'versions', to: paths.workflowVersions(workflowId), labelKey: 'workflows.tabs.versions' },
  ]

  return (
    <div className="flex h-full flex-col">
      <PageHeader
        title={title}
        breadcrumb={{ label: t('workflows.list.title'), to: paths.workflows }}
      />
      <nav aria-label={t('workflows.tabs.label')} className="flex shrink-0 gap-1 border-b px-3">
        {tabs.map((entry) => (
          <NavLink
            key={entry.id}
            to={entry.to}
            end
            className={({ isActive }) =>
              cn(
                '-mb-px border-b-2 px-2 py-2 text-sm transition-colors',
                isActive
                  ? 'border-foreground font-medium text-foreground'
                  : 'border-transparent text-muted-foreground hover:text-foreground',
              )
            }
          >
            {t(entry.labelKey)}
          </NavLink>
        ))}
      </nav>
      <div className="flex min-h-0 flex-1 flex-col">{children}</div>
    </div>
  )
}
