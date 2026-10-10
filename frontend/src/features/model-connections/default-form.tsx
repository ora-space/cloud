import { useState } from 'react'
import { Button } from '@/components/ui/button'
import {
  modelConnectionFailure,
  useSaveModelDefault,
  type ModelConnection,
  type ModelDefault,
} from './api'
import { ConnectionChoice } from './fields'

/** Versioned default selector; only enabled, credential-backed connections are offered. */
export function ModelDefaultForm({
  connections,
  selection,
}: {
  connections: ModelConnection[]
  selection: ModelDefault
}) {
  const ready = connections.filter(
    (connection) => connection.enabled && connection.credentialConfigured,
  )
  const current = connections.find((connection) => connection.id === selection.connectionId)
  const currentModel = current?.models.find((model) => model.id === selection.modelId)
  let summary = '尚未选择默认模型'
  if (current && currentModel) {
    summary =
      current.enabled && current.credentialConfigured
        ? `当前默认：${current.name} · ${currentModel.name}`
        : '当前默认不可用，请启用连接并配置 API Key。'
  }
  const [connectionId, setConnectionId] = useState(selection.connectionId)
  const [modelId, setModelId] = useState(selection.modelId)
  const models = ready.find((connection) => connection.id === connectionId)?.models ?? []
  const save = useSaveModelDefault()
  const valid = models.some((model) => model.id === modelId)
  return (
    <form
      className="space-y-3 rounded-lg border p-4"
      onSubmit={(event) => {
        event.preventDefault()
        if (valid) save.mutate({ connectionId, modelId, version: selection.version })
      }}
    >
      <h3 className="font-medium">默认模型</h3>
      <p className="text-xs text-muted-foreground">
        新会话使用此默认模型。修改只对之后启动的会话生效。
      </p>
      <p role="status" className="text-xs">
        {summary}
      </p>
      {ready.length === 0 ? (
        <p className="text-sm">请先创建连接并配置 API Key。</p>
      ) : (
        <>
          <ConnectionChoice
            label="默认连接"
            value={connectionId}
            options={ready.map((connection) => ({ value: connection.id, label: connection.name }))}
            onChange={(id) => {
              setConnectionId(id)
              setModelId('')
            }}
          />
          <ConnectionChoice
            label="默认模型"
            value={modelId}
            options={models.map((model) => ({
              value: model.id,
              label: `${model.name} (${model.id})`,
            }))}
            onChange={setModelId}
          />
          <Button type="submit" disabled={save.isPending || !valid}>
            保存默认模型
          </Button>
        </>
      )}
      {save.isError && (
        <p role="alert" className="text-xs text-destructive">
          {modelConnectionFailure(save.error)}
        </p>
      )}
      {save.isSuccess && (
        <p role="status" className="text-xs">
          默认模型已保存
        </p>
      )}
    </form>
  )
}
