import { useState } from 'react'
import { useMutation } from '@tanstack/react-query'
import {
  createMarketSource,
  editMarketSource,
  deleteMarketSource,
  syncMarketSource,
  useListMarketSources,
  type MarketSource as Source,
  type MarketSourceInput,
  type Retrieval,
} from '../market-api/generated'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { DetailPanel } from '../components/detail-panel'

export function MarketSources({
  tenant,
  space,
  owner,
  officialUrl,
  officialSyncedAt,
  onClose,
  refresh,
}: {
  tenant: string
  space: string
  owner: boolean
  officialUrl?: string
  officialSyncedAt?: string
  onClose: () => void
  refresh: () => Promise<void>
}) {
  const list = useListMarketSources(tenant, space, {
    query: { enabled: !!space && !!tenant },
  })
  const [url, setUrl] = useState('')
  const [branch, setBranch] = useState('main')
  const [editing, setEditing] = useState<Source>()
  const [removing, setRemoving] = useState('')
  const [retrieval, setRetrieval] = useState<Retrieval>({
    mode: 'direct_https',
    useProxy: false,
  })
  const [accessKey, setAccessKey] = useState('')
  const [secretKey, setSecretKey] = useState('')
  const reset = () => {
    setUrl('')
    setBranch('main')
    setEditing(undefined)
    setRetrieval({ mode: 'direct_https', useProxy: false })
    setAccessKey('')
    setSecretKey('')
  }
  type Change =
    | { action: 'create'; data: MarketSourceInput }
    | { action: 'edit'; id: string; data: MarketSourceInput }
    | { action: 'delete' | 'sync'; id: string }
  const change = useMutation({
    mutationFn: (op: Change) => {
      switch (op.action) {
        case 'create':
          return createMarketSource(tenant, space, op.data)
        case 'edit':
          return editMarketSource(tenant, space, op.id, op.data)
        case 'delete':
          return deleteMarketSource(tenant, space, op.id)
        case 'sync':
          return syncMarketSource(tenant, space, op.id)
      }
    },
    onSettled: async () => {
      await Promise.all([list.refetch(), refresh()])
    },
  })
  const busy = change.isPending
  return (
    <DetailPanel
      title="管理市场"
      description="官方市场由系统维护；附加市场只对当前空间可见。"
      onClose={onClose}
    >
      {list.isError && (
        <p role="alert">
          无法读取市场。
          <Button variant="link" onClick={() => void list.refetch()}>
            重试
          </Button>
        </p>
      )}
      {officialUrl && (
        <article
          aria-label="官方市场"
          className="space-y-2 rounded-lg border p-3"
        >
          <strong className="break-all">{officialUrl}</strong>
          <p className="text-sm text-muted-foreground">
            系统内置 · 所有空间共享 ·{' '}
            {officialSyncedAt
              ? new Date(officialSyncedAt).toLocaleString()
              : '目录尚未同步'}
          </p>
          {owner && (
            <Button
              size="sm"
              disabled={busy}
              onClick={() => change.mutate({ action: 'sync', id: 'official' })}
            >
              同步
            </Button>
          )}
        </article>
      )}
      {list.data?.items.map((source) => (
        <article
          key={source.namespace}
          className="space-y-2 rounded-lg border p-3"
        >
          <strong className="break-all">{source.url}</strong>
          <p className="text-sm text-muted-foreground">
            {source.branch} · {source.enabled ? '已启用' : '已停用'} ·{' '}
            {source.syncedAt
              ? new Date(source.syncedAt).toLocaleString()
              : '尚未同步'}
          </p>
          {source.failure && <p role="alert">{source.failure}</p>}
          {owner && (
            <div className="flex flex-wrap gap-2">
              <Button
                size="sm"
                disabled={busy || !source.enabled}
                onClick={() =>
                  change.mutate({ action: 'sync', id: source.namespace })
                }
              >
                同步
              </Button>
              <Button
                variant="outline"
                size="sm"
                disabled={busy}
                onClick={() => {
                  setAccessKey('')
                  setSecretKey('')
                  setRetrieval(source.retrieval ?? { mode: 'direct_https' })
                  setEditing(source)
                  setUrl(source.url)
                  setBranch(source.branch)
                }}
              >
                编辑
              </Button>
              <Button
                variant="outline"
                size="sm"
                disabled={busy}
                onClick={() =>
                  change.mutate({
                    action: 'edit',
                    id: source.namespace,
                    data: {
                      url: source.url,
                      branch: source.branch,
                      enabled: !source.enabled,
                      retrieval: source.retrieval,
                    },
                  })
                }
              >
                {source.enabled ? '停用' : '启用'}
              </Button>
              {removing === source.namespace ? (
                <>
                  <Button
                    variant="destructive"
                    size="sm"
                    disabled={busy}
                    onClick={() =>
                      change.mutate(
                        { action: 'delete', id: source.namespace },
                        {
                          onSuccess: () => {
                            setRemoving('')
                            reset()
                          },
                        },
                      )
                    }
                  >
                    确认移除来源
                  </Button>
                  <Button
                    variant="ghost"
                    size="sm"
                    onClick={() => setRemoving('')}
                  >
                    取消
                  </Button>
                </>
              ) : (
                <Button
                  variant="ghost"
                  size="sm"
                  onClick={() => setRemoving(source.namespace)}
                >
                  移除来源
                </Button>
              )}
            </div>
          )}
        </article>
      ))}
      {owner && (
        <form
          className="space-y-3"
          onSubmit={(e) => {
            e.preventDefault()
            change.mutate(
              {
                ...(editing
                  ? { action: 'edit' as const, id: editing.namespace }
                  : { action: 'create' as const }),
                data: {
                  url: url.trim(),
                  branch: branch.trim(),
                  enabled: editing?.enabled ?? true,
                  retrieval,
                  credentials:
                    retrieval.mode === 's3_sigv4' && (accessKey || secretKey)
                      ? { accessKey, secretKey }
                      : undefined,
                },
              },
              { onSuccess: reset },
            )
          }}
        >
          <label className="block">
            HTTPS 仓库地址
            <Input
              type="url"
              required
              value={url}
              onChange={(e) => setUrl(e.target.value)}
            />
          </label>
          <label className="block">
            分支
            <Input
              required
              value={branch}
              onChange={(e) => setBranch(e.target.value)}
            />
          </label>
          <details>
            <summary className="cursor-pointer">高级设置</summary>
            <div className="mt-3 space-y-3">
              <label>
                <input
                  type="checkbox"
                  checked={retrieval.useProxy ?? false}
                  onChange={(e) =>
                    setRetrieval({ ...retrieval, useProxy: e.target.checked })
                  }
                />{' '}
                使用部署配置的市场代理
              </label>
              <label className="block">
                插件包获取方式
                <select
                  className="ml-2 rounded border p-1"
                  value={retrieval.mode}
                  onChange={(e) =>
                    setRetrieval({
                      ...retrieval,
                      mode:
                        e.target.value === 's3_sigv4'
                          ? 's3_sigv4'
                          : 'direct_https',
                    })
                  }
                >
                  <option value="direct_https">直接 HTTPS</option>
                  <option value="s3_sigv4">S3 签名下载</option>
                </select>
              </label>
              {retrieval.mode === 's3_sigv4' && (
                <>
                  {(['endpoint', 'bucket', 'region'] as const).map(
                    (field, index) => (
                      <label className="block" key={field}>
                        {['S3 端点', '存储桶', '区域'][index]}
                        <Input
                          required
                          value={retrieval[field] ?? ''}
                          onChange={(e) =>
                            setRetrieval({
                              ...retrieval,
                              [field]: e.target.value,
                            })
                          }
                        />
                      </label>
                    ),
                  )}
                  <label className="block">
                    访问密钥 ID
                    <Input
                      type="password"
                      autoComplete="new-password"
                      value={accessKey}
                      onChange={(e) => setAccessKey(e.target.value)}
                    />
                  </label>
                  <label className="block">
                    访问密钥
                    <Input
                      type="password"
                      autoComplete="new-password"
                      value={secretKey}
                      onChange={(e) => setSecretKey(e.target.value)}
                    />
                  </label>
                  {editing && (
                    <p className="text-xs text-muted-foreground">
                      保持端点和存储桶不变时，留空沿用已保存的凭据。
                    </p>
                  )}
                </>
              )}
            </div>
          </details>
          <Button type="submit" disabled={busy || !space}>
            {editing ? '保存修改' : '添加市场'}
          </Button>
          {editing && (
            <Button type="button" variant="ghost" onClick={reset}>
              取消编辑
            </Button>
          )}
        </form>
      )}
      {busy && <p role="status">正在处理…</p>}
      {change.isError && (
        <p role="alert">操作失败，请检查仓库地址、分支及空间权限。</p>
      )}
    </DetailPanel>
  )
}
