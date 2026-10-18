import { fireEvent, render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import { adaptArchive } from '@/features/session-data'
import { SessionTimeline } from './index'

const epoch = Date.parse('2026-02-03T04:05:00Z')

function observations(rows: { at: number | null; name: string; duration?: number }[]) {
  return adaptArchive({
    runId: 'synthetic-chart-run',
    records: rows.map((row, seq) => ({
      seq,
      type: 'update',
      tool_timing: {
        startedAt: row.at === null ? undefined : new Date(epoch + row.at).toISOString(),
        durationMs: row.duration,
      },
      update: { sessionUpdate: 'tool_call', kind: 'read', title: row.name, status: 'completed' },
    })),
  })
}

function selectRange(start: number, end: number) {
  fireEvent.change(screen.getByRole('slider', { name: '框选起点' }), {
    target: { value: epoch + start },
  })
  fireEvent.change(screen.getByRole('slider', { name: '框选终点' }), {
    target: { value: epoch + end },
  })
}

describe('timeline layout and viewport', () => {
  it('defaults to compact chronological columns without changing source or list order', () => {
    const model = observations([
      { at: 1200000, name: 'Late file', duration: 1 },
      { at: 0, name: 'First file', duration: 0 },
      { at: 600000, name: 'Middle file', duration: 4000 },
    ])
    const original = structuredClone(model)
    render(<SessionTimeline model={model} />)
    expect(screen.getByRole('button', { name: '紧凑事件' })).toHaveAttribute('aria-pressed', 'true')
    const plot = within(screen.getByRole('group', { name: '紧凑事件时间轴' }))
    expect(plot.getAllByRole('img').map((mark) => mark.getAttribute('aria-label'))).toEqual([
      '读取 · First file · 0ms',
      '读取 · Middle file · 4.0s',
      '读取 · Late file · 1ms',
    ])
    expect(screen.getByText(/块宽不表示耗时/)).toBeInTheDocument()
    const list = within(screen.getByRole('table', { name: '时间轴事件' }))
    expect(list.getAllByRole('button').map((row) => row.textContent)).toEqual([
      '读取 · Late file',
      '读取 · First file',
      '读取 · Middle file',
    ])
    expect(model).toEqual(original)
  })

  it('keeps simultaneous observations distinct and unknown starts only in the list', () => {
    render(
      <SessionTimeline
        model={observations([
          { at: 0, name: 'Same time A', duration: 0 },
          { at: 0, name: 'Same time B' },
          { at: null, name: 'Untimed record' },
        ])}
      />,
    )
    const plot = within(screen.getByRole('group', { name: '紧凑事件时间轴' }))
    expect(plot.getAllByRole('img').map((mark) => mark.getAttribute('aria-label'))).toEqual([
      '读取 · Same time A · 0ms',
      '读取 · Same time B · 未知',
    ])
    expect(plot.getByText('#1')).toBeInTheDocument()
    expect(plot.getByText('#2')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '读取 · Untimed record' })).toBeInTheDocument()
  })

  it('zooms a selected window, clips crossing tools and never piles offscreen events at the edges', async () => {
    const user = userEvent.setup()
    render(
      <SessionTimeline
        model={observations([
          { at: 0, name: 'Before window', duration: 1 },
          { at: 4000, name: 'Crossing tool', duration: 8000 },
          { at: 20000, name: 'After window', duration: 1 },
        ])}
      />,
    )
    await user.click(screen.getByRole('button', { name: '真实时间' }))
    expect(screen.getByRole('button', { name: '放大选区' })).toBeDisabled()
    selectRange(8000, 10000)
    expect(screen.getByRole('button', { name: '读取 · Crossing tool' })).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '读取 · Before window' })).not.toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: '放大选区' }))
    const plot = within(screen.getByRole('group', { name: '时间轴框选区域' }))
    expect(plot.getAllByRole('img')).toHaveLength(1)
    expect(plot.getByRole('img', { name: '读取 · Crossing tool · 8.0s' })).toHaveStyle({
      width: '100%',
    })
    expect(screen.getByRole('slider', { name: '框选起点' })).toHaveAttribute(
      'min',
      String(epoch + 8000),
    )
    expect(screen.getByRole('slider', { name: '框选终点' })).toHaveAttribute(
      'max',
      String(epoch + 10000),
    )
    await user.click(screen.getByRole('button', { name: '显示全程' }))
    expect(plot.getAllByRole('img')).toHaveLength(3)
    expect(screen.getByRole('button', { name: '读取 · Before window' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '放大选区' })).toBeDisabled()
  })

  it('uses millisecond ruler ticks for a subsecond viewport and restores compact mode', async () => {
    const user = userEvent.setup()
    render(
      <SessionTimeline
        model={observations([
          { at: 0, name: 'Anchor', duration: 0 },
          { at: 4000, name: 'Short tool', duration: 100 },
          { at: 10000, name: 'Last anchor', duration: 0 },
        ])}
      />,
    )
    await user.click(screen.getByRole('button', { name: '真实时间' }))
    selectRange(4000, 4100)
    await user.click(screen.getByRole('button', { name: '放大选区' }))
    const strip = within(screen.getByRole('region', { name: '三泳道时间轴' }))
    const ticks = strip.getAllByText(/^\d{2}:\d{2}:\d{2}\.\d{3}$/)
    expect(new Set(ticks.map((tick) => tick.textContent)).size).toBe(5)
    expect(strip.getByRole('img', { name: '读取 · Short tool · 100ms' })).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: '紧凑事件' }))
    expect(screen.getByRole('group', { name: '紧凑事件时间轴' })).toBeInTheDocument()
    expect(screen.queryByRole('slider')).not.toBeInTheDocument()
  })

  it('keeps a single point usable without an artificial duration or positive zoom range', async () => {
    const user = userEvent.setup()
    render(<SessionTimeline model={observations([{ at: 0, name: 'Single point' }])} />)
    expect(screen.getByRole('img', { name: '读取 · Single point · 未知' })).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: '真实时间' }))
    expect(screen.getByRole('button', { name: '放大选区' })).toBeDisabled()
    expect(screen.getByRole('img', { name: '读取 · Single point · 未知' })).toHaveStyle({
      width: '0%',
    })
  })
})
