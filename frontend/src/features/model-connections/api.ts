import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import {
  deleteApiV1MeModelConnectionsMcid,
  deleteApiV1MeModelConnectionsMcidCredential,
  getApiV1MeModelConnections,
  getApiV1MeModelDefault,
  getGetApiV1MeModelConnectionsQueryKey,
  getGetApiV1MeModelDefaultQueryKey,
  postApiV1MeModelConnections,
  putApiV1MeModelConnectionsMcid,
  putApiV1MeModelConnectionsMcidCredential,
  putApiV1MeModelDefault,
} from '@/api/me/me'
import { idempotencyKeyFor, mutationHeaders, useIdempotencyKeys } from '@/features/spaces/api'
import { faultCode } from '@/lib/api-client'
import { readAllPages } from '@/lib/pagination'

/** Public connection metadata; credentials are never part of this resource. */
export type ModelConnection = Awaited<
  ReturnType<typeof getApiV1MeModelConnections>
>['items'][number]

/** Editable metadata sent separately from a write-only credential. */
export type ConnectionDraft = Pick<
  ModelConnection,
  'name' | 'protocol' | 'baseUrl' | 'authMode' | 'models' | 'enabled'
>

/** The signed-in user's selected connection and model, fenced by its own version. */
export type ModelDefault = Awaited<ReturnType<typeof getApiV1MeModelDefault>>

/** Loads every private connection without placing credentials in the query cache. */
export function useModelConnections() {
  return useQuery({
    queryKey: getGetApiV1MeModelConnectionsQueryKey(),
    queryFn: async ({ signal }) => {
      const page = await readAllPages((after) =>
        getApiV1MeModelConnections(
          after ? { after, limit: 100 } : { limit: 100 },
          undefined,
          signal,
        ),
      )
      return page.items
    },
  })
}

/** Reads the default used only when a new model-backed run is created. */
export function useModelDefault() {
  return useQuery({
    queryKey: getGetApiV1MeModelDefaultQueryKey(),
    queryFn: ({ signal }) => getApiV1MeModelDefault(undefined, signal),
  })
}

/** Creates metadata or replaces it with the form's loaded version. */
export function useSaveConnection(connection: ModelConnection | undefined) {
  const queryClient = useQueryClient()
  const keyFor = useIdempotencyKeys()
  return useMutation({
    mutationFn: async (input: ConnectionDraft) => {
      const result = connection
        ? await putApiV1MeModelConnectionsMcid(connection.id, {
            ...input,
            version: connection.version,
          })
        : await postApiV1MeModelConnections(input, { headers: mutationHeaders(keyFor(input)) })
      return result.resource
    },
    onSuccess: () =>
      queryClient.invalidateQueries({ queryKey: getGetApiV1MeModelConnectionsQueryKey() }),
  })
}

/** Deletes one connection with its version and a stable submission identity. */
export function useDeleteConnection(connection: ModelConnection) {
  const queryClient = useQueryClient()
  const keyFor = useIdempotencyKeys()
  return useMutation({
    mutationFn: (input: { version: number }) =>
      deleteApiV1MeModelConnectionsMcid(connection.id, input, {
        headers: mutationHeaders(keyFor(input)),
      }),
    onSuccess: async () => {
      await Promise.all([
        queryClient.invalidateQueries({ queryKey: getGetApiV1MeModelConnectionsQueryKey() }),
        queryClient.invalidateQueries({ queryKey: getGetApiV1MeModelDefaultQueryKey() }),
      ])
    },
  })
}

/** Chooses a ready connection/model; the response replaces the safe default cache. */
export function useSaveModelDefault() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (input: ModelDefault) => putApiV1MeModelDefault(input),
    onSuccess: (result) => queryClient.setQueryData(getGetApiV1MeModelDefaultQueryKey(), result),
  })
}

/**
 * Writes a secret outside TanStack mutation state. The caller clears the input
 * immediately; a rejection is reduced to its safe fault code because HTTP errors
 * retain request configuration, which can contain the submitted credential.
 */
export async function writeCredential(connection: ModelConnection, apiKey: string): Promise<void> {
  try {
    const operation = { connectionId: connection.id, version: connection.version }
    const headers = mutationHeaders(idempotencyKeyFor({ current: null }, operation))
    await putApiV1MeModelConnectionsMcidCredential(
      connection.id,
      {
        apiKey,
        version: connection.version,
      },
      { headers },
    )
  } catch (error) {
    // oxlint-disable-next-line preserve-caught-error -- HTTP causes retain secret request configuration and must not escape this boundary.
    throw new Error(faultCode(error) ?? 'internal')
  }
}

/** Clears the write-only credential; raw values never enter this request. */
export async function clearCredential(connection: ModelConnection): Promise<void> {
  try {
    await deleteApiV1MeModelConnectionsMcidCredential(connection.id, {
      version: connection.version,
    })
  } catch (error) {
    // oxlint-disable-next-line preserve-caught-error -- Credential errors share the same safe fault-only boundary as writes.
    throw new Error(faultCode(error) ?? 'internal')
  }
}

const FAULT_MESSAGES: Record<string, string> = {
  version_conflict: '配置已在别处修改，请刷新后重试',
  invalid_model_connection: '请检查连接地址、认证方式和模型信息',
  invalid_model_protocol: '请选择支持的模型协议',
  invalid_model_auth: '该协议不支持所选认证方式',
  invalid_model_url: '服务地址需为公共 HTTPS 地址，不能包含账号、查询参数或片段',
  invalid_model_list: '连接需包含 1–100 个模型',
  invalid_model_id: '模型标识不可为空、重复或包含换行',
  invalid_model_limits: '输出上限需为正整数且不能超过上下文上限',
  invalid_model_default: '请选择已启用且配置了 API Key 的连接和模型',
  model_credential_required: '请先配置 API Key',
  model_connection_unavailable: '模型连接当前不可用，请检查启用状态和 API Key',
  invalid_model_credential: 'API Key 不可为空或包含换行',
}

/** Maps public codes to bounded messages without rendering upstream error details. */
export function modelConnectionFailure(error: unknown): string {
  const code = faultCode(error) ?? (error instanceof Error ? error.message : '')
  return FAULT_MESSAGES[code] ?? '操作失败，请稍后重试'
}
