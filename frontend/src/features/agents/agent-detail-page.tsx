import { AgentsPage } from './agents-page'

/** Retains old Agent detail links without resolving their ids against demo records. */
export function AgentDetailPage({ slug }: { slug: string }) {
  return <AgentsPage slug={slug} />
}
