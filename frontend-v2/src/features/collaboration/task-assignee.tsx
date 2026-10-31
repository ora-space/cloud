import { useGetApiV1TenantsTidMembers } from '@/api/members/members'
import { useGetApiV1TenantsTidSpaces, useGetApiV1TenantsTidSpacesSpaceIdPlugins } from '@/api/spaces/spaces'
import { NativeSelect, NativeSelectOptGroup, NativeSelectOption } from '@/components/ui/native-select'
import type { SquadSummary } from './api/generated'
import { pluginLabel } from './member-model'
import type { TaskAssigneeRef } from './tasks-api/generated'

/** The select value for one typed reference; '' means unassigned. */
const encode = (value: TaskAssigneeRef | null): string => (value ? `${value.type}:${value.id}` : '')

/** Reads back a select value; only the three typed kinds the server accepts are understood. */
function decode(raw: string): TaskAssigneeRef | null {
  if (!raw) return null
  const [type, id] = raw.split(':')
  if (!id || (type !== 'human' && type !== 'agent' && type !== 'squad')) return null
  return { type, id }
}

/**
 * Picks the single typed assignee of a task. Candidates are the workspace's active members, the
 * space's installed agents and the live squads, all read from routes that already scope them to
 * this workspace, so no fabricated or foreign target can be chosen.
 */
export function TaskAssigneePicker({ tenantId, squads, value, disabled, onAssign }: {
  tenantId: string
  /** Live squads of the workspace, already loaded by the page. */
  squads: SquadSummary[]
  /** The task's current assignee; null means unassigned. */
  value: TaskAssigneeRef | null
  /** A write is in flight. */
  disabled: boolean
  onAssign: (next: TaskAssigneeRef | null) => void
}) {
  const roster = useGetApiV1TenantsTidMembers(tenantId, undefined, { query: { enabled: !!tenantId } })
  const spaces = useGetApiV1TenantsTidSpaces(tenantId, undefined, { query: { enabled: !!tenantId } })
  const spaceId = spaces.data?.items[0]?.id ?? ''
  const plugins = useGetApiV1TenantsTidSpacesSpaceIdPlugins(tenantId, spaceId, { query: { enabled: !!tenantId && !!spaceId } })
  const people = (roster.data?.items ?? []).filter(member => member.status === 'active')
  const agents = (plugins.data?.items ?? []).filter(plugin => plugin.kind === 'agent' && plugin.observedState === 'installed')
  const label = roster.isLoading || plugins.isLoading ? '正在加载指派候选…' : roster.isError || plugins.isError ? '候选加载失败，请关闭后重试。' : ''

  return <div className="mc-assignee">
    <NativeSelect aria-label="指派给" value={encode(value)} disabled={disabled} onChange={event => onAssign(decode(event.target.value))}>
      <NativeSelectOption value="">未指派</NativeSelectOption>
      {people.length > 0 && <NativeSelectOptGroup label="工作区成员">
        {people.map(member => <NativeSelectOption key={member.userId} value={`human:${member.userId}`}>{member.displayName}</NativeSelectOption>)}
      </NativeSelectOptGroup>}
      {agents.length > 0 && <NativeSelectOptGroup label="空间 Agent">
        {agents.map(plugin => <NativeSelectOption key={plugin.id} value={`agent:${plugin.id}`}>{pluginLabel(plugin)}</NativeSelectOption>)}
      </NativeSelectOptGroup>}
      {squads.length > 0 && <NativeSelectOptGroup label="小队">
        {squads.map(squad => <NativeSelectOption key={squad.id} value={`squad:${squad.id}`}>{squad.name}</NativeSelectOption>)}
      </NativeSelectOptGroup>}
    </NativeSelect>
    {label && <p role="status" className="mc-empty-line">{label}</p>}
  </div>
}
