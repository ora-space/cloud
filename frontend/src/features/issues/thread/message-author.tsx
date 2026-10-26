import { createContext, useContext } from 'react'
import { ActorAvatar } from '@/components/common/actor-avatar'

const Speaker = createContext('Agent')

/** Supplies the resolved execution identity without teaching the transcript about target discovery. */
export function AgentSpeaker({ name, children }: { name: string; children: React.ReactNode }) {
  return <Speaker.Provider value={name}>{children}</Speaker.Provider>
}

/** An unresolved historical target remains an Agent instead of borrowing a different target's name. */
export function useAgentSpeaker() {
  return useContext(Speaker)
}

/** Shared author line; dates come only from persisted message records. */
export function MessageAuthor({
  name,
  type = 'user',
  at,
}: {
  name: string
  type?: 'user' | 'agent' | 'team'
  at?: string | undefined
}) {
  const date = at ? new Date(at) : undefined
  return (
    <div className="mb-2 flex items-center gap-2 text-xs text-muted-foreground">
      <span aria-hidden="true">
        <ActorAvatar actor={{ name, type }} size="sm" />
      </span>
      <span className="font-medium text-foreground">{name}</span>
      {date && Number.isFinite(date.getTime()) && (
        <time dateTime={at}>{date.toLocaleString()}</time>
      )}
    </div>
  )
}
