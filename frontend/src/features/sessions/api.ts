import { useInfiniteQuery, useQuery } from '@tanstack/react-query'
import { useMemo } from 'react'
import { getApiV1TenantsTidProjects } from '@/api/projects/projects'
import type {
  GetApiV1TenantsTidProjects200,
  GetApiV1TenantsTidAgentSessionsParams,
} from '@/api/generated.schemas'
import {
  getApiV1TenantsTidAgentSessions,
  getApiV1TenantsTidIssuesIidRunsRid,
  getApiV1TenantsTidIssuesIidRunsRidHistory,
  getApiV1TenantsTidIssuesIidRunsRidThread,
} from '@/api/tenants/tenants'
import { adaptArchive, adaptThread } from '@/features/session-data'
import { faultCode } from '@/lib/api-client'

/** Scope required for every detail read; workspace slugs are never tenant credentials. */
export interface SessionRef {
  tid: string
  issueId: string
  runId: string
}

/** Newest-first summaries; each filter combination owns its pagination cache. */
export function useSessionList(tid: string, filters: GetApiV1TenantsTidAgentSessionsParams) {
  return useInfiniteQuery({
    queryKey: ['session-list', tid, filters],
    initialPageParam: '',
    queryFn: ({ pageParam, signal }) =>
      getApiV1TenantsTidAgentSessions(
        tid,
        { ...filters, limit: 50, ...(pageParam ? { after: pageParam } : {}) },
        undefined,
        signal,
      ),
    getNextPageParam: (page) => page.nextCursor || undefined,
    retry: false,
  })
}

async function threadPage(ref: SessionRef, before: number | undefined, signal: AbortSignal) {
  try {
    return await getApiV1TenantsTidIssuesIidRunsRidThread(
      ref.tid,
      ref.issueId,
      ref.runId,
      { limit: 200, ...(before === undefined ? {} : { before }) },
      undefined,
      signal,
    )
  } catch (error) {
    if (faultCode(error) === 'not_found') return null
    throw error
  }
}

/** Manual snapshots with bounded older-page reads, separate from the conversation composer cache. */
export function useSessionContent(ref: SessionRef) {
  const run = useQuery({
    queryKey: ['session-run', ref],
    queryFn: ({ signal }) =>
      getApiV1TenantsTidIssuesIidRunsRid(ref.tid, ref.issueId, ref.runId, undefined, signal),
    retry: false,
  })
  const thread = useInfiniteQuery({
    queryKey: ['session-thread', ref],
    enabled: run.isSuccess && run.data.executorType === 'agent',
    initialPageParam: undefined as number | undefined,
    queryFn: ({ pageParam, signal }) => threadPage(ref, pageParam, signal),
    getNextPageParam: (page) => {
      const first = page?.items[0]?.seq
      return first !== undefined && first > 1 ? first : undefined
    },
    retry: false,
  })
  const archive = useQuery({
    queryKey: ['session-history', ref, run.data?.revision?.id],
    enabled: Boolean(run.data?.revision),
    queryFn: ({ signal }) =>
      getApiV1TenantsTidIssuesIidRunsRidHistory(ref.tid, ref.issueId, ref.runId, undefined, signal),
    retry: false,
  })
  const model = useMemo(() => {
    const frozenModel = { id: thread.data?.pages[0]?.model?.modelId }
    if (run.data?.revision) {
      return archive.data
        ? adaptArchive({ runId: ref.runId, records: archive.data.records, frozenModel })
        : null
    }
    return thread.data
      ? adaptThread({
          runId: ref.runId,
          entries: thread.data.pages.flatMap((page) => page?.items ?? []),
          frozenModel,
        })
      : null
  }, [archive.data, thread.data, ref.runId, run.data?.revision])
  const refresh = async () => {
    const current = await run.refetch()
    if (current.isError) return false
    await thread.refetch()
    if (run.data?.revision) await archive.refetch()
    return true
  }
  return { run, thread, archive, model, refresh }
}

async function readProjects(
  tid: string,
  signal: AbortSignal,
  after = '',
): Promise<GetApiV1TenantsTidProjects200['items']> {
  const page = await getApiV1TenantsTidProjects(
    tid,
    { limit: 100, ...(after ? { after } : {}) },
    undefined,
    signal,
  )
  if (!page.nextCursor) return page.items
  return [...page.items, ...(await readProjects(tid, signal, page.nextCursor))]
}

/** Project names are loaded across every page so filters never silently hide later projects. */
export function useSessionProjects(tid: string) {
  return useQuery({
    queryKey: ['session-projects', tid],
    queryFn: ({ signal }) => readProjects(tid, signal),
    retry: false,
  })
}
