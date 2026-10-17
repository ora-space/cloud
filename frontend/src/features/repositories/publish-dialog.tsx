import { useState } from 'react'
import type { ProjectRepository } from '@/api/generated.schemas'
import { Button } from '@/components/ui/button'
import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
  DialogDescription,
} from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { useSession } from '@/features/auth/session'
import { useCurrentSpace } from '@/features/spaces/current-space'
import { faultCode } from '@/lib/api-client'
import { useGitCredentials, usePublishSources, useRepositoryAction } from './api'

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

function PublishForm({
  projectId,
  source,
  onDone,
}: {
  projectId: string
  source: ProjectRepository
  onDone: () => void
}) {
  const [workspaceId, setWorkspaceId] = useState('')
  const [sourceRef, setSourceRef] = useState('')
  const [revisionId, setRevisionId] = useState('')
  const [credentialId, setCredentialId] = useState(source.binding.credentialRefId ?? '')
  const [targetBranch, setTargetBranch] = useState(source.binding.branch)
  const [oldOid, setOldOid] = useState(source.snapshot?.commitOid ?? '0'.repeat(40))
  const { branches, workspaces } = usePublishSources(projectId, workspaceId)
  const credentials = useGitCredentials()
  const action = useRepositoryAction(projectId)
  const { session } = useSession()
  const { space } = useCurrentSpace()
  const selected = credentials.data?.items.find((c) => c.id === credentialId)
  const branch = branches.data?.items.find((b) => b.ref === sourceRef)
  const selfApproved =
    selected?.attribution === 'team'
      ? space?.role === 'admin'
      : session.status === 'signed-in' && selected?.ownerUserId === session.user.id
  async function submit() {
    try {
      await action.mutateAsync({
        action: 'publish',
        body: {
          ...(revisionId ? { revisionId } : { sourceRef, sourceCommit: branch?.commit ?? '' }),
          targetBranch,
          expectedOldOid: oldOid,
          credentialRefId: credentialId,
        },
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
      <label className="block">
        Workspace{' '}
        <select
          aria-label="发布 Workspace"
          value={workspaceId}
          onChange={(e) => {
            setWorkspaceId(e.target.value)
            setSourceRef('')
          }}
        >
          <option value="">选择 Workspace</option>
          {workspaces.data?.items.map((w) => (
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
          value={sourceRef}
          onChange={(e) => setSourceRef(e.target.value)}
        >
          <option value="">选择分支</option>
          {branches.data?.items.map((b) => (
            <option key={b.ref} value={b.ref}>
              {b.branch} · {b.commit.slice(0, 12)}
            </option>
          ))}
        </select>
      </label>
      <Input
        aria-label="Revision ID"
        placeholder="或填写 Revision ID"
        value={revisionId}
        onChange={(e) => setRevisionId(e.target.value)}
      />
      <label className="block">
        发布凭据{' '}
        <select
          aria-label="发布凭据"
          value={credentialId}
          onChange={(e) => setCredentialId(e.target.value)}
        >
          <option value="">请选择已验证凭据</option>
          {credentials.data?.items
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
      <label className="block">
        目标分支
        <Input
          value={targetBranch}
          onChange={(e) => {
            setTargetBranch(e.target.value)
            setOldOid(
              e.target.value === source.binding.branch
                ? (source.snapshot?.commitOid ?? '0'.repeat(40))
                : '0'.repeat(40),
            )
          }}
          required
        />
      </label>
      <label className="block">
        基于外部提交（全零表示新分支）
        <Input
          value={oldOid}
          onChange={(e) => setOldOid(e.target.value)}
          pattern="([0-9a-f]{40}|[0-9a-f]{64})"
          required
        />
      </label>
      <p>
        {selected?.attribution === 'personal'
          ? `需要凭据所有者 ${selected.ownerUserId} 批准`
          : '需要管理员批准'}
      </p>
      {branch && <p>源提交：{branch.commit}</p>}
      {(branches.isError || workspaces.isError || action.isError) && (
        <p role="alert">{faultCode(action.error) ?? '无法读取发布源'}</p>
      )}
      <Button disabled={action.isPending || !credentialId || (!revisionId && !branch)}>
        {selfApproved ? '发布' : '请求发布'}
      </Button>
    </form>
  )
}
