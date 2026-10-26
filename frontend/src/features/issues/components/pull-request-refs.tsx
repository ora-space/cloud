import { useState } from 'react'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { useContextRefs, useCreateContextRef, useDeleteContextRef } from '@/features/issues/api'

function externalLink(value: string): string | undefined {
  try {
    const url = new URL(value)
    if (['https:', 'http:'].includes(url.protocol) && !url.username && !url.password)
      return url.href
  } catch {
    /* Opaque references remain readable without inventing an external location. */
  }
  return undefined
}

/** Groups existing PR references without claiming knowledge of external review or merge state. */
export function PullRequestRefs({ tid, issueId }: { tid: string; issueId: string }) {
  const refs = useContextRefs(tid, issueId)
  const create = useCreateContextRef(tid, issueId)
  const remove = useDeleteContextRef(tid, issueId)
  const [adding, setAdding] = useState(false)
  const [value, setValue] = useState('')
  const items = refs.isError
    ? []
    : (refs.data ?? []).filter((ref) => ref.refType === 'pull_request')
  return (
    <section aria-label="关联 PR" className="space-y-2 border-t pt-4">
      <div className="flex items-center justify-between">
        <h2 className="font-medium">Pull Request</h2>
        <Button
          size="xs"
          variant="ghost"
          onClick={() => setAdding((open) => !open)}
          aria-expanded={adding}
        >
          添加关联 PR
        </Button>
      </div>
      {refs.isError && (
        <p role="alert" className="text-xs">
          PR 引用加载失败
        </p>
      )}
      {refs.isPending && <p className="text-xs text-muted-foreground">加载中…</p>}
      {!refs.isPending && !refs.isError && !items.length && (
        <p className="text-xs text-muted-foreground">暂无关联 PR</p>
      )}
      {items.map((ref) => (
        <div key={ref.id} className="flex items-start gap-2 text-xs">
          {externalLink(ref.refId) ? (
            <a
              className="min-w-0 flex-1 break-all underline"
              href={externalLink(ref.refId)}
              target="_blank"
              rel="noopener noreferrer"
            >
              {ref.refId}
            </a>
          ) : (
            <span className="min-w-0 flex-1 break-all">{ref.refId}</span>
          )}
          <Button
            size="xs"
            variant="ghost"
            disabled={remove.isPending}
            aria-label={`移除 PR ${ref.refId}`}
            onClick={() => remove.mutate(ref.id)}
          >
            移除
          </Button>
        </div>
      ))}
      {adding && (
        <form
          className="space-y-2"
          onSubmit={(event) => {
            event.preventDefault()
            if (!value.trim() || create.isPending) return
            create.mutate(
              { refType: 'pull_request', refId: value.trim() },
              {
                onSuccess: () => {
                  setValue('')
                  setAdding(false)
                },
              },
            )
          }}
        >
          <Input
            aria-label="PR 链接或引用 ID"
            placeholder="PR 链接或引用 ID"
            value={value}
            onChange={(event) => setValue(event.target.value)}
          />
          <Button size="xs" type="submit" disabled={!value.trim() || create.isPending}>
            保存关联
          </Button>
        </form>
      )}
      {(create.isError || remove.isError) && (
        <p role="alert" className="text-xs text-destructive">
          保存 PR 引用失败，请重试。
        </p>
      )}
    </section>
  )
}
