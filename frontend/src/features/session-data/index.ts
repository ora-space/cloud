/** Pure boundary adapters expose conservative snapshots to session views and query consumers. */
export { adaptArchive, adaptThread } from './adapter'
/** Public view contracts keep unknown values and source provenance explicit across modules. */
export type {
  ArchiveSessionInput,
  FrozenSessionModel,
  SessionCategory,
  SessionDiagnostic,
  SessionEvent,
  SessionSnapshot,
  SessionStatistics,
  SessionTimelineEvent,
  SessionTimelineModel,
  SessionTool,
  SessionToolSummary,
  SessionTurnMarker,
  ThreadSessionInput,
} from './types'
