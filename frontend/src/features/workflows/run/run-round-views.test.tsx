import { fireEvent, screen, within } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { RunContainerRounds } from '@/features/workflows/run/run-round-views'
import type { RunContainer } from '@/features/workflows/run/run-rounds'
import { renderWithProviders } from '@/test/render'

/** Two settled Iteration items: the first collected a value, the second failed under continue. */
const ITERATION: RunContainer = {
  id: 'iter-1',
  kind: 'iteration',
  status: 'succeeded',
  memberIds: ['pick'],
  limit: 2,
  settled: 2,
  failed: 1,
  rounds: [
    {
      index: 0,
      source: 'archived',
      outcome: 'succeeded',
      states: { pick: { status: 'succeeded', output: { output: '甲' } } },
      item: '甲',
      entry: { status: 'succeeded', output: '收集值-甲', error: null },
    },
    {
      index: 1,
      source: 'archived',
      outcome: 'absorbed',
      states: { pick: { status: 'failed', error: 'no match' } },
      item: '乙',
      entry: { status: 'failed', output: null, error: 'aggregator_no_match' },
    },
  ],
}

/** Renders the rounds section with a spy for the round switch. */
function renderRounds(selection: Record<string, number>) {
  const onSelectRound = vi.fn<(containerId: string, index: number) => void>()
  renderWithProviders(
    <RunContainerRounds
      container={ITERATION}
      selection={selection}
      memberTitles={new Map([['pick', '透传']])}
      onSelectRound={onSelectRound}
    />,
  )
  return { onSelectRound, rounds: screen.getByRole('region', { name: '轮次' }) }
}

describe('RunContainerRounds for an Iteration', () => {
  it('shows the item, the value it collected and its members, numbering items from 1', () => {
    const { rounds } = renderRounds({ 'iter-1': 0 })

    expect(within(rounds).getByRole('button', { name: /第 1 项/ })).toHaveAttribute(
      'aria-pressed',
      'true',
    )
    expect(within(rounds).getByText('本项收集到')).toBeInTheDocument()
    expect(within(rounds).getByText('收集值-甲')).toBeInTheDocument()
    expect(within(rounds).getByText('透传')).toBeInTheDocument()
  })

  it('shows the ledger error, not a collected value, for an absorbed failure', () => {
    const { rounds, onSelectRound } = renderRounds({ 'iter-1': 1 })

    expect(within(rounds).queryByText('本项收集到')).not.toBeInTheDocument()
    expect(within(rounds).getByText('aggregator_no_match')).toBeInTheDocument()

    fireEvent.click(within(rounds).getByRole('button', { name: /第 1 项/ }))
    expect(onSelectRound).toHaveBeenCalledWith('iter-1', 0)
  })
})
