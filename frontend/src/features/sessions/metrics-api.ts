import { useQuery } from '@tanstack/react-query'
import { useState } from 'react'
import {
  getApiV1TenantsTidIssuesIidRunsRidStatistics,
  getApiV1TenantsTidIssuesIidRunsRidModelCalls,
} from '@/api/tenants/tenants'
import { isForbiddenError, isUnauthorizedError } from '@/lib/api-client'
import type { SessionRef } from './api'

const pageSize = 25

/** Reads all-recorded-attempt aggregates and one cursor page without depending on archive access. */
export function useSessionMetrics(ref: SessionRef, enabled: boolean) {
  const [cursors, setCursors] = useState([''])
  const after = cursors.at(-1) ?? ''
  const statistics = useQuery({
    queryKey: ['session-model-statistics', ref],
    enabled,
    queryFn: ({ signal }) =>
      getApiV1TenantsTidIssuesIidRunsRidStatistics(
        ref.tid,
        ref.issueId,
        ref.runId,
        undefined,
        signal,
      ),
    retry: false,
  })
  const calls = useQuery({
    queryKey: ['session-model-calls', ref, after],
    enabled,
    queryFn: ({ signal }) =>
      getApiV1TenantsTidIssuesIidRunsRidModelCalls(
        ref.tid,
        ref.issueId,
        ref.runId,
        { limit: pageSize, ...(after ? { after } : {}) },
        undefined,
        signal,
      ),
    retry: false,
  })
  const accessError = [statistics.error, calls.error].find(
    (error) => isForbiddenError(error) || isUnauthorizedError(error),
  )
  const nextPage = () => {
    const cursor = calls.data?.nextCursor
    if (cursor && !calls.isFetching && !cursors.includes(cursor)) {
      setCursors((previous) => [...previous, cursor])
    }
  }
  const previousPage = () =>
    setCursors((previous) => previous.slice(0, Math.max(1, previous.length - 1)))
  const refresh = async () => {
    await Promise.all([statistics.refetch(), calls.refetch()])
  }
  return {
    statistics,
    calls,
    accessError,
    pageIndex: cursors.length - 1,
    pageSize,
    nextPage,
    previousPage,
    refresh,
  }
}
