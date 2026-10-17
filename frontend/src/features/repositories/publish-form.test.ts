import { act, renderHook } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import {
  branchFixture,
  repositoryFixture,
  TEST_BRANCH_OID,
  TEST_CREDENTIAL_ID,
  TEST_SNAPSHOT_OID,
} from '@/test/cloud-handlers'
import { expectedOldOidFor, publishReady, publishRequest, usePublishForm } from './publish-form'
import type { PublishFormState } from './publish-form'

const ZERO_OID = '0'.repeat(40)

function stateWith(overrides: Partial<PublishFormState> = {}): PublishFormState {
  return {
    workspaceId: '',
    sourceRef: '',
    revisionId: '',
    credentialId: TEST_CREDENTIAL_ID,
    targetBranch: 'main',
    expectedOldOid: TEST_SNAPSHOT_OID,
    ...overrides,
  }
}

describe('expectedOldOidFor', () => {
  it('expects the recorded snapshot while the target is the Binding branch', () => {
    expect(expectedOldOidFor('main', repositoryFixture())).toBe(TEST_SNAPSHOT_OID)
  })

  it('expects an absent branch for any other target, which the publish creates', () => {
    expect(expectedOldOidFor('release', repositoryFixture())).toBe(ZERO_OID)
  })

  it('expects an absent branch when the Binding has no snapshot yet', () => {
    expect(expectedOldOidFor('main', repositoryFixture({ snapshot: null }))).toBe(ZERO_OID)
  })
})

describe('publishRequest', () => {
  it('names the chosen Revision alone, because the backend takes one source', () => {
    const body = publishRequest(
      stateWith({ revisionId: 'rev-1', sourceRef: 'refs/heads/topic' }),
      TEST_BRANCH_OID,
    )
    expect(body).toEqual({
      targetBranch: 'main',
      expectedOldOid: TEST_SNAPSHOT_OID,
      credentialRefId: TEST_CREDENTIAL_ID,
      revisionId: 'rev-1',
    })
  })

  it('names the branch and the commit read at submit time', () => {
    const body = publishRequest(stateWith({ sourceRef: 'refs/heads/topic' }), TEST_BRANCH_OID)
    expect(body).toEqual({
      targetBranch: 'main',
      expectedOldOid: TEST_SNAPSHOT_OID,
      credentialRefId: TEST_CREDENTIAL_ID,
      sourceRef: 'refs/heads/topic',
      sourceCommit: TEST_BRANCH_OID,
    })
  })

  it('sends an empty commit when the chosen branch did not resolve to one', () => {
    const body = publishRequest(stateWith({ sourceRef: 'refs/heads/gone' }), undefined)
    expect(body.sourceCommit).toBe('')
  })
})

describe('publishReady', () => {
  const topic = branchFixture()

  it('refuses a request without a credential, whatever the source is', () => {
    expect(publishReady(stateWith({ credentialId: '', revisionId: 'rev-1' }), topic)).toBe(false)
  })

  it('accepts a Revision as a source on its own', () => {
    expect(publishReady(stateWith({ revisionId: 'rev-1' }), undefined)).toBe(true)
  })

  it('accepts a branch that resolved to a commit', () => {
    expect(publishReady(stateWith({ sourceRef: 'refs/heads/topic' }), topic)).toBe(true)
  })

  it('refuses a branch the Workspace never resolved', () => {
    expect(publishReady(stateWith({ sourceRef: 'refs/heads/topic' }), undefined)).toBe(false)
  })

  it('refuses a credential with neither a Revision nor a branch', () => {
    expect(publishReady(stateWith(), undefined)).toBe(false)
  })
})

describe('usePublishForm', () => {
  it('seeds every field from the Binding so the common publish needs no edits', () => {
    const { result } = renderHook(() => usePublishForm(repositoryFixture()))
    expect(result.current.state).toEqual({
      workspaceId: '',
      sourceRef: '',
      revisionId: '',
      credentialId: TEST_CREDENTIAL_ID,
      targetBranch: 'main',
      expectedOldOid: TEST_SNAPSHOT_OID,
    })
  })

  it('re-derives the expected commit when the target branch changes, both ways', () => {
    const { result } = renderHook(() => usePublishForm(repositoryFixture()))
    act(() => result.current.chooseTargetBranch('release'))
    expect(result.current.state.expectedOldOid).toBe(ZERO_OID)
    act(() => result.current.chooseTargetBranch('main'))
    expect(result.current.state.expectedOldOid).toBe(TEST_SNAPSHOT_OID)
  })

  it('clears the branch when the Workspace changes, whose branch list is scoped to it', () => {
    const { result } = renderHook(() => usePublishForm(repositoryFixture()))
    act(() => result.current.setSourceRef('refs/heads/topic'))
    act(() => result.current.chooseWorkspace('w-2'))
    expect(result.current.state).toMatchObject({ workspaceId: 'w-2', sourceRef: '' })
  })

  it('keeps every field the caller set explicitly', () => {
    const { result } = renderHook(() => usePublishForm(repositoryFixture()))
    act(() => result.current.setRevisionId('rev-9'))
    act(() => result.current.setCredentialId('cred-9'))
    act(() => result.current.setExpectedOldOid(TEST_BRANCH_OID))
    expect(result.current.state).toMatchObject({
      revisionId: 'rev-9',
      credentialId: 'cred-9',
      expectedOldOid: TEST_BRANCH_OID,
    })
  })
})
