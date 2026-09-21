import { useMutation, useQueryClient } from '@tanstack/react-query'
import { isAxiosError } from 'axios'
import { getGetApiV1MeQueryKey, useGetApiV1Me } from '@/api/me/me'
import { AXIOS_INSTANCE } from '@/lib/api-client'

interface LoginStart {
  authorizationUrl: string
}

/** Stable authentication outcomes screens use without depending on Axios details. */
export type AuthenticationFailure = 'unauthenticated' | 'forbidden' | 'unavailable'

function authorizationURL(value: unknown): string {
  if (typeof value !== 'object' || value === null || !('authorizationUrl' in value)) {
    throw new Error('Gateway returned an invalid login response')
  }
  const raw = (value as { authorizationUrl?: unknown }).authorizationUrl
  if (typeof raw !== 'string') throw new Error('Gateway returned an invalid login response')
  const parsed = new URL(raw)
  if ((parsed.protocol !== 'https:' && parsed.protocol !== 'http:') || parsed.host === '') {
    throw new Error('Gateway returned an unsafe authorization URL')
  }
  return parsed.toString()
}

/** Classifies a current-user failure without exposing transport details to screens. */
export function classifyAuthenticationFailure(error: unknown): AuthenticationFailure {
  if (isAxiosError(error) && error.response?.status === 401) return 'unauthenticated'
  if (isAxiosError(error) && error.response?.status === 403) return 'forbidden'
  return 'unavailable'
}

/** Reads the authoritative current user through the Gateway-backed Cloud endpoint. */
export function useCurrentUser() {
  return useGetApiV1Me({ query: { retry: false, staleTime: 30_000 } })
}

/** Starts the configured external login and returns the validated browser redirect target. */
export function useStartLogin() {
  return useMutation({
    mutationFn: async (returnTo: string) => {
      const response = await AXIOS_INSTANCE.post<unknown>('/auth/login', { returnTo })
      return { authorizationUrl: authorizationURL(response.data) } satisfies LoginStart
    },
  })
}

/** Revokes the HttpOnly Gateway session and evicts the cached current-user fact. */
export function useLogout() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async () => {
      await AXIOS_INSTANCE.post('/auth/logout')
    },
    onSuccess: () => {
      queryClient.removeQueries({ queryKey: getGetApiV1MeQueryKey() })
    },
  })
}
