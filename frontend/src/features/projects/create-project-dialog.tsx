import { useState } from 'react'
import { DialogFormField } from '@/components/common/dialog-form-field'
import { Button } from '@/components/ui/button'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import { useCreateProject } from '@/features/projects/api'

/**
 * Accepts HTTPS or SSH repository URLs without embedded credentials; the
 * backend enforces the same rule and reports 400 for anything else.
 */
function looksLikeRepositoryUrl(value: string): boolean {
  return /^(https:\/\/|ssh:\/\/|git@)/.test(value.trim())
}

/** Form fields for a new project in the current space. */
function CreateProjectFields({
  onSubmit,
  pending,
  errorCode,
}: {
  onSubmit: (input: { title: string; repositoryUrl: string; defaultBranch: string }) => void
  pending: boolean
  errorCode: string | undefined
}) {
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
      {errorCode && <p className="text-xs text-destructive">创建失败：{errorCode}</p>}
      <Button type="submit" className="w-full" disabled={!submittable}>
        {pending ? '创建中…' : '创建'}
      </Button>
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
          <DialogDescription>项目创建后由后端异步完成存储与运行环境初始化。</DialogDescription>
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
