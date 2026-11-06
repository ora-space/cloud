import { createContext, useContext } from 'react'
import { useChatStore } from '../store'

/** The tenant of a run view that a page outside chat hosts; chat itself leaves it unset. */
export const RunTenantContext = createContext<string | undefined>(undefined)

/** The tenant a run view reads: its hosting page's, otherwise the one chat is connected to. */
export function useRunTenant(): string {
  const hosted = useContext(RunTenantContext)
  const connected = useChatStore((s) => s.tenantId)
  return hosted ?? connected ?? ''
}
