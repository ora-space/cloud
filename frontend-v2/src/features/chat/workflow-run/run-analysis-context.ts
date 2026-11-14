import { createContext, useContext } from 'react'

const none: ReadonlyMap<string, readonly string[]> = new Map()

/**
 * The unapplied settings v2-server's analysis of the shown run's frozen graph lists, by Agent node
 * id. The run pane provides it; outside a run view it is empty.
 */
export const RunAnalyzedUnappliedContext = createContext<ReadonlyMap<string, readonly string[]>>(none)

/** The analysis's unapplied settings for one Agent node, or undefined while unknown or unlisted. */
export function useAnalyzedUnapplied(nodeId: string | undefined): readonly string[] | undefined {
  const analyzed = useContext(RunAnalyzedUnappliedContext)
  return nodeId === undefined ? undefined : analyzed.get(nodeId)
}
