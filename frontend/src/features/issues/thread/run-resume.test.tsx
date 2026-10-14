import { render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import type { IssueRun } from '@/features/issues/types'
import { RunResume, resumeSummary } from './run-resume'

const base: IssueRun = {
  id: 'run-2',
  tenantId: 't1',
  issueId: 'i1',
  executorType: 'agent',
  executorId: 'agent-1',
  input: {},
  status: 'running',
  createdAt: '2026-10-10T00:00:00Z',
  updatedAt: '2026-10-10T00:00:00Z',
}

const earlier: IssueRun = {
  ...base,
  id: 'run-1',
  status: 'completed',
  revision: {
    id: 'rev-1',
    baseCommit: 'a'.repeat(40),
    finalCommit: '351e791b5ed9c31fde0b5953b6bf7254d0c12db4',
    changed: true,
    bundleSize: 549,
    historySize: 512,
    createdAt: '2026-10-10T00:00:00Z',
  },
}

describe('resumeSummary', () => {
  it('names the short final commit of the resumed Revision found among the runs', () => {
    expect(resumeSummary({ ...base, resumeRevisionId: 'rev-1' }, [earlier])).toBe(
      '续接自 Revision 351e791',
    )
  })

  it('still says the run resumed when the resumed run is not listed', () => {
    expect(resumeSummary({ ...base, resumeRevisionId: 'rev-9' }, [earlier])).toBe(
      '续接自上一次保存的 Revision',
    )
  })

  it('says nothing for a run that started fresh', () => {
    expect(resumeSummary(base, [earlier])).toBeNull()
    expect(resumeSummary({ ...base, resumeRevisionId: null }, [earlier])).toBeNull()
  })
})

describe('RunResume', () => {
  it('renders the summary as a labelled note and nothing for a fresh run', () => {
    const { rerender, container } = render(
      <RunResume run={{ ...base, resumeRevisionId: 'rev-1' }} runs={[earlier]} />,
    )
    expect(screen.getByRole('note', { name: '续接' })).toHaveTextContent('续接自 Revision 351e791')
    rerender(<RunResume run={base} runs={[earlier]} />)
    expect(container).toBeEmptyDOMElement()
  })
})
