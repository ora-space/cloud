import { useCallback, useRef, useState } from 'react'

/** An excerpt points to prior text while keeping the next run on the latest task revision. */
export interface ConversationQuote {
  text: string
  agentId: string
}

/** Owns one task-scoped input draft and applies quotes in the click handler, not a render effect. */
export function useConversationDraft() {
  const [mode, setMode] = useState<'agent' | 'comment'>('agent')
  const [selected, setSelected] = useState<string | null>(null)
  const [draft, setDraft] = useState('')
  const input = useRef<HTMLTextAreaElement>(null)
  const quote = useCallback(({ text, agentId }: ConversationQuote) => {
    const excerpt = text
      .slice(0, 4000)
      .split('\n')
      .map((line) => `> ${line}`)
      .join('\n')
    setDraft(
      (previous) =>
        `${previous}${previous ? '\n\n' : ''}${excerpt}${text.length > 4000 ? '\n> …' : ''}\n\n`,
    )
    setSelected(agentId)
    setMode('agent')
    input.current?.focus()
  }, [])
  return { mode, setMode, selected, setSelected, draft, setDraft, input, quote }
}
