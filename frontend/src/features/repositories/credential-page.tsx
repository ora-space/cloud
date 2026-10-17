import { useState } from 'react'
import type { GitCredential, GitCredentialSubmission } from '@/api/generated.schemas'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { useSession } from '@/features/auth/session'
import { useCurrentSpace } from '@/features/spaces/current-space'
import { faultCode } from '@/lib/api-client'
import { useConfirmCredential, useCredentialActions, useGitCredentials } from './api'
import { submitGitToken } from './submission'

/** Tenant-scoped Git credential metadata, with secrets submitted directly to the Store. */
export function GitCredentialsPage() {
  const list = useGitCredentials()
  const actions = useCredentialActions()
  const [pending, setPending] = useState<{
    id: string
    submission: GitCredentialSubmission
  } | null>(null)
  async function create(data: {
    name: string
    repositoryScope: string
    attribution: 'personal' | 'team'
  }) {
    try {
      const result = await actions.mutateAsync({ action: 'create', ...data })
      if (result.submission) setPending({ id: result.resource.id, submission: result.submission })
    } catch {
      /* The mutation exposes a safe error below. */
    }
  }
  return (
    <div className="space-y-5 p-6">
      <h1 className="text-xl font-semibold">Git 凭据</h1>
      <p>凭据仅用于所填仓库。个人凭据的发布由所有者批准；团队凭据由管理员批准。</p>
      <CredentialForm onCreate={create} pending={actions.isPending} />
      {(list.isError || actions.isError) && (
        <p role="alert">{faultCode(actions.error) ?? '无法读取凭据，请重试'}</p>
      )}
      {pending && (
        <SecretSubmission
          key={pending.submission.submissionId}
          pending={pending}
          onDone={() => setPending(null)}
        />
      )}
      {list.data?.items.map((c) => (
        <CredentialRow
          key={c.id}
          credential={c}
          onRotate={(submission) => setPending({ id: c.id, submission })}
        />
      ))}
    </div>
  )
}

function CredentialForm({
  onCreate,
  pending,
}: {
  onCreate: (data: {
    name: string
    repositoryScope: string
    attribution: 'personal' | 'team'
  }) => Promise<void>
  pending: boolean
}) {
  const [name, setName] = useState('')
  const [scope, setScope] = useState('')
  const [attribution, setAttribution] = useState<'personal' | 'team'>('personal')
  const { space } = useCurrentSpace()
  return (
    <form
      className="flex flex-wrap gap-2"
      onSubmit={(e) => {
        e.preventDefault()
        void onCreate({ name, repositoryScope: scope, attribution })
      }}
    >
      <Input
        aria-label="凭据名称"
        value={name}
        onChange={(e) => setName(e.target.value)}
        required
        placeholder="凭据名称"
      />
      <Input
        aria-label="仓库作用域"
        value={scope}
        onChange={(e) => setScope(e.target.value)}
        required
        type="url"
        placeholder="https://github.com/org/repo.git"
      />
      <select
        aria-label="凭据归属"
        value={attribution}
        onChange={(e) => setAttribution(e.target.value === 'team' ? 'team' : 'personal')}
      >
        <option value="personal">个人</option>
        {space?.role === 'admin' && <option value="team">团队</option>}
      </select>
      <Button disabled={pending}>申请提交授权</Button>
    </form>
  )
}

function SecretSubmission({
  pending,
  onDone,
}: {
  pending: { id: string; submission: GitCredentialSubmission }
  onDone: () => void
}) {
  const [token, setToken] = useState('')
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  const confirm = useConfirmCredential()
  async function submit() {
    setBusy(true)
    setError('')
    const secret = token
    setToken('')
    try {
      await submitGitToken(pending.submission, secret)
      await confirm.mutateAsync({
        credentialId: pending.id,
        submissionId: pending.submission.submissionId,
      })
      onDone()
    } catch {
      setError('提交或确认失败，请检查凭据状态后重新申请授权')
    } finally {
      setBusy(false)
    }
  }
  return (
    <form
      className="space-y-2 rounded border p-3"
      onSubmit={(e) => {
        e.preventDefault()
        void submit()
      }}
    >
      <p>token 将直接发送到 {new URL(pending.submission.ingressUrl).origin}，不经过 Cloud。</p>
      <Input
        aria-label="Git token"
        type="password"
        autoComplete="off"
        value={token}
        onChange={(e) => setToken(e.target.value)}
        required
      />
      <Button disabled={busy}>提交凭据</Button>{' '}
      <Button type="button" variant="outline" onClick={onDone}>
        取消
      </Button>
      {error && <p role="alert">{error}</p>}
    </form>
  )
}

function CredentialRow({
  credential: c,
  onRotate,
}: {
  credential: GitCredential
  onRotate: (s: GitCredentialSubmission) => void
}) {
  const { session } = useSession()
  const { space } = useCurrentSpace()
  const actions = useCredentialActions()
  const manager =
    c.attribution === 'personal'
      ? session.status === 'signed-in' && session.user.id === c.ownerUserId
      : space?.role === 'admin'
  async function act(action: 'rotate' | 'verify' | 'revoke') {
    try {
      const result = await actions.mutateAsync({ action, credential: c })
      if (result.submission) onRotate(result.submission)
    } catch {
      /* Display only the safe fault code. */
    }
  }
  return (
    <article className="space-y-2 rounded border p-3">
      <p>
        {c.name} · {c.repositoryScope} · {c.secretState} / {c.availability}
      </p>
      <p>
        已验证能力：{c.capabilities.join('、') || '暂无'} · 版本 {c.storeVersion} ·{' '}
        {c.lastVerification?.result}
      </p>
      {c.pendingSubmission && <p>有待确认的新版本</p>}
      {manager && c.secretState !== 'revoked' && (
        <>
          <Button disabled={actions.isPending} onClick={() => void act('rotate')}>
            轮换 token
          </Button>
          <Button
            disabled={actions.isPending || c.secretState !== 'stored'}
            onClick={() => void act('verify')}
          >
            验证推送能力
          </Button>
        </>
      )}
      {(manager || space?.role === 'admin') && c.secretState !== 'revoked' && (
        <Button
          variant="destructive"
          disabled={actions.isPending}
          onClick={() => void act('revoke')}
        >
          撤销凭据
        </Button>
      )}
      {actions.isError && <p role="alert">{faultCode(actions.error) ?? '操作失败'}</p>}
    </article>
  )
}
