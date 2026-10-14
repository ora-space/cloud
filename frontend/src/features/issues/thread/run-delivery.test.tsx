import { render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import type { IssueRun } from '@/features/issues/types'
import { RunDelivery, deliverySummary } from './run-delivery'

const base: IssueRun = {
  id: 'run-1',
  tenantId: 't1',
  issueId: 'i1',
  executorType: 'agent',
  executorId: 'agent-1',
  input: {},
  status: 'completed',
  createdAt: '2026-10-09T00:00:00Z',
  updatedAt: '2026-10-09T00:00:00Z',
}

const revision = {
  id: 'rev-1',
  baseCommit: 'a'.repeat(40),
  finalCommit: '1234567890abcdef1234567890abcdef12345678',
  bundleSize: 2048,
  historySize: 512,
  createdAt: '2026-10-09T00:00:00Z',
}

describe('deliverySummary', () => {
  it('names the short final commit and the bundle size of a changed Revision', () => {
    expect(deliverySummary({ ...base, revision: { ...revision, changed: true } }, true)).toBe(
      '已保存 Revision 1234567（含改动，bundle 2.0 KB）',
    )
  })

  it('says an unchanged Revision saved no file changes', () => {
    expect(
      deliverySummary(
        { ...base, revision: { ...revision, changed: false, bundleSize: null } },
        true,
      ),
    ).toBe('已保存 Revision 1234567（无文件改动）')
  })

  it('says a resumed run without new commits reused the resumed Revision', () => {
    expect(
      deliverySummary(
        {
          ...base,
          revision: { ...revision, changed: false, bundleSize: null, priorRevisionId: 'rev-0' },
        },
        true,
      ),
    ).toBe('已保存 Revision 1234567（无新改动，沿用续接的成果）')
  })

  it('explains a skipped or failed delivery from the run result', () => {
    expect(
      deliverySummary({ ...base, result: { deliveryState: 'skipped', revisionId: null } }, true),
    ).toBe('未保存 Revision（未配置对象存储）')
    expect(
      deliverySummary({ ...base, result: { deliveryState: 'failed', revisionId: null } }, true),
    ).toBe('Revision 保存失败')
  })

  it('shows saving only for a running run whose Thread already ended', () => {
    expect(deliverySummary({ ...base, status: 'running' }, true)).toBe('正在保存 Revision…')
    expect(deliverySummary({ ...base, status: 'running' }, false)).toBeNull()
    expect(deliverySummary(base, true)).toBeNull()
  })
})

describe('RunDelivery', () => {
  it('renders the summary as a labelled status and nothing without one', () => {
    const { rerender, container } = render(
      <RunDelivery run={{ ...base, revision: { ...revision, changed: true } }} threadEnded />,
    )
    expect(screen.getByRole('status', { name: 'Revision 交付' })).toHaveTextContent(
      '已保存 Revision 1234567',
    )
    rerender(<RunDelivery run={base} threadEnded={false} />)
    expect(container).toBeEmptyDOMElement()
  })
})
