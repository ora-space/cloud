import { useState } from 'react'
import {
  useModelConnections,
  useModelDefault,
  type ModelConnection,
} from '@/features/model-connections/api'
import {
  useSaveAgentConfiguration,
  useClearAgentConfiguration,
  type AgentConfiguration,
  type AgentConfigurationScope,
} from './api'
import {
  agentConfigurationBody,
  agentConfigurationDraft,
  agentConfigurationNumbers,
  type AgentConfigurationDraft,
  type AgentSettingValue,
} from './draft'

/**
 * Resolves the connection whose write-only credential controls this session shows.
 * Global-default plugins persist no connection, so the default draft or an inline
 * creation names the target until the user chooses another connection there.
 */
function useCredentialTarget(draft: AgentConfigurationDraft) {
  const [focus, setFocus] = useState<string>()
  const connections = useModelConnections()
  const globalDefault = useModelDefault(draft.usesGlobalDefault)
  // This editing baseline is captured when the default form can first mount; later reads keep its draft.
  if (draft.usesGlobalDefault && focus === undefined && connections.data && globalDefault.data) {
    setFocus(globalDefault.data.connectionId)
  }
  const connectionId = draft.usesGlobalDefault
    ? (focus ?? globalDefault.data?.connectionId)
    : draft.connectionId
  return {
    connections,
    globalDefault,
    target: connections.data?.find((connection) => connection.id === connectionId),
    focus: setFocus,
  }
}

/**
 * Whether a read after a write shows exactly the revision that write confirmed, in the
 * release context the draft was edited against. Anything else is another editor's write or
 * a changed selection the user has not compared their draft with.
 */
function confirmsWrite(
  latest: AgentConfiguration,
  revision: number,
  draft: AgentConfigurationDraft,
) {
  return (
    latest.revision === revision &&
    latest.selectionVersion === draft.selectionVersion &&
    latest.declaration.fingerprint === draft.fingerprint
  )
}

function withSetting(
  draft: AgentConfigurationDraft,
  id: string,
  value: AgentSettingValue | undefined,
): AgentConfigurationDraft {
  const values = { ...draft.values }
  if (value === undefined) delete values[id]
  else values[id] = value
  return { ...draft, values }
}

/**
 * Input to an editing session. The dialog owns the read query, so both callbacks
 * resolve with the freshest authoritative context instead of triggering a remount.
 */
export type AgentEditorProps = {
  context: AgentConfiguration
  scope: AgentConfigurationScope
  onReload: () => Promise<AgentConfiguration>
  onSaved: () => Promise<AgentConfiguration>
}

/** Owns draft baselines and explicit reconciliation without copying live server state. */
export function useAgentEditor({ context, scope, onReload, onSaved }: AgentEditorProps) {
  const [draft, setDraft] = useState(() => agentConfigurationDraft(context))
  const [acceptedRepair, setAcceptedRepair] = useState(false)
  const [comparing, setComparing] = useState(false)
  const [confirmClear, setConfirmClear] = useState(false)
  const [reloadFailed, setReloadFailed] = useState(false)
  const save = useSaveAgentConfiguration(scope)
  const clear = useClearAgentConfiguration(scope)
  const { connections, globalDefault, target, focus } = useCredentialTarget(draft)
  const body = agentConfigurationBody(draft)
  const busy = save.isPending || clear.isPending
  const canSave =
    !busy && body !== undefined && (!draft.requiresRepair || acceptedRepair) && !comparing
  async function reload() {
    setReloadFailed(false)
    try {
      await onReload()
      setComparing(true)
    } catch {
      setReloadFailed(true)
    }
  }
  function choose(keepDraft: boolean) {
    setDraft(agentConfigurationDraft(context, keepDraft ? draft : undefined))
    setAcceptedRepair(false)
    setComparing(false)
    save.reset()
    clear.reset()
  }
  function changeSetting(id: string, value: AgentSettingValue | undefined) {
    setDraft((current) => withSetting(current, id, value))
  }
  function selectConnection(connectionId: string, modelId: string) {
    setDraft((current) => ({ ...current, connectionId, modelId }))
  }
  function createdConnection(connection: ModelConnection) {
    if (draft.usesGlobalDefault) {
      focus(connection.id)
      return
    }
    selectConnection(connection.id, connection.models[0]?.id ?? '')
  }
  /**
   * Advances only this session's option baseline, and only to the revision this write
   * confirmed. The follow-up read is adopted in place when it is that revision in the same
   * release context; values stay as submitted (or fall back to defaults after a clear). A
   * read showing another editor's later write or a changed selection keeps the draft on the
   * confirmed revision and requires an explicit reconciliation, so a retained draft never
   * gains a newer CAS revision it was not compared against. The default-model draft keeps
   * its own state and CAS version, so an unsaved default selection never silently reverts.
   */
  async function adoptWrite(written: { revision: number; cleared: boolean }) {
    setDraft((current) => ({ ...current, revision: written.revision, requiresRepair: false }))
    try {
      const latest = await onSaved()
      const confirmed = confirmsWrite(latest, written.revision, draft)
      if (confirmed)
        setDraft((current) =>
          agentConfigurationDraft(latest, written.cleared ? undefined : current),
        )
      setComparing(!confirmed)
      setAcceptedRepair(false)
      setConfirmClear(false)
      setReloadFailed(false)
    } catch {
      setReloadFailed(true)
    }
  }
  function submit(event: React.FormEvent) {
    event.preventDefault()
    if (canSave && body)
      save.mutate(
        { body, repair: draft.requiresRepair, numbers: agentConfigurationNumbers(draft) },
        { onSuccess: (written) => void adoptWrite(written.resource) },
      )
  }
  function clearOptions() {
    clear.mutate(
      { revision: draft.revision },
      { onSuccess: (written) => void adoptWrite(written.resource) },
    )
  }
  return {
    draft,
    body,
    busy,
    canSave,
    acceptedRepair,
    setAcceptedRepair,
    comparing,
    confirmClear,
    setConfirmClear,
    reloadFailed,
    save,
    clear,
    connections,
    globalDefault,
    credentialTarget: target,
    reload,
    choose,
    changeSetting,
    selectConnection,
    createdConnection,
    followDefaultConnection: focus,
    submit,
    clearOptions,
  }
}

/** State and named actions shared only by the editor's presentational pieces. */
export type AgentEditorState = ReturnType<typeof useAgentEditor>
