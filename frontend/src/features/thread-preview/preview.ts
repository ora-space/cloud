import type { ThreadEntry, ThreadPreviewSnapshot } from '@/api/generated.schemas'

/** No cached producer is distinct from a connected producer with an empty pending set. */
export function emptyPreview(): ThreadPreviewSnapshot {
  return { available: false, executionId: '', version: 0, blocks: [], incomplete: false }
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return value !== null && typeof value === 'object' && !Array.isArray(value)
}

function previewBlock(value: unknown): ThreadPreviewSnapshot['blocks'][number] | null {
  if (
    !isRecord(value) ||
    typeof value['historySequence'] !== 'number' ||
    !Number.isSafeInteger(value['historySequence']) ||
    value['historySequence'] < 0 ||
    value['historySequence'] > 4294967295 ||
    typeof value['turnId'] !== 'string' ||
    !isRecord(value['record'])
  )
    return null
  return {
    historySequence: value['historySequence'],
    turnId: value['turnId'],
    record: value['record'],
  }
}

/** Validates optional stream content before it can enter the task's query cache. */
export function parseThreadPreview(data: string): ThreadPreviewSnapshot | null {
  if (data.length > 512 * 1024) return null
  let value: unknown
  try {
    value = JSON.parse(data)
  } catch {
    return null
  }
  if (
    !isRecord(value) ||
    typeof value['available'] !== 'boolean' ||
    typeof value['incomplete'] !== 'boolean' ||
    typeof value['executionId'] !== 'string' ||
    typeof value['version'] !== 'number' ||
    !Number.isSafeInteger(value['version']) ||
    value['version'] < 0 ||
    !Array.isArray(value['blocks']) ||
    value['blocks'].length > 32
  )
    return null
  const seen = new Set<number>()
  const blocks: ThreadPreviewSnapshot['blocks'] = []
  for (const raw of value['blocks']) {
    const block = previewBlock(raw)
    if (!block || seen.has(block.historySequence)) return null
    seen.add(block.historySequence)
    blocks.push(block)
  }
  return {
    available: value['available'],
    incomplete: value['incomplete'],
    executionId: value['executionId'],
    version: value['version'],
    blocks,
  }
}

/** Whole snapshots replace content; duplicate or late versions cannot append text a second time. */
export function adoptThreadPreview(
  current: ThreadPreviewSnapshot,
  next: ThreadPreviewSnapshot,
): ThreadPreviewSnapshot {
  if (!next.available) return emptyPreview()
  if (current.executionId && current.executionId !== next.executionId) return current
  return current.version >= next.version ? current : next
}

/** Adds still-open records after the authoritative window and drops every already-settled block. */
export function mergeThreadPreview(
  entries: readonly ThreadEntry[],
  preview: ThreadPreviewSnapshot,
): (ThreadEntry & { livePreview?: boolean })[] {
  if (!preview.available) return [...entries]
  const settled = new Set(
    entries
      .map((entry) => entry.historySequence)
      .filter((seq) => seq !== null && seq !== undefined),
  )
  const pending: (ThreadEntry & { livePreview?: boolean })[] = preview.blocks
    .filter((block) => !settled.has(block.historySequence))
    .toSorted((a, b) => a.historySequence - b.historySequence)
    .map((block) => ({
      seq: Number.MAX_SAFE_INTEGER - 4294967296 + block.historySequence,
      source: 'node',
      kind: typeof block.record['type'] === 'string' ? block.record['type'] : 'unknown',
      turnId: block.turnId || null,
      record: { ...block.record, seq: block.historySequence },
      createdAt: '',
      historySequence: block.historySequence,
      livePreview: true,
    }))
  return [...entries, ...pending]
}
