import {
  ArrowRight,
  Boxes,
  Braces,
  CornerDownRight,
  GitBranch,
  GitMerge,
  Layers,
  Merge,
  Play,
  Repeat,
  UserCheck,
  Zap,
  type LucideIcon,
} from 'lucide-react'
import type { WorkflowNodeKind } from '@/features/workflows/runtime/types'

/** Stable visual identity of one node kind. */
export interface WorkflowNodeMetadata {
  kind: WorkflowNodeKind
  icon: LucideIcon
  /** Tailwind classes tinting the icon tile; the kind's only colour cue. */
  tone: string
}

/**
 * One icon and one tone per node kind, in a record keyed by the kind union so
 * adding a kind to `WORKFLOW_NODE_KINDS` without a look here is a type error
 * rather than a card that renders a blank tile.
 */
const NODE_METADATA: Record<WorkflowNodeKind, WorkflowNodeMetadata> = {
  start: {
    kind: 'start',
    icon: Play,
    tone: 'bg-emerald-500/12 text-emerald-700 dark:text-emerald-400',
  },
  agent: { kind: 'agent', icon: Zap, tone: 'bg-blue-500/12 text-blue-700 dark:text-blue-400' },
  condition: {
    kind: 'condition',
    icon: GitBranch,
    tone: 'bg-amber-500/12 text-amber-700 dark:text-amber-400',
  },
  aggregator: {
    kind: 'aggregator',
    icon: Merge,
    tone: 'bg-orange-500/12 text-orange-700 dark:text-orange-400',
  },
  tool: { kind: 'tool', icon: Braces, tone: 'bg-cyan-500/12 text-cyan-700 dark:text-cyan-400' },
  junction: {
    kind: 'junction',
    icon: GitMerge,
    tone: 'bg-teal-500/12 text-teal-700 dark:text-teal-400',
  },
  human: { kind: 'human', icon: UserCheck, tone: 'bg-sky-500/12 text-sky-700 dark:text-sky-400' },
  loop: {
    kind: 'loop',
    icon: Repeat,
    tone: 'bg-indigo-500/12 text-indigo-700 dark:text-indigo-400',
  },
  loopExit: {
    kind: 'loopExit',
    icon: CornerDownRight,
    tone: 'bg-indigo-500/12 text-indigo-700 dark:text-indigo-400',
  },
  iteration: {
    kind: 'iteration',
    icon: Layers,
    tone: 'bg-violet-500/12 text-violet-700 dark:text-violet-400',
  },
  subflow: {
    kind: 'subflow',
    icon: Boxes,
    tone: 'bg-fuchsia-500/12 text-fuchsia-700 dark:text-fuchsia-400',
  },
  output: {
    kind: 'output',
    icon: ArrowRight,
    tone: 'bg-rose-500/12 text-rose-700 dark:text-rose-400',
  },
}

/**
 * Resolves the visual identity of one node kind.
 *
 * @param kind - Kind to look up.
 * @returns The icon and tone the card and the inspector header both render.
 */
export function workflowNodeMetadata(kind: WorkflowNodeKind): WorkflowNodeMetadata {
  return NODE_METADATA[kind]
}
