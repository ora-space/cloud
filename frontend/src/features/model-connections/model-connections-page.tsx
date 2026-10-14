import { useState } from 'react'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Skeleton } from '@/components/ui/skeleton'
import {
  modelConnectionFailure,
  useDeleteConnection,
  useModelConnections,
  useModelDefault,
  type ModelConnection,
} from './api'
import { ConnectionForm } from './connection-form'
import { CredentialForm } from './credential-form'
import { ModelDefaultForm } from './default-form'

function ConnectionCard({
  connection,
  onEdit,
  onDeleted,
}: {
  connection: ModelConnection
  onEdit: () => void
  onDeleted: () => void
}) {
  const remove = useDeleteConnection(connection)
  const [confirming, setConfirming] = useState(false)
  return (
    <article aria-label={connection.name} className="space-y-3 rounded-lg border p-4">
      <div className="flex items-center justify-between gap-2">
        <h3 className="font-medium">{connection.name}</h3>
        <Badge variant="secondary">{connection.enabled ? '已启用' : '已停用'}</Badge>
      </div>
      <p className="break-all text-xs text-muted-foreground">
        {connection.protocol} · {connection.baseUrl} · {connection.authMode}
      </p>
      <ul className="space-y-1 text-sm">
        {connection.models.map((model) => (
          <li key={model.id}>
            {model.name}{' '}
            <span className="text-xs text-muted-foreground">
              {model.id} · 上下文 {model.contextWindow} · 输出 {model.maxTokens}
            </span>
          </li>
        ))}
      </ul>
      <div className="flex gap-2">
        <Button type="button" size="sm" variant="outline" onClick={onEdit}>
          编辑连接
        </Button>
        <Button
          type="button"
          size="sm"
          variant={confirming ? 'destructive' : 'ghost'}
          disabled={remove.isPending}
          onClick={() =>
            confirming
              ? remove.mutate({ version: connection.version }, { onSuccess: onDeleted })
              : setConfirming(true)
          }
        >
          {confirming ? '确认删除连接' : '删除连接'}
        </Button>
        {confirming && (
          <Button type="button" size="sm" variant="ghost" onClick={() => setConfirming(false)}>
            取消删除
          </Button>
        )}
      </div>
      {confirming && (
        <p className="text-xs text-muted-foreground">删除后，使用此连接的运行将停止模型请求。</p>
      )}
      {remove.isError && (
        <p role="alert" className="text-xs text-destructive">
          {modelConnectionFailure(remove.error)}
        </p>
      )}
      <CredentialForm connection={connection} />
    </article>
  )
}

/** Personal model settings; metadata and write-only credentials have separate controls. */
export function ModelConnectionsPage() {
  const connections = useModelConnections()
  const selection = useModelDefault()
  const [editor, setEditor] = useState<{ kind: 'new' } | { kind: 'existing'; id: string } | null>(
    null,
  )
  const connection =
    editor?.kind === 'existing'
      ? connections.data?.find((item) => item.id === editor.id)
      : undefined
  const failed = connections.isError || selection.isError
  return (
    <div className="max-w-2xl space-y-4 p-4">
      <h2 className="text-sm font-semibold">模型连接</h2>
      <p className="text-sm text-muted-foreground">
        这些连接仅属于当前登录用户。API Key 只可写入、更换或清除，页面不会读取密钥。
      </p>
      {failed && (
        <p role="alert" className="text-sm text-destructive">
          模型连接加载失败
        </p>
      )}
      {!failed && (!connections.data || !selection.data) && <Skeleton className="h-24 w-full" />}
      {connections.data && selection.data && (
        <>
          <ModelDefaultForm
            key={`${selection.data.version}-${connections.data.map((item) => `${item.id}:${item.version}`).join(',')}`}
            connections={connections.data}
            selection={selection.data}
          />
          <Button type="button" onClick={() => setEditor({ kind: 'new' })}>
            新建连接
          </Button>
          {editor !== null && (
            <ConnectionForm
              key={connection ? `${connection.id}:${connection.version}` : 'new'}
              connection={connection}
              onClose={() => setEditor(null)}
            />
          )}
          {connections.data.map((item) => (
            <ConnectionCard
              key={item.id}
              connection={item}
              onEdit={() => setEditor({ kind: 'existing', id: item.id })}
              onDeleted={() => {
                if (editor?.kind === 'existing' && editor.id === item.id) setEditor(null)
              }}
            />
          ))}
        </>
      )}
    </div>
  )
}
