import { render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import {
  provisioningFailureMessage,
  type ProvisioningFailure,
} from '@/features/projects/provisioning'
import { ProvisioningFailureNotice } from './provisioning-failure-notice'

/** The failure the hook hands over: Cloud's code together with its sentence. */
function failure(code: string): ProvisioningFailure {
  return { code, message: provisioningFailureMessage(code) }
}

describe('ProvisioningFailureNotice', () => {
  it('shows the sentence a member acts on next to the code support asks for', () => {
    render(<ProvisioningFailureNotice failure={failure('repository_credential_revoked')} />)

    const banner = screen.getByRole('alert')
    expect(banner).toHaveTextContent('项目创建失败')
    expect(banner).toHaveTextContent('Token 已销毁')
    expect(banner).toHaveTextContent('新建项目并绑定替代凭据')
    expect(banner).toHaveTextContent('repository_credential_revoked')
  })

  it('shows an unknown reason as its bare code rather than as a sentence nobody wrote', () => {
    // The banner is the only place a reason reaches the member. A reassuring
    // sentence invented here would hide a reason Cloud did state.
    render(<ProvisioningFailureNotice failure={failure('some_future_code')} />)

    expect(screen.getByRole('alert')).toHaveTextContent('项目创建失败：some_future_code')
  })

  it('offers no action, because the retry route refuses a failed Operation', () => {
    render(<ProvisioningFailureNotice failure={failure('mirror_preparation_failed')} />)

    expect(screen.queryAllByRole('button')).toEqual([])
    expect(screen.queryAllByRole('link')).toEqual([])
  })
})
