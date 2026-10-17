import { useState } from 'react'
import type { RepositoryOperation } from '@/api/generated.schemas'
import { Button } from '@/components/ui/button'
import { useSession } from '@/features/auth/session'
import { useCurrentSpace } from '@/features/spaces/current-space'
import { faultCode } from '@/lib/api-client'
import { useGitCredentials, useRepository, useRepositoryAction } from './api'
import { CredentialSelect } from './credential-select'
import { PublishDialog } from './publish-dialog'

const states: Record<string, string> = {
  pending: '等待执行',
  leased: '准备执行',
  running: '执行中',
  uncertain: '正在核验',
  succeeded: '成功',
  conflict: '外部分支已变化，请先同步并更新 Workspace',
  rejected: '外部仓库拒绝发布',
  cancelled: '已取消',
  failed: '失败',
  superseded: '已被后续同步取代',
  awaiting_approval: '等待批准',
}
const errors: Record<string, string> = {
  credential_rejected: '凭据被外部平台拒绝，请轮换',
  credential_unavailable: '凭据不可用，请检查或更换',
  repository_requires_credential: '私有仓库需要凭据',
  upstream_branch_missing: '外部分支不存在',
  not_fast_forward: '不是快进，请先合并或变基',
  outcome_unknown: '无法确认外部仓库是否已更新，请同步后查看',
  not_applied: '推送没有生效，可以重新发起发布',
  approval_expired: '批准已过期，请重新发起',
  credential_changed: '凭据已轮换，请重新发起发布',
  repository_too_large: '仓库超过大小限制',
}

/** Code source, explicit sync/candidate changes, and auditable publication status. */
export function RepositoryPanel({ projectId }: { projectId: string }) {
  const { source, operations } = useRepository(projectId)
  const action = useRepositoryAction(projectId)
  const [credential, setCredential] = useState('')
  const [publishOpen, setPublishOpen] = useState(false)
  if (source.isPending) return <p className="p-4">正在读取代码来源…</p>
  if (!source.data)
    return (
      <p role="alert" className="p-4">
        无法读取代码来源
      </p>
    )
  const data = source.data
  async function sync() {
    try {
      await action.mutateAsync({ action: 'sync' })
    } catch {
      /* Visible below. */
    }
  }
  async function changeCredential() {
    try {
      await action.mutateAsync({
        action: 'credential',
        credentialRefId: credential,
        version: data.binding.version,
      })
    } catch {
      /* Visible below. */
    }
  }
  return (
    <section className="space-y-3 border-b p-6">
      <h2 className="font-semibold">代码来源</h2>
      <p>
        {data.binding.repositoryUrl} · {data.binding.branch}
      </p>
      <p>
        当前快照：{data.snapshot?.commitOid ?? '尚未同步'}{' '}
        {data.snapshot?.forced && '（外部历史已改写，旧快照仍保留）'}
      </p>
      <p>
        同步：{data.latestSync ? states[data.latestSync.state] : '暂无'} ·{' '}
        {data.latestSync?.errorCode
          ? (errors[data.latestSync.errorCode] ?? data.latestSync.errorCode)
          : ''}
      </p>
      <p>
        凭据：{data.credential?.name ?? '匿名读取'}{' '}
        {data.binding.resyncRequested && ' · 发布后的补同步已安排'}
      </p>
      <Button disabled={action.isPending} onClick={() => void sync()}>
        立即同步
      </Button>
      <CredentialSelect value={credential} onChange={setCredential} />
      <Button
        variant="outline"
        disabled={!credential || action.isPending}
        onClick={() => void changeCredential()}
      >
        验证并更换凭据
      </Button>
      {action.isError && <p role="alert">{faultCode(action.error) ?? '操作失败'}</p>}
      <h2 className="font-semibold">发布</h2>
      <Button onClick={() => setPublishOpen(true)}>发布到外部仓库</Button>
      <PublishDialog
        projectId={projectId}
        source={data}
        open={publishOpen}
        onOpenChange={setPublishOpen}
      />
      {operations.isError && <p role="alert">无法读取发布记录</p>}
      {operations.data?.items
        .filter((o) => o.type === 'repository_publish')
        .map((o) => (
          <PublishRow key={o.id} operation={o} />
        ))}
      <p className="text-sm text-muted-foreground">删除项目可能需要等待正在核验的发布结束。</p>
    </section>
  )
}

function PublishRow({ operation: o }: { operation: RepositoryOperation }) {
  const action = useRepositoryAction(o.projectId)
  const credentials = useGitCredentials()
  const { session } = useSession()
  const { space } = useCurrentSpace()
  const c = credentials.data?.items.find((c) => c.id === o.credentialRefId)
  const uid = session.status === 'signed-in' ? session.user.id : ''
  const approver =
    c?.attribution === 'personal'
      ? c.ownerUserId === uid
      : c?.attribution === 'team' && space?.role === 'admin'
  async function act(name: 'approve' | 'reject' | 'cancel' | 'check') {
    try {
      await action.mutateAsync({ action: name, operationId: o.id })
    } catch {
      /* Visible below. */
    }
  }
  return (
    <article className="space-y-2 rounded border p-3">
      <p>
        {states[o.state]} · {o.targetBranch} · {o.sourceCommit?.slice(0, 12)}
      </p>
      <p>
        源：{o.sourceRef} · 基于外部：{o.expectedOldOid} · 凭据版本 {o.credentialVersion}
      </p>
      <p>
        发起人 {o.requestedBy} · 审批人 {o.approvedBy ?? '待批准'}
      </p>
      {o.errorCode && <p>{errors[o.errorCode] ?? o.errorCode}</p>}
      {o.result['confirmedByVerification'] === true && <p>结果由核验确认</p>}
      {o.state === 'awaiting_approval' && approver && (
        <>
          <Button disabled={action.isPending} onClick={() => void act('approve')}>
            批准
          </Button>
          <Button disabled={action.isPending} onClick={() => void act('reject')}>
            拒绝
          </Button>
        </>
      )}
      {['awaiting_approval', 'pending'].includes(o.state) && uid === o.requestedBy && (
        <Button disabled={action.isPending} onClick={() => void act('cancel')}>
          取消发布
        </Button>
      )}
      {o.state === 'uncertain' && (approver || uid === o.requestedBy) && (
        <Button disabled={action.isPending} onClick={() => void act('check')}>
          立即核验
        </Button>
      )}
      {action.isError && <p role="alert">{faultCode(action.error) ?? '操作失败'}</p>}
    </article>
  )
}
