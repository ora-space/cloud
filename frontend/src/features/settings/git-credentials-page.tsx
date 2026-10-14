import { useState } from 'react'
import { format } from 'date-fns'
import type { GitCredential } from '@/api/generated.schemas'
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
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Skeleton } from '@/components/ui/skeleton'
import { useCurrentSpace } from '@/features/spaces/current-space'
import {
  gitCredentialFailureMessage,
  useCreateGitCredential,
  useGitCredentials,
  useRevokeGitCredential,
  useRotateGitCredential,
} from './git-credentials-api'

const AVAILABILITY_LABELS: Record<GitCredential['availability'], string> = {
  available: '可用',
  frozen: '已冻结',
  invalid: '已删除',
}

const AVAILABILITY_VARIANTS: Record<
  GitCredential['availability'],
  'default' | 'outline' | 'destructive'
> = {
  available: 'default',
  frozen: 'outline',
  invalid: 'destructive',
}

// The backend explains a non-available state through a stable reason code; an
// unrecognized code falls back to a generic line rather than rendering the raw
// code, which would mean less than showing only the availability badge.
const UNAVAILABLE_REASONS: Record<string, string> = {
  revoked_by_owner: 'Token 已销毁，记录为审计保留',
  associated_member_departed: '因不再是有效成员被冻结，重新加入也不会解冻',
}

/** One-line explanation of why a credential is not available, if it is not. */
function unavailableExplanation(credential: GitCredential): string | undefined {
  if (credential.availability === 'available') return undefined
  const reason = credential.unavailableReason
  return (reason ? UNAVAILABLE_REASONS[reason] : undefined) ?? '原因未提供'
}

/** Availability badge; the single source of truth for whether it may be used. */
function AvailabilityBadge({ credential }: { credential: GitCredential }) {
  return (
    <Badge variant={AVAILABILITY_VARIANTS[credential.availability]}>
      {AVAILABILITY_LABELS[credential.availability]}
    </Badge>
  )
}

/**
 * Create form: the label, the one repository the credential is scoped to, and
 * the token. The token stays in this form's local state only and is cleared
 * once the server accepted it; a refusal keeps it so the member can retry.
 */
function CreateGitCredentialForm({ tenantId }: { tenantId: string | undefined }) {
  const create = useCreateGitCredential(tenantId)
  const [name, setName] = useState('')
  const [repositoryScope, setRepositoryScope] = useState('')
  const [token, setToken] = useState('')
  const submittable =
    name.trim() !== '' && repositoryScope.trim() !== '' && token !== '' && !create.isPending

  return (
    <form
      className="space-y-3"
      onSubmit={(event) => {
        event.preventDefault()
        if (!submittable) return
        create.mutate(
          { name: name.trim(), repositoryScope: repositoryScope.trim(), token },
          {
            onSuccess: () => {
              setName('')
              setRepositoryScope('')
              setToken('')
            },
          },
        )
      }}
    >
      <div className="space-y-1.5">
        <Label htmlFor="git-credential-name">名称</Label>
        <Input
          id="git-credential-name"
          value={name}
          onChange={(event) => setName(event.target.value)}
        />
      </div>
      <div className="space-y-1.5">
        <Label htmlFor="git-credential-scope">仓库范围</Label>
        <Input
          id="git-credential-scope"
          value={repositoryScope}
          placeholder="https://github.com/owner/repo"
          onChange={(event) => setRepositoryScope(event.target.value)}
        />
      </div>
      <div className="space-y-1.5">
        <Label htmlFor="git-credential-token">Token</Label>
        <Input
          id="git-credential-token"
          type="password"
          autoComplete="off"
          value={token}
          onChange={(event) => setToken(event.target.value)}
        />
      </div>
      <div className="flex items-center gap-3">
        <Button type="submit" disabled={!submittable}>
          添加凭据
        </Button>
        {create.error != null && (
          <p role="alert" className="text-xs text-destructive">
            {gitCredentialFailureMessage(create.error)}
          </p>
        )}
        {create.isSuccess && !create.error && (
          <p role="status" className="text-xs text-muted-foreground">
            已添加
          </p>
        )}
      </div>
    </form>
  )
}

/**
 * Per-row rotate and delete. Both are disabled while the credential is not
 * available because the server would refuse them; the disabled state is a hint,
 * never the authorization — the server checks availability and version again.
 *
 * Deleting is confirmed first, and the dialog states the consequences that
 * outlive the click: the Token is destroyed in the Credential Service, the
 * credential can never authorize a clone again, and a project already bound to
 * it is never re-pointed at another credential — that project simply stops
 * cloning until the member binds a replacement.
 */
function GitCredentialActions({
  tenantId,
  credential,
}: {
  tenantId: string | undefined
  credential: GitCredential
}) {
  const rotate = useRotateGitCredential(tenantId)
  const revoke = useRevokeGitCredential(tenantId)
  const [token, setToken] = useState('')
  const [confirming, setConfirming] = useState(false)
  const available = credential.availability === 'available'
  const busy = rotate.isPending || revoke.isPending
  const failure = rotate.error ?? revoke.error

  return (
    <div className="space-y-2">
      <div className="flex items-end gap-2">
        <div className="flex-1 space-y-1.5">
          <Label htmlFor={`rotate-token-${credential.id}`}>新 Token</Label>
          <Input
            id={`rotate-token-${credential.id}`}
            type="password"
            autoComplete="off"
            value={token}
            disabled={!available}
            onChange={(event) => setToken(event.target.value)}
          />
        </div>
        <Button
          type="button"
          variant="outline"
          disabled={!available || token === '' || busy}
          onClick={() => {
            revoke.reset()
            rotate.mutate(
              { id: credential.id, token, version: credential.version },
              { onSuccess: () => setToken('') },
            )
          }}
        >
          轮换
        </Button>
        <AlertDialog open={confirming} onOpenChange={setConfirming}>
          <AlertDialogTrigger
            render={
              <Button type="button" variant="destructive" disabled={!available || busy}>
                删除凭据
              </Button>
            }
          />
          <AlertDialogContent>
            <AlertDialogHeader>
              <AlertDialogTitle>删除「{credential.name}」？</AlertDialogTitle>
              <AlertDialogDescription>
                删除后 Token
                立即销毁，该凭据不能再用于任何克隆。已绑定它的项目不会被改绑到其他凭据，在重新绑定之前无法克隆。
              </AlertDialogDescription>
            </AlertDialogHeader>
            <AlertDialogFooter>
              <AlertDialogCancel>取消</AlertDialogCancel>
              <AlertDialogAction
                variant="destructive"
                onClick={() => {
                  // Close before dispatching: the row is where a refusal is
                  // reported, and an open modal marks everything outside it
                  // aria-hidden, so a dialog left open would hide its own
                  // failure from the member.
                  setConfirming(false)
                  rotate.reset()
                  revoke.mutate({ id: credential.id, version: credential.version })
                }}
              >
                确认删除
              </AlertDialogAction>
            </AlertDialogFooter>
          </AlertDialogContent>
        </AlertDialog>
      </div>
      {failure != null && (
        <p role="alert" className="text-xs text-destructive">
          {gitCredentialFailureMessage(failure)}
        </p>
      )}
      {failure == null && revoke.isSuccess && (
        <p role="status" className="text-xs text-muted-foreground">
          已删除，Token 已销毁
        </p>
      )}
      {failure == null && !revoke.isSuccess && rotate.isSuccess && (
        <p role="status" className="text-xs text-muted-foreground">
          已轮换
        </p>
      )}
    </div>
  )
}

/** One listed credential: identity, availability, and its rotate/delete row. */
function GitCredentialRow({
  tenantId,
  credential,
}: {
  tenantId: string | undefined
  credential: GitCredential
}) {
  const explanation = unavailableExplanation(credential)
  return (
    <li className="space-y-3 rounded-md border p-3">
      <div className="flex items-start justify-between gap-2">
        <div className="min-w-0 space-y-0.5">
          <p className="text-sm font-medium">{credential.name}</p>
          <p className="text-xs break-all text-muted-foreground">{credential.repositoryScope}</p>
          <p className="text-xs text-muted-foreground">
            创建于 {format(new Date(credential.createdAt), 'yyyy年M月d日 HH:mm')}
          </p>
        </div>
        <AvailabilityBadge credential={credential} />
      </div>
      {explanation && (
        <p className="text-xs text-muted-foreground">不可用：{explanation}，无法轮换或删除</p>
      )}
      <GitCredentialActions tenantId={tenantId} credential={credential} />
    </li>
  )
}

/**
 * Settings → "Git 凭据": the signed-in user's own personal HTTPS Git
 * credentials for this space's tenant. A credential is write-only for its
 * token — no response returns it — and deleting one destroys the stored Token
 * while the row stays listed as an audit record, so a member can see what
 * happened instead of a row silently disappearing. The listed row is never
 * what authorizes a clone: the server re-reads availability and version.
 */
export function GitCredentialsPage() {
  const { tenantId } = useCurrentSpace()
  const credentials = useGitCredentials(tenantId)

  return (
    <div className="max-w-2xl space-y-4 p-4">
      <Card>
        <CardHeader>
          <CardTitle className="text-sm">Git 凭据</CardTitle>
        </CardHeader>
        <CardContent className="space-y-4">
          <p className="text-xs text-muted-foreground">
            凭据仅属于你个人，用于克隆对应私有仓库；Token 只在你提交时发送，之后不会再显示。
          </p>
          <CreateGitCredentialForm tenantId={tenantId} />
        </CardContent>
      </Card>
      <Card>
        <CardHeader className="flex flex-row items-center justify-between gap-2">
          <CardTitle className="text-sm">我的凭据</CardTitle>
        </CardHeader>
        <CardContent className="space-y-3">
          {credentials.isError && <p className="text-sm text-destructive">Git 凭据加载失败</p>}
          {credentials.isPending && (
            <Skeleton role="status" aria-label="正在加载 Git 凭据" className="h-24 w-full" />
          )}
          {credentials.data?.length === 0 && (
            <p className="text-sm text-muted-foreground">暂无凭据，请在上方添加。</p>
          )}
          {credentials.data && credentials.data.length > 0 && (
            <ul className="space-y-2">
              {credentials.data.map((credential) => (
                <GitCredentialRow key={credential.id} tenantId={tenantId} credential={credential} />
              ))}
            </ul>
          )}
        </CardContent>
      </Card>
    </div>
  )
}
