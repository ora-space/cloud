import { render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import type { RunPreparation as Preparation } from '@/api/generated.schemas'
import { RunPreparation, SessionFailure } from './run-preparation'

const base: Preparation = {
  stage: 'clone',
  cloneAttempts: 0,
  maxCloneAttempts: 3,
  retryAt: null,
  errorCode: null,
  syncErrorCode: null,
}

describe('RunPreparation', () => {
  it('shows a missing snapshot as a wait for the first sync, not as a failure', () => {
    render(
      <RunPreparation
        progress={{ ...base, errorCode: 'snapshot_not_ready', retryAt: '2026-10-17T00:01:00Z' }}
      />,
    )
    expect(screen.getByRole('status')).toHaveTextContent(
      '正在等待项目首次同步仓库，同步完成后自动继续。',
    )
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
  })

  it('shows a failed repository sync as a failure naming the sync error code', () => {
    render(
      <RunPreparation
        progress={{
          ...base,
          stage: 'failed',
          errorCode: 'repository_sync_failed',
          syncErrorCode: 'remote_auth_failed',
        }}
      />,
    )
    expect(screen.getByRole('alert')).toHaveTextContent(
      '项目仓库同步失败，无法获取仓库（repository_sync_failed） 同步错误码：remote_auth_failed',
    )
    expect(screen.queryByRole('status')).not.toBeInTheDocument()
  })

  it('treats a failed repository sync as a failure even before the stage says failed', () => {
    render(<RunPreparation progress={{ ...base, errorCode: 'repository_sync_failed' }} />)
    expect(screen.getByRole('alert')).toHaveTextContent(
      '项目仓库同步失败，无法获取仓库（repository_sync_failed）',
    )
  })

  it('keeps showing clone attempts and the retry wait while cloning', () => {
    render(
      <RunPreparation
        progress={{ ...base, cloneAttempts: 2, errorCode: 'clone_failed', retryAt: 'x' }}
      />,
    )
    expect(screen.getByRole('status')).toHaveTextContent(
      '正在获取仓库… 尝试 2 / 3 等待重试，稍后自动继续。',
    )
  })

  it('waits for the session when there is no progress yet', () => {
    render(<RunPreparation progress={null} />)
    expect(screen.getByRole('status')).toHaveTextContent('等待 Agent 会话启动…')
  })
})

describe('SessionFailure', () => {
  it('drops a sync error code that is not a short snake_case classification', () => {
    render(<SessionFailure code="repository_sync_failed" syncErrorCode="fatal: <token> leaked" />)
    expect(screen.getByRole('alert')).toHaveTextContent(
      /^项目仓库同步失败，无法获取仓库（repository_sync_failed）$/,
    )
  })

  it('replaces an unknown failure code with the fixed agent failure', () => {
    render(<SessionFailure code="provider said something" />)
    expect(screen.getByRole('alert')).toHaveTextContent('Agent 会话失败（agent_failed）')
  })
})
