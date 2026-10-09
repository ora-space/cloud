import { useState } from 'react'
import type { GitIdentity } from '@/api/generated.schemas'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Skeleton } from '@/components/ui/skeleton'
import {
  gitIdentityFailureMessage,
  useGitIdentity,
  useRestoreDefaultGitIdentity,
  useSaveGitIdentity,
} from './git-identity-api'

/**
 * The editable form. Its drafts start from the loaded identity; the page keys
 * it by version, so a save or restore that changes the version reloads the
 * drafts from the server's answer instead of keeping stale text.
 */
function GitIdentityForm({
  identity,
  busy,
  onSave,
  onRestore,
}: {
  identity: GitIdentity
  busy: boolean
  onSave: (draft: { name: string; email: string }) => void
  onRestore: () => void
}) {
  const [name, setName] = useState(identity.name)
  const [email, setEmail] = useState(identity.email)

  return (
    <form
      className="space-y-3"
      onSubmit={(event) => {
        event.preventDefault()
        onSave({ name, email })
      }}
    >
      <div className="space-y-1.5">
        <Label htmlFor="git-identity-name">名称</Label>
        <Input id="git-identity-name" value={name} onChange={(e) => setName(e.target.value)} />
      </div>
      <div className="space-y-1.5">
        <Label htmlFor="git-identity-email">邮箱</Label>
        <Input
          id="git-identity-email"
          type="email"
          value={email}
          onChange={(e) => setEmail(e.target.value)}
        />
      </div>
      <div className="flex gap-2">
        <Button type="submit" disabled={busy}>
          保存
        </Button>
        <Button
          type="button"
          variant="outline"
          disabled={busy || identity.isDefault}
          onClick={onRestore}
        >
          恢复默认
        </Button>
      </div>
    </form>
  )
}

/** The outcome line under the form: the last failure, or a confirmation. */
function GitIdentityOutcome({ failure, saved }: { failure: unknown; saved: boolean }) {
  if (failure) {
    return (
      <p role="alert" className="text-xs text-destructive">
        {gitIdentityFailureMessage(failure)}
      </p>
    )
  }
  return saved ? (
    <p role="status" className="text-xs text-muted-foreground">
      已保存
    </p>
  ) : null
}

/**
 * Settings → "Git 身份": the name and email Agent commits are authored and
 * committed with. The identity belongs to the signed-in user, not to the
 * space, and a change applies only to sessions started afterwards.
 */
export function GitIdentityPage() {
  const { data: identity, isError } = useGitIdentity()
  const save = useSaveGitIdentity()
  const restore = useRestoreDefaultGitIdentity()

  return (
    <div className="max-w-lg space-y-4 p-4">
      <Card>
        <CardHeader className="flex flex-row items-center justify-between gap-2">
          <CardTitle className="text-sm">Git 身份</CardTitle>
          {identity?.isDefault && <Badge variant="secondary">默认身份</Badge>}
        </CardHeader>
        <CardContent className="space-y-3">
          <p className="text-xs text-muted-foreground">
            Agent 提交代码时以此身份作为 Git
            作者（author）与提交者（committer）。修改只对之后启动的会话生效。
          </p>
          {isError && <p className="text-sm text-destructive">Git 身份加载失败</p>}
          {!identity && !isError && <Skeleton className="h-24 w-full" />}
          {identity && (
            <GitIdentityForm
              key={`${identity.version}-${identity.isDefault}`}
              identity={identity}
              busy={save.isPending || restore.isPending}
              onSave={(draft) => {
                restore.reset()
                save.mutate({ ...draft, version: identity.version })
              }}
              onRestore={() => {
                save.reset()
                restore.mutate({ version: identity.version })
              }}
            />
          )}
          <GitIdentityOutcome
            failure={save.error ?? restore.error}
            saved={save.isSuccess || restore.isSuccess}
          />
        </CardContent>
      </Card>
    </div>
  )
}
