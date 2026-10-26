import { useMemo, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { Play } from 'lucide-react'
import type { Project, WorkflowRun, WorkflowSnapshot } from '@/api/generated.schemas'
import { Button } from '@/components/ui/button'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import { Label } from '@/components/ui/label'
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'
import { DynamicFormRenderer } from '@/features/issues/components/dynamic-form-renderer'
import type { FormDescriptor } from '@/features/issues/types'
import { useSpaceProjects } from '@/features/spaces/api'
import { useCurrentSpace } from '@/features/spaces/current-space'
import { useCreateWorkflowRun, useWorkflowSnapshots } from '@/features/workflows/api'
import { useWorkflowTranslator } from '@/features/workflows/editor/use-workflow-translator'
import { workflowErrorKey } from '@/features/workflows/error-messages'
import { parseWorkflowGraphValue } from '@/features/workflows/runtime/graph-codec'
import type { WorkflowNodeTranslator } from '@/features/workflows/runtime/node-factory'
import {
  initialWorkflowRunValues,
  missingWorkflowRunFields,
  workflowRunFields,
  workflowRunInput,
} from '@/features/workflows/runtime/run-input'
import type { WorkflowRunField, WorkflowRunValues } from '@/features/workflows/runtime/run-input'

/**
 * Starts one run against a published version, collecting what that version asks for first.
 *
 * Two choices frame the form, because they are what the run freezes besides the answers. The
 * version decides which frozen graph is asked, and therefore which questions, defaults and
 * required marks appear — an edit saved to the draft since cannot change what this run is started
 * with. The Project decides where the run's Workspace provisions, and it is stated rather than
 * inferred: a workflow belongs to a space, not to a Project, so nothing else could name one. Neither
 * choice is defaulted past what the member sees: the newest version is shown but can be changed, and
 * no Project stands selected until the member picks one, so a run never lands in a Project nobody
 * named.
 *
 * Switching versions remounts the form, which is what discards the previous version's answers rather
 * than carrying them onto fields that may no longer exist; the chosen Project survives that, since
 * it belongs to the run rather than to the version.
 *
 * @param props.tenantId - Tenant the workflow belongs to; scopes the API calls only.
 * @param props.workflowId - Workflow to start a run of.
 * @param props.open - Whether the dialog is showing.
 * @param props.onOpenChange - Receives the next open state.
 * @param props.onStarted - Receives the run the server created, once it exists.
 */
export function WorkflowRunDialog({
  tenantId,
  workflowId,
  open,
  onOpenChange,
  onStarted,
}: {
  tenantId: string
  workflowId: string
  open: boolean
  onOpenChange: (open: boolean) => void
  onStarted: (run: WorkflowRun) => void
}) {
  const { t } = useTranslation()
  const snapshots = useWorkflowSnapshots(tenantId, workflowId)
  const { space } = useCurrentSpace()
  // The route resolves one space; a dialog handed another tenant's id would list Projects the run
  // cannot be created in, so the query stays off rather than guessing.
  const spaceId = space !== undefined && space.tenantId === tenantId ? space.id : undefined
  const projects = useSpaceProjects(tenantId, spaceId)
  const [chosenId, setChosenId] = useState<string | null>(null)
  const [chosenProjectId, setChosenProjectId] = useState<string | null>(null)
  const versions = snapshots.data ?? []
  // The newest version is the default, resolved on every render rather than copied into state: a
  // refetch that adds a version must not leave the dialog pinned to a choice that predates it.
  const selected = versions.find((version) => version.id === chosenId) ?? versions[0]
  const projectChoices = (projects.data?.items ?? []).filter(isRunProject)
  // A refetch can drop the chosen Project (deleted, or moved out of reach); the form then asks again
  // instead of submitting an id the server would refuse.
  const projectId = projectChoices.find((project) => project.id === chosenProjectId)?.id ?? ''

  /** Closing forgets both choices, so the next visit opens on the newest version again. */
  function handleOpenChange(next: boolean): void {
    if (!next) {
      setChosenId(null)
      setChosenProjectId(null)
    }
    onOpenChange(next)
  }

  return (
    <Dialog open={open} onOpenChange={handleOpenChange}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>{t('workflows.run.startTitle')}</DialogTitle>
          <DialogDescription>{t('workflows.run.startDescription')}</DialogDescription>
        </DialogHeader>
        {selected === undefined ? (
          <p className="text-xs text-muted-foreground">{t('workflows.run.noPublishedSnapshot')}</p>
        ) : (
          <>
            <ProjectPicker
              projects={projectChoices}
              selectedId={projectId}
              // Until the route's space resolves, the list is not empty — it is unknown, and an
              // empty-looking picker would claim the space has no Project.
              pending={spaceId === undefined || projects.isLoading}
              failed={projects.isError}
              onSelect={setChosenProjectId}
            />
            <VersionPicker versions={versions} selectedId={selected.id} onSelect={setChosenId} />
            <WorkflowRunForm
              key={selected.id}
              tenantId={tenantId}
              workflowId={workflowId}
              projectId={projectId}
              snapshot={selected}
              onCancel={() => handleOpenChange(false)}
              onStarted={onStarted}
            />
          </>
        )}
      </DialogContent>
    </Dialog>
  )
}

/**
 * Whether a Project can host a run.
 *
 * A Project on its way out cannot: the creation gate refuses one already being deleted, and a
 * deleted one no longer resolves, so offering either would only produce the refusal the member
 * would then have to read.
 */
function isRunProject(project: Project): boolean {
  return project.lifecycle !== 'deleting' && project.lifecycle !== 'deleted'
}

/**
 * The Project the run and its Workspace belong to.
 *
 * The run is created against the Project named here rather than one derived from anything else: a
 * workflow is a tenant document, and the Project is what says which repository the Workspace clones
 * and where its lifecycle is accounted for. A space with no Project to choose from says so, because
 * the next step is creating one — an empty picker would leave the member with a disabled button and
 * no reason why.
 */
function ProjectPicker({
  projects,
  selectedId,
  pending,
  failed,
  onSelect,
}: {
  projects: readonly Project[]
  selectedId: string
  pending: boolean
  failed: boolean
  onSelect: (projectId: string | null) => void
}) {
  const { t } = useTranslation()
  if (failed) {
    return (
      <p role="alert" className="text-xs text-destructive">
        {t('workflows.run.projectsUnavailable')}
      </p>
    )
  }
  if (!pending && projects.length === 0) {
    return <p className="text-xs text-muted-foreground">{t('workflows.run.noProjects')}</p>
  }
  return (
    <div className="space-y-1.5">
      <Label htmlFor="workflow-run-project">{t('workflows.run.project')}</Label>
      <Select
        value={selectedId === '' ? null : selectedId}
        disabled={pending}
        onValueChange={(next) => onSelect(typeof next === 'string' ? next : null)}
      >
        <SelectTrigger id="workflow-run-project" className="w-full">
          <SelectValue placeholder={t('workflows.run.chooseProject')} />
        </SelectTrigger>
        <SelectContent>
          {projects.map((project) => (
            <SelectItem key={project.id} value={project.id}>
              {project.name}
            </SelectItem>
          ))}
        </SelectContent>
      </Select>
      {selectedId === '' && !pending && (
        <p className="text-xs text-muted-foreground">{t('workflows.run.projectRequired')}</p>
      )}
    </div>
  )
}

/** The published versions the run may execute, newest first, labelled the way the `@` form does. */
function VersionPicker({
  versions,
  selectedId,
  onSelect,
}: {
  versions: readonly WorkflowSnapshot[]
  selectedId: string
  onSelect: (snapshotId: string) => void
}) {
  const { t } = useTranslation()
  return (
    <div className="space-y-1.5">
      <Label htmlFor="workflow-run-version">{t('workflows.run.version')}</Label>
      <Select value={selectedId} onValueChange={(next) => onSelect(next ?? '')}>
        <SelectTrigger id="workflow-run-version" className="w-full">
          <SelectValue>
            {(current: unknown) =>
              versionLabel(versions, typeof current === 'string' ? current : '')
            }
          </SelectValue>
        </SelectTrigger>
        <SelectContent>
          {versions.map((version) => (
            <SelectItem key={version.id} value={version.id}>
              {versionLabel(versions, version.id)}
            </SelectItem>
          ))}
        </SelectContent>
      </Select>
    </div>
  )
}

/** The version number is what a member recognises, so it leads; the name says which release it was. */
function versionLabel(versions: readonly WorkflowSnapshot[], snapshotId: string): string {
  const version = versions.find((candidate) => candidate.id === snapshotId)
  if (version === undefined) return snapshotId
  return `v${version.version} · ${version.name}`
}

/**
 * The selected version's own form, its submit, and whatever the server said about the last attempt.
 *
 * The request object is built once per set of answers and reused for every attempt, which is what
 * makes a retry — a second click, or React Query replaying the mutation — carry the same
 * idempotency key and reach the run the first attempt created instead of starting another one.
 */
function WorkflowRunForm({
  tenantId,
  workflowId,
  projectId,
  snapshot,
  onCancel,
  onStarted,
}: {
  tenantId: string
  workflowId: string
  projectId: string
  snapshot: WorkflowSnapshot
  onCancel: () => void
  onStarted: (run: WorkflowRun) => void
}) {
  const { t } = useTranslation()
  const translate = useWorkflowTranslator()
  const createRun = useCreateWorkflowRun(tenantId)
  const fields = useMemo(
    () => workflowRunFields(parseWorkflowGraphValue(snapshot.graph)),
    [snapshot.graph],
  )
  const descriptor = useMemo(() => runFormDescriptor(fields, translate), [fields, translate])
  const [values, setValues] = useState<WorkflowRunValues>(() => initialWorkflowRunValues(fields))
  const missing = missingWorkflowRunFields(fields, values)
  const request = useMemo(
    () => ({
      workflowId,
      projectId,
      snapshotId: snapshot.id,
      input: workflowRunInput(fields, values),
    }),
    [workflowId, projectId, snapshot.id, fields, values],
  )

  /** Starts the run, and hands the created run to the caller; a fault stays on this form. */
  async function submit(): Promise<void> {
    try {
      onStarted(await createRun.mutateAsync(request))
    } catch {
      // the fault line above the footer already reports it
    }
  }

  return (
    <>
      {fields.length === 0 ? (
        <p className="text-xs text-muted-foreground">{t('workflows.run.noInput')}</p>
      ) : (
        <DynamicFormRenderer
          descriptor={descriptor}
          values={values}
          onChange={(key, next) => setValues((current) => ({ ...current, [key]: next }))}
        />
      )}
      {missing.length > 0 && (
        <p className="text-xs text-muted-foreground">
          {t('workflows.run.missingRequired', {
            fields: missing.map((field) => fieldLabel(field, translate)).join('、'),
          })}
        </p>
      )}
      {createRun.error && (
        <p className="text-xs text-destructive">
          {t(workflowErrorKey(createRun.error.response?.data?.code))}
        </p>
      )}
      <DialogFooter>
        <Button variant="outline" onClick={onCancel}>
          {t('workflows.list.cancel')}
        </Button>
        <Button
          disabled={createRun.isPending || missing.length > 0 || projectId === ''}
          onClick={() => void submit()}
        >
          <Play className="size-3.5" fill="currentColor" />
          {t('workflows.run.start')}
        </Button>
      </DialogFooter>
    </>
  )
}

/**
 * The text above one field's control.
 *
 * The platform's own field arrives named by its translation key, because the runtime layer knows
 * which wording is the platform's but not what language the member reads; a variable the graph
 * declares carries its author's label as text. Resolving both through one function is what keeps the
 * label above a control and the "still missing" line below the form saying the same thing about the
 * same field.
 */
function fieldLabel(field: WorkflowRunField, translate: WorkflowNodeTranslator): string {
  return field.labelKey === undefined ? field.label : translate(field.labelKey)
}

/**
 * Recasts the version's fields as the descriptor the shared Issues form renderer draws.
 *
 * The manual entry and the `@` form ask the same questions of the same graph, so they draw them with
 * the same controls; keeping one renderer is what stops the two surfaces from drifting into asking
 * the same thing two different ways. The form reference is this surface's own name, since the
 * descriptor is rendered here and never fetched by it.
 */
function runFormDescriptor(
  fields: readonly WorkflowRunField[],
  translate: WorkflowNodeTranslator,
): FormDescriptor {
  return {
    formRef: 'workflow-run',
    fields: fields.map((field) => ({
      key: field.key,
      label: fieldLabel(field, translate),
      type: field.control,
      required: field.required,
      options: field.options.map((option) => ({ value: option, label: option })),
      ...(field.defaultValue === undefined ? {} : { defaultValue: field.defaultValue }),
    })),
  }
}
