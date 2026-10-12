import { useState } from 'react'
import type { GitCredential } from '@/api/generated.schemas'
import { CreateFormSubmit } from '@/components/common/create-form-submit'
import { DialogFormField } from '@/components/common/dialog-form-field'
import {
  Dialog,
  DialogContent,
  DialogDescription,
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
import { useCreateProject } from '@/features/projects/api'
import { useGitCredentials } from '@/features/settings/git-credentials-api'
import { useCurrentSpace } from '@/features/spaces/current-space'

/**
 * Accepts HTTPS or SSH repository URLs without embedded credentials; the
 * backend enforces the same rule and reports 400 for anything else.
 */
function looksLikeRepositoryUrl(value: string): boolean {
  return /^(https:\/\/|ssh:\/\/|git@)/.test(value.trim())
}

/** Sentinel for "no credential"; credential ids are UUIDs and never collide. */
const NO_CREDENTIAL = 'none'

// The server answers a refused credential with a reason code, not a sentence;
// these two are actionable (pick another or fix the scope), so they get their
// own copy instead of the generic "创建失败：<code>" line.
const CREDENTIAL_REFUSALS: Record<string, string> = {
  repository_credential_frozen:
    '所选凭据当前不可用（已冻结或已撤销），请改用其他凭据，或选择“不使用凭据（公开仓库）”',
  repository_credential_scope_mismatch:
    '所选凭据的仓库范围与项目仓库 URL 不一致，请重新选择匹配的凭据',
}

/**
 * Input the create form submits; `credentialRefId` is absent when the member
 * chose a public repository, so the request never carries an empty id.
 */
interface CreateProjectDraft {
  title: string
  repositoryUrl: string
  defaultBranch: string
  credentialRefId?: string
}

/**
 * Optional credential picker. It lists the caller's own credentials (name and
 * repository scope) and defaults to a public repository; the backend refuses a
 * credential that is not available or whose scope does not exactly match the
 * project's repository URL, and the dialog turns those codes into advice.
 */
function CredentialPicker({
  credentials,
  value,
  onChange,
}: {
  credentials: GitCredential[]
  value: string
  onChange: (value: string) => void
}) {
  return (
    <div className="space-y-1.5">
      <Label htmlFor="new-project-credential">凭据</Label>
      <Select
        value={value}
        onValueChange={(next) => {
          if (next !== null) onChange(next)
        }}
      >
        <SelectTrigger id="new-project-credential" className="w-full">
          <SelectValue />
        </SelectTrigger>
        <SelectContent>
          <SelectItem value={NO_CREDENTIAL}>不使用凭据（公开仓库）</SelectItem>
          {credentials.map((credential) => (
            <SelectItem key={credential.id} value={credential.id}>
              {credential.name}（{credential.repositoryScope}）
            </SelectItem>
          ))}
        </SelectContent>
      </Select>
      <p className="text-xs text-muted-foreground">
        仅列出你自己的 Git 凭据；私有仓库需选择范围匹配的凭据。
      </p>
    </div>
  )
}

/** Fields for a new project in the current space, including its credential. */
function CreateProjectFields({
  onSubmit,
  pending,
  errorCode,
}: {
  onSubmit: (input: CreateProjectDraft) => void
  pending: boolean
  errorCode: string | undefined
}) {
  const { tenantId } = useCurrentSpace()
  const credentials = useGitCredentials(tenantId)
  const [title, setTitle] = useState('')
  const [repositoryUrl, setRepositoryUrl] = useState('')
  const [defaultBranch, setDefaultBranch] = useState('main')
  const [credentialValue, setCredentialValue] = useState(NO_CREDENTIAL)
  const urlValid = repositoryUrl === '' || looksLikeRepositoryUrl(repositoryUrl)
  // Cloud clones only a literal branch and never reads the remote to resolve HEAD.
  const branchValid = defaultBranch.trim() !== '' && defaultBranch.trim() !== 'HEAD'
  const submittable =
    title.trim() !== '' && repositoryUrl.trim() !== '' && urlValid && branchValid && !pending
  const credentialRefusal = errorCode ? CREDENTIAL_REFUSALS[errorCode] : undefined

  return (
    <form
      onSubmit={(e) => {
        e.preventDefault()
        if (!submittable) return
        onSubmit({
          title: title.trim(),
          repositoryUrl: repositoryUrl.trim(),
          defaultBranch: defaultBranch.trim(),
          ...(credentialValue === NO_CREDENTIAL ? {} : { credentialRefId: credentialValue }),
        })
      }}
      className="space-y-4"
    >
      <DialogFormField
        id="new-project-name"
        label="名称"
        value={title}
        onChange={setTitle}
        placeholder="Demo Project"
        required
      />
      <DialogFormField
        id="new-project-url"
        label="仓库 URL（HTTPS 或 SSH）"
        value={repositoryUrl}
        onChange={setRepositoryUrl}
        placeholder="https://example.com/repo.git"
        hint={urlValid ? undefined : '仅支持 https:// 或 ssh:// 地址'}
        required
      />
      <DialogFormField
        id="new-project-branch"
        label="默认分支"
        value={defaultBranch}
        onChange={setDefaultBranch}
        placeholder="main"
        hint={branchValid ? undefined : '请填写具体分支名，不能为空或 HEAD'}
        required
      />
      <CredentialPicker
        credentials={credentials.data ?? []}
        value={credentialValue}
        onChange={setCredentialValue}
      />
      {credentialRefusal && (
        <p role="alert" className="text-xs text-destructive">
          {credentialRefusal}
        </p>
      )}
      <CreateFormSubmit
        pending={pending}
        submittable={submittable}
        errorCode={credentialRefusal ? undefined : errorCode}
      />
    </form>
  )
}

/**
 * Dialog for creating a project inside the current space. Creation is
 * asynchronous on the backend (202 + operation), so the dialog closes
 * immediately and the list invalidates when the operation lands. The optional
 * credential picker never sends an id for a public repository.
 */
export function CreateProjectDialog({
  open,
  onOpenChange,
  onCreated,
}: {
  open: boolean
  onOpenChange: (open: boolean) => void
  onCreated: (projectId: string) => void
}) {
  const createProject = useCreateProject()

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-sm">
        <DialogHeader>
          <DialogTitle>新建项目</DialogTitle>
          <DialogDescription>项目创建后将异步启动运行环境并克隆所选分支。</DialogDescription>
        </DialogHeader>
        <CreateProjectFields
          pending={createProject.isPending}
          errorCode={createProject.error?.response?.data?.code}
          onSubmit={(input) => {
            void (async () => {
              try {
                const result = await createProject.mutateAsync(input)
                onOpenChange(false)
                onCreated(result.resource.id)
              } catch {
                // the hook surfaces the fault code next to the form
              }
            })()
          }}
        />
      </DialogContent>
    </Dialog>
  )
}
