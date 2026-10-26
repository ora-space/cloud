import { ModelDefaultForm } from '@/features/model-connections/default-form'
import { agentConfigurationFailure } from './api'
import { AgentConnectionTools } from './connections'
import { useAgentEditor, type AgentEditorProps, type AgentEditorState } from './editor-state'
import { AgentOptionsForm, ClearAgentOptions } from './options-form'
import { AgentConfigurationReconcile } from './reconcile'

/** Composes independent option, default-selection and credential forms for one editing session. */
export function AgentConfigurationEditor(props: AgentEditorProps) {
  const editor = useAgentEditor(props)
  const { draft, globalDefault, connections } = editor
  return (
    <section className="space-y-4">
      <p className="text-xs text-muted-foreground">
        配置只属于当前账号。修改影响之后的新运行；保存成功还需通过运行时的能力和模型检查。
      </p>
      {draft.usesGlobalDefault && globalDefault.data && connections.data && (
        <ModelDefaultForm
          connections={connections.data}
          selection={globalDefault.data}
          onConnectionChange={editor.followDefaultConnection}
        />
      )}
      <AgentOptionsForm context={props.context} editor={editor} />
      <EditorMessages editor={editor} />
      {editor.comparing && (
        <AgentConfigurationReconcile latest={props.context} onChoose={editor.choose} />
      )}
      {editor.confirmClear && <ClearAgentOptions editor={editor} />}
      <AgentConnectionTools
        context={props.context}
        selected={editor.credentialTarget}
        onCreated={editor.createdConnection}
      />
    </section>
  )
}

function EditorMessages({ editor }: { editor: AgentEditorState }) {
  return (
    <>
      {(editor.save.isError || editor.clear.isError) && (
        <p role="alert" className="text-sm text-destructive">
          {agentConfigurationFailure(editor.save.error ?? editor.clear.error)}
        </p>
      )}
      {editor.reloadFailed && <p role="alert">最新配置读取失败，草稿仍保留，请重试。</p>}
      {(editor.connections.isError ||
        (editor.globalDefault.isError && editor.draft.usesGlobalDefault)) && (
        <p role="alert">个人模型信息读取失败，请稍后重试。</p>
      )}
    </>
  )
}
