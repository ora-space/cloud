import { Button } from '@/components/ui/button'
import type { AgentConfiguration } from './api'
import { AgentConnectionPicker } from './connections'
import type { AgentEditorState } from './editor-state'
import { AgentSettingFields } from './setting-fields'

/** Renders an edit's fixed declaration and explicit save/reload/clear actions. */
export function AgentOptionsForm({
  context,
  editor,
}: {
  context: AgentConfiguration
  editor: AgentEditorState
}) {
  return (
    <form className="space-y-4" onSubmit={editor.submit}>
      {!editor.draft.usesGlobalDefault && (
        <AgentConnectionPicker
          context={context}
          draft={editor.draft}
          connections={editor.connections.data ?? []}
          onChange={editor.selectConnection}
        />
      )}
      <AgentSettingFields
        settings={editor.draft.schema}
        values={editor.draft.values}
        onChange={editor.changeSetting}
      />
      {editor.draft.requiresRepair && (
        <RepairNotice
          removedFields={editor.draft.removedFields}
          accepted={editor.acceptedRepair}
          onChange={editor.setAcceptedRepair}
        />
      )}
      {!editor.body && (
        <p className="text-xs text-muted-foreground">请完成模型和必填选项，并检查数字格式。</p>
      )}
      <div className="flex flex-wrap gap-2">
        <Button type="submit" disabled={!editor.canSave}>
          {editor.draft.requiresRepair ? '确认修复并保存' : '保存我的配置'}
        </Button>
        <Button
          type="button"
          variant="outline"
          disabled={editor.busy}
          onClick={() => {
            void editor.reload()
          }}
        >
          重新核对最新配置
        </Button>
        <Button
          type="button"
          variant="ghost"
          disabled={editor.busy || editor.draft.revision === 0}
          onClick={() => editor.setConfirmClear(true)}
        >
          清除个人选项
        </Button>
      </div>
    </form>
  )
}

function RepairNotice({
  removedFields,
  accepted,
  onChange,
}: {
  removedFields: string[]
  accepted: boolean
  onChange: (value: boolean) => void
}) {
  return (
    <div className="space-y-2 rounded-md border p-3">
      <p className="text-sm">
        插件配置要求已变化，请按本版本重新确认。此配置在其他空间也会按该版本检查。
      </p>
      {removedFields.length > 0 && (
        <p className="text-xs">本次确认将移除旧选项：{removedFields.join('、')}</p>
      )}
      <label className="flex items-center gap-2 text-sm">
        <input
          type="checkbox"
          checked={accepted}
          onChange={(event) => onChange(event.target.checked)}
        />
        我已核对当前版本的选项和模型
      </label>
    </div>
  )
}

/** Confirmation is separate from the options form, so credential and connection forms stay valid HTML. */
export function ClearAgentOptions({ editor }: { editor: AgentEditorState }) {
  return (
    <div className="space-y-2 rounded-md border p-3">
      <p className="text-sm">清除插件选项与模型引用？个人连接、API Key 和已有运行仍保留。</p>
      <Button
        type="button"
        variant="destructive"
        disabled={editor.busy}
        onClick={editor.clearOptions}
      >
        确认清除
      </Button>
      <Button type="button" variant="ghost" onClick={() => editor.setConfirmClear(false)}>
        取消清除
      </Button>
    </div>
  )
}
