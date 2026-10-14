import { useId, useState } from 'react'
import { Button } from '@/components/ui/button'
import { Label } from '@/components/ui/label'
import { Textarea } from '@/components/ui/textarea'
import {
  endFailureMessage,
  sendFailureMessage,
  useEndThread,
  useSendThreadMessage,
  type ThreadRef,
} from './thread-api'
import { isThreadClosed, type ThreadState } from './thread-entries'

/**
 * "结束会话" behind an inline confirm step: the first click only asks, the
 * second ("确认结束") sends the request. Ending cannot be undone, so one
 * stray click must never end a session.
 */
function EndSessionControl({ threadRef, closed }: { threadRef: ThreadRef; closed: boolean }) {
  const end = useEndThread(threadRef)
  const [confirming, setConfirming] = useState(false)

  if (confirming && !closed) {
    return (
      <div className="flex items-center gap-2">
        <span className="text-xs text-muted-foreground">结束后不能再发送消息</span>
        <Button
          type="button"
          size="sm"
          variant="destructive"
          disabled={end.isPending}
          onClick={() => end.mutate({}, { onSettled: () => setConfirming(false) })}
        >
          确认结束
        </Button>
        <Button type="button" size="sm" variant="ghost" onClick={() => setConfirming(false)}>
          取消
        </Button>
      </div>
    )
  }
  return (
    <div className="space-y-1">
      <Button
        type="button"
        size="sm"
        variant="outline"
        disabled={closed}
        onClick={() => setConfirming(true)}
      >
        结束会话
      </Button>
      {end.isError && (
        <p role="alert" className="text-xs text-destructive">
          {endFailureMessage(end.error)}
        </p>
      )}
    </div>
  )
}

/**
 * Composer for one Thread: a labelled textarea, "发送" and "结束会话". Both
 * actions are disabled once the Thread is ending or ended, the same states in
 * which the server answers 409 thread_closed; a fault the server still
 * returns is shown as a readable sentence rather than a code.
 */
export function ThreadComposer({
  threadRef,
  threadState,
  canAppend,
  canEnd,
}: {
  threadRef: ThreadRef
  threadState: ThreadState
  canAppend: boolean
  canEnd: boolean
}) {
  const send = useSendThreadMessage(threadRef)
  const [text, setText] = useState('')
  const inputId = useId()
  const closed = isThreadClosed(threadState)

  function submit(event: React.FormEvent) {
    event.preventDefault()
    const message = text.trim()
    if (!message || closed || !canAppend) return
    send.mutate({ text: message }, { onSuccess: () => setText('') })
  }

  return (
    <form onSubmit={submit} className="space-y-2">
      <Label htmlFor={inputId} className="text-xs text-muted-foreground">
        给 Agent 发送消息
      </Label>
      <Textarea
        id={inputId}
        value={text}
        onChange={(event) => setText(event.target.value)}
        disabled={closed || !canAppend}
        placeholder={closed ? '会话已结束' : '输入消息…'}
        rows={3}
      />
      {!canAppend && !closed && (
        <p className="text-xs text-muted-foreground">
          此会话当前只读；只有发起者且模型连接仍有效时可继续发送。
        </p>
      )}
      {send.isError && (
        <p role="alert" className="text-xs text-destructive">
          {sendFailureMessage(send.error)}
        </p>
      )}
      <div className="flex items-start justify-between gap-2">
        {canEnd && <EndSessionControl threadRef={threadRef} closed={closed} />}
        <Button
          type="submit"
          size="sm"
          disabled={closed || !canAppend || send.isPending || !text.trim()}
        >
          发送
        </Button>
      </div>
    </form>
  )
}
