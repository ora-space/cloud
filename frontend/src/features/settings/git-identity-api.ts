import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import type { Error as ApiError, GitIdentity } from '@/api/generated.schemas'
import {
  deleteApiV1MeGitIdentity,
  getApiV1MeGitIdentity,
  getGetApiV1MeGitIdentityQueryKey,
  putApiV1MeGitIdentity,
} from '@/api/me/me'
import { mutationHeaders, useIdempotencyKeys } from '@/features/spaces/api'
import { faultCode, type ErrorType } from '@/lib/api-client'

/** The signed-in user's Git commit identity (their own, or the derived default). */
export function useGitIdentity() {
  return useQuery({
    queryKey: getGetApiV1MeGitIdentityQueryKey(),
    queryFn: ({ signal }) => getApiV1MeGitIdentity(undefined, signal),
  })
}

/** A versioned identity write; `version` is the one the form was loaded with. */
export interface SaveGitIdentityInput {
  name: string
  email: string
  version: number
}

/**
 * Saves the identity with the optimistic version guard (version 0 creates it).
 * The response replaces the cached identity, so the next save carries the new
 * version without a refetch.
 */
export function useSaveGitIdentity() {
  const queryClient = useQueryClient()
  return useMutation<GitIdentity, ErrorType<ApiError>, SaveGitIdentityInput>({
    mutationFn: (input) => putApiV1MeGitIdentity(input),
    onSuccess: (identity) => {
      queryClient.setQueryData(getGetApiV1MeGitIdentityQueryKey(), identity)
    },
  })
}

/**
 * Drops the user's own identity so the default applies again. DELETE carries
 * an idempotency key that stays stable across retries of one click.
 */
export function useRestoreDefaultGitIdentity() {
  const queryClient = useQueryClient()
  const keyFor = useIdempotencyKeys()
  return useMutation<GitIdentity, ErrorType<ApiError>, { version: number }>({
    mutationFn: (input) =>
      deleteApiV1MeGitIdentity(input, { headers: mutationHeaders(keyFor(input)) }),
    onSuccess: (identity) => {
      queryClient.setQueryData(getGetApiV1MeGitIdentityQueryKey(), identity)
    },
  })
}

const GIT_IDENTITY_FAULTS: Record<string, string> = {
  invalid_git_name: '名称需为 1–200 个字符，不能包含换行或尖括号',
  invalid_git_email: '邮箱格式不正确',
  version_conflict: '身份已在别处修改，请刷新后重试',
}

/** User-facing reason a save or restore failed, keyed by the public Fault code. */
export function gitIdentityFailureMessage(error: unknown): string {
  return GIT_IDENTITY_FAULTS[faultCode(error) ?? ''] ?? '保存失败，请稍后重试'
}
