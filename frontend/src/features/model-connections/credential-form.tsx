import { useId, useRef, useState } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { getGetApiV1MeModelConnectionsQueryKey } from '@/api/me/me'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import {
  clearCredential,
  modelConnectionFailure,
  writeCredential,
  type ModelConnection,
} from './api'

/**
 * Write-only credential control: the input is emptied before sending, requests
 * never enter mutation caches, and only a safe text outcome is retained.
 */
export function CredentialForm({ connection }: { connection: ModelConnection }) {
  const queryClient = useQueryClient()
  const input = useRef<HTMLInputElement>(null)
  const inputId = useId()
  const [busy, setBusy] = useState(false)
  const [outcome, setOutcome] = useState<{ kind: 'error' | 'success'; message: string } | null>(
    null,
  )
  async function perform(action: () => Promise<void>) {
    setBusy(true)
    setOutcome(null)
    try {
      await action()
      await queryClient.invalidateQueries({ queryKey: getGetApiV1MeModelConnectionsQueryKey() })
      setOutcome({ kind: 'success', message: 'API Key 已更新' })
    } catch (error) {
      setOutcome({ kind: 'error', message: modelConnectionFailure(error) })
    } finally {
      setBusy(false)
    }
  }
  function submit(event: React.FormEvent) {
    event.preventDefault()
    const apiKey = input.current?.value ?? ''
    if (input.current) input.current.value = ''
    if (!apiKey.trim()) return
    void perform(() => writeCredential(connection, apiKey))
  }
  return (
    <form className="space-y-2 border-t pt-3" onSubmit={submit}>
      <p className="text-xs text-muted-foreground">
        {connection.credentialConfigured ? 'API Key 已配置' : 'API Key 未配置'}
        ；仅用于此用户的模型连接。
      </p>
      <Label htmlFor={inputId}>API Key</Label>
      <Input
        ref={input}
        id={inputId}
        type="password"
        autoComplete="new-password"
        required
        disabled={busy}
      />
      <div className="flex gap-2">
        <Button type="submit" size="sm" disabled={busy}>
          保存 API Key
        </Button>
        <Button
          type="button"
          size="sm"
          variant="outline"
          disabled={busy || !connection.credentialConfigured}
          onClick={() => {
            if (input.current) input.current.value = ''
            void perform(() => clearCredential(connection))
          }}
        >
          清除 API Key
        </Button>
      </div>
      {outcome && (
        <p role={outcome.kind === 'error' ? 'alert' : 'status'} className="text-xs">
          {outcome.message}
        </p>
      )}
    </form>
  )
}
