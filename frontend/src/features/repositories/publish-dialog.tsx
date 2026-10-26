import type {
  GitCredential,
  ProjectRepository,
  WorkspaceBranch,
  WorkspaceListItem,
} from '@/api/generated.schemas'
import { Button } from '@/components/ui/button'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { faultCode } from '@/lib/api-client'
import { useGitCredentials, usePublishSources, useRepositoryAction } from './api'
import { mayManageCredential, useCredentialViewer } from './credential-authority'
import { publishReady, publishRequest, usePublishForm } from './publish-form'
import type { PublishForm as PublishFormState } from './publish-form'

/** Explicit external publication from a visible workspace branch or a recorded Revision. */
export function PublishDialog({
  projectId,
  source,
  open,
  onOpenChange,
}: {
  projectId: string
  source: ProjectRepository
  open: boolean
  onOpenChange: (open: boolean) => void
}) {
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>发布到外部仓库</DialogTitle>
          <DialogDescription>只有批准后才会写入外部仓库。仅支持快进发布。</DialogDescription>
        </DialogHeader>
        <PublishForm projectId={projectId} source={source} onDone={() => onOpenChange(false)} />
      </DialogContent>
    </Dialog>
  )
}

/** Who the backend will ask to approve the chosen credential. */
function approvalHint(credential: GitCredential | undefined): string {
  if (credential?.attribution === 'personal') {
    return `需要凭据所有者 ${credential.ownerUserId} 批准`
  }
  return '需要管理员批准'
}

/** The source half of the form: one Workspace branch, or a Revision instead. */
function PublishSourceFields({
  form,
  workspaces,
  branches,
}: {
  form: PublishFormState
  workspaces: WorkspaceListItem[]
  branches: WorkspaceBranch[]
}) {
  return (
    <>
      <label className="block">
        Workspace{' '}
        <select
          aria-label="发布 Workspace"
          value={form.state.workspaceId}
          onChange={(e) => form.chooseWorkspace(e.target.value)}
        >
          <option value="">选择 Workspace</option>
          {workspaces.map((w) => (
            <option key={w.id} value={w.id}>
              {w.id}
            </option>
          ))}
        </select>
      </label>
      <label className="block">
        源分支{' '}
        <select
          aria-label="发布源分支"
          value={form.state.sourceRef}
          onChange={(e) => form.setSourceRef(e.target.value)}
        >
          <option value="">选择分支</option>
          {branches.map((b) => (
            <option key={b.ref} value={b.ref}>
              {b.branch} · {b.commit.slice(0, 12)}
            </option>
          ))}
        </select>
      </label>
      <Input
        aria-label="Revision ID"
        placeholder="或填写 Revision ID"
        value={form.state.revisionId}
        onChange={(e) => form.setRevisionId(e.target.value)}
      />
    </>
  )
}

/** The target half: the credential that will push, its approver, and the external expectation. */
function PublishTargetFields({
  form,
  credentials,
}: {
  form: PublishFormState
  credentials: GitCredential[]
}) {
  const selected = credentials.find((c) => c.id === form.state.credentialId)
  return (
    <>
      <label className="block">
        发布凭据{' '}
        <select
          aria-label="发布凭据"
          value={form.state.credentialId}
          onChange={(e) => form.setCredentialId(e.target.value)}
        >
          <option value="">请选择已验证凭据</option>
          {credentials
            .filter(
              (c) =>
                c.capabilities.includes('publish') &&
                c.secretState === 'stored' &&
                c.availability === 'available',
            )
            .map((c) => (
              <option key={c.id} value={c.id}>
                {c.name}
              </option>
            ))}
        </select>
      </label>
      <p>{approvalHint(selected)}</p>
      <label className="block">
        目标分支
        <Input
          value={form.state.targetBranch}
          onChange={(e) => form.chooseTargetBranch(e.target.value)}
          required
        />
      </label>
      <label className="block">
        基于外部提交（全零表示新分支）
        <Input
          value={form.state.expectedOldOid}
          onChange={(e) => form.setExpectedOldOid(e.target.value)}
          pattern="([0-9a-f]{40}|[0-9a-f]{64})"
          required
        />
      </label>
    </>
  )
}

/** The form's submit row: the request itself, and why it cannot be sent yet. */
function PublishSubmit({
  action,
  ready,
  selfApproved,
}: {
  action: ReturnType<typeof useRepositoryAction>
  ready: boolean
  selfApproved: boolean
}) {
  return (
    <>
      {action.isError && <p role="alert">{faultCode(action.error) ?? '发布请求失败'}</p>}
      <Button type="submit" disabled={action.isPending || !ready}>
        {selfApproved ? '发布' : '请求发布'}
      </Button>
    </>
  )
}

/** The publish form itself; it never submits a push the user did not describe. */
function PublishForm({
  projectId,
  source,
  onDone,
}: {
  projectId: string
  source: ProjectRepository
  onDone: () => void
}) {
  const form = usePublishForm(source)
  const { branches, workspaces } = usePublishSources(projectId, form.state.workspaceId)
  const credentials = useGitCredentials()
  const action = useRepositoryAction(projectId)
  const viewer = useCredentialViewer()
  const selected = credentials.data?.items.find((c) => c.id === form.state.credentialId)
  const branch = branches.data?.items.find((b) => b.ref === form.state.sourceRef)
  // The requester approving their own credential skips the approval step.
  const selfApproved = mayManageCredential(selected, viewer)
  async function submit() {
    try {
      await action.mutateAsync({
        action: 'publish',
        body: publishRequest(form.state, branch?.commit),
      })
      onDone()
    } catch {
      /* Safe fault rendered below. */
    }
  }
  return (
    <form
      className="space-y-3"
      onSubmit={(e) => {
        e.preventDefault()
        void submit()
      }}
    >
      <PublishSourceFields
        form={form}
        workspaces={workspaces.data?.items ?? []}
        branches={branches.data?.items ?? []}
      />
      <PublishTargetFields form={form} credentials={credentials.data?.items ?? []} />
      {branch && <p>源提交：{branch.commit}</p>}
      {branches.isError && <p role="alert">无法读取发布源分支</p>}
      {workspaces.isError && <p role="alert">无法读取 Workspace</p>}
      <PublishSubmit
        action={action}
        ready={publishReady(form.state, branch)}
        selfApproved={selfApproved}
      />
    </form>
  )
}
