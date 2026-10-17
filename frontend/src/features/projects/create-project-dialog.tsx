import { useState } from 'react'
import { CredentialSelect } from '@/features/repositories/credential-select'
import { DialogFormField } from '@/components/common/dialog-form-field'
import { CreateFormSubmit } from '@/components/common/create-form-submit'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import { useCreateProject } from '@/features/projects/api'

/**
 * Accepts HTTPS repository URLs without embedded credentials; the
 * backend enforces the same rule and reports 400 for anything else.
 */
function looksLikeRepositoryUrl(value: string): boolean {
  return /^https:\/\//.test(value.trim())
}

/** Form fields for a new project in the current space. */
function CreateProjectFields({
  onSubmit,
  pending,
  errorCode,
}: {
  onSubmit: (input: {
    title: string
    repositoryUrl: string
    defaultBranch: string
    credentialRefId?: string
  }) => void
  pending: boolean
  errorCode: string | undefined
}) {
  const [credentialRefId, setCredentialRefId] = useState('')
  const [title, setTitle] = useState('')
  const [repositoryUrl, setRepositoryUrl] = useState('')
  const [defaultBranch, setDefaultBranch] = useState('main')
  const urlValid = repositoryUrl === '' || looksLikeRepositoryUrl(repositoryUrl)
  // Cloud clones only a literal branch and never reads the remote to resolve HEAD.
  const branchValid = defaultBranch.trim() !== '' && defaultBranch.trim() !== 'HEAD'
  const submittable =
    title.trim() !== '' && repositoryUrl.trim() !== '' && urlValid && branchValid && !pending

  return (
    <form
      onSubmit={(e) => {
        e.preventDefault()
        if (!submittable) return
        onSubmit({
          title: title.trim(),
          repositoryUrl: repositoryUrl.trim(),
          defaultBranch: defaultBranch.trim(),
          ...(credentialRefId ? { credentialRefId } : {}),
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
        label="仓库 URL（HTTPS）"
        value={repositoryUrl}
        onChange={setRepositoryUrl}
        placeholder="https://example.com/repo.git"
        hint={urlValid ? undefined : '仅支持 https:// 地址'}
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
      <CredentialSelect value={credentialRefId} onChange={setCredentialRefId} />
      <CreateFormSubmit pending={pending} submittable={submittable} errorCode={errorCode} />
    </form>
  )
}

/**
 * Dialog for creating a project inside the current space. Creation is
 * asynchronous on the backend (202 + operation), so the dialog closes
 * immediately and the list invalidates when the operation lands.
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
