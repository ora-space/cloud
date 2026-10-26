import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import type {
  PersonalAgentConfigurationContext,
  PutApiV1MeAgentConfigurationsSpaceIdPluginNamespacePluginIdentifierBody,
} from '@/api/generated.schemas'
import {
  deleteApiV1MeAgentConfigurationsSpaceIdPluginNamespacePluginIdentifier as clearConfiguration,
  getApiV1MeAgentConfigurationsSpaceIdPluginNamespacePluginIdentifier as getConfiguration,
  postApiV1MeAgentConfigurationsSpaceIdPluginNamespacePluginIdentifierRepair as repairConfiguration,
  putApiV1MeAgentConfigurationsSpaceIdPluginNamespacePluginIdentifier as putConfiguration,
} from '@/api/me/me'
import { mutationHeaders, useIdempotencyKeys } from '@/features/spaces/api'
import { faultCode } from '@/lib/api-client'
import {
  agentConfigurationResponse,
  serializeAgentConfiguration,
  type AgentNumberLiterals,
} from './numbers'

/** One user's configuration, resolved in the selected space's pinned release context. */
export type AgentConfiguration = PersonalAgentConfigurationContext & {
  numericValues?: AgentNumberLiterals
  numericDefaults?: AgentNumberLiterals
}
/** Complete, versioned non-secret options submitted to the server. */
export type AgentConfigurationWrite =
  PutApiV1MeAgentConfigurationsSpaceIdPluginNamespacePluginIdentifierBody
/** An editing context; canonical plugin identity includes both source and identifier. */
export type AgentConfigurationScope = { spaceId: string; pluginId: string }

/** Stable cache scope; personal data is discarded by the session's QueryClient reset. */
export function agentConfigurationKey(scope: AgentConfigurationScope) {
  return ['personal-agent-configuration', scope.spaceId, scope.pluginId]
}

function pluginIdentity(pluginId: string): [string, string] {
  const parts = pluginId.split('/')
  const source = parts[0]
  const identifier = parts[1]
  if (parts.length !== 2 || !source || !identifier) throw new Error('invalid_plugin_id')
  return [source, identifier]
}

/** Reads the authenticated user's values; it never starts an Agent or reads a credential. */
export function useAgentConfiguration(scope: AgentConfigurationScope, enabled: boolean) {
  const [source, identifier] = pluginIdentity(scope.pluginId)
  return useQuery({
    queryKey: agentConfigurationKey(scope),
    queryFn: ({ signal }) =>
      getConfiguration(
        scope.spaceId,
        source,
        identifier,
        { transformResponse: agentConfigurationResponse },
        signal,
      ),
    enabled,
    retry: false,
  })
}

/** Writes only non-secret configuration. Conflicts are surfaced without automatic retries. */
export function useSaveAgentConfiguration(scope: AgentConfigurationScope) {
  const queryClient = useQueryClient()
  const keyFor = useIdempotencyKeys()
  const [source, identifier] = pluginIdentity(scope.pluginId)
  return useMutation({
    mutationFn: (input: {
      body: AgentConfigurationWrite
      repair: boolean
      numbers: AgentNumberLiterals
    }) => {
      const options = {
        headers: mutationHeaders(keyFor(input)),
        transformRequest: () => serializeAgentConfiguration(input.body, input.numbers),
      }
      const write = input.repair ? repairConfiguration : putConfiguration
      return write(scope.spaceId, source, identifier, input.body, options)
    },
    onSuccess: () => queryClient.invalidateQueries({ queryKey: agentConfigurationKey(scope) }),
  })
}

/** Clearing options advances their revision; the shared personal connection remains intact. */
export function useClearAgentConfiguration(scope: AgentConfigurationScope) {
  const queryClient = useQueryClient()
  const keyFor = useIdempotencyKeys()
  const [source, identifier] = pluginIdentity(scope.pluginId)
  return useMutation({
    mutationFn: (body: { revision: number }) =>
      clearConfiguration(scope.spaceId, source, identifier, body, {
        headers: mutationHeaders(keyFor(body)),
      }),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: agentConfigurationKey(scope) }),
  })
}

const FAILURE_MESSAGES: Record<string, string> = {
  revision_conflict: '配置已在别处修改，当前草稿已保留。请重新核对后保存。',
  agent_release_conflict: '空间选用的插件版本已变化，当前草稿已保留。请重新核对。',
  agent_declaration_conflict: '插件配置要求已变化，当前草稿已保留。请重新核对。',
  agent_configuration_repair_required: '请按当前插件版本确认修复配置。',
  agent_declaration_required: '这个插件版本尚未提供个人配置，请联系空间管理员升级插件。',
  agent_declaration_invalid: '这个插件版本的配置声明不可用，请联系空间管理员。',
  agent_plugin_unavailable: '当前空间尚未选用可配置的插件版本。',
  agent_setting_required: '请填写必填选项。',
  invalid_agent_setting_type: '选项类型不正确，请检查输入。',
  unknown_agent_setting: '插件已移除部分选项，请重新核对。',
  model_not_found: '所选模型已不可用，请重新选择。',
  model_connection_disabled: '所选连接已停用，请重新选择。',
  agent_model_protocol_mismatch: '连接协议与此插件不匹配，请重新选择。',
  not_found: '配置或个人连接已不可用，请重新加载。',
}

/** Converts bounded public codes to user-facing text, without exposing request/error objects. */
export function agentConfigurationFailure(error: unknown): string {
  return FAILURE_MESSAGES[faultCode(error) ?? ''] ?? '配置操作失败，请稍后重试。'
}
