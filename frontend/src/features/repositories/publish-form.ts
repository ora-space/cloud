import { useState } from 'react'
import type {
  PostApiV1TenantsTidProjectsPidRepositoryPublishesBody,
  ProjectRepository,
  WorkspaceBranch,
} from '@/api/generated.schemas'

/** An external commit oid the backend reads as "this branch does not exist yet". */
const ZERO_OID = '0'.repeat(40)

/** What the publish dialog's fields hold while the user fills them in. */
export type PublishFormState = {
  workspaceId: string
  sourceRef: string
  revisionId: string
  credentialId: string
  targetBranch: string
  expectedOldOid: string
}

/** The form's state plus the two changes that carry a rule with them. */
export type PublishForm = {
  state: PublishFormState
  /** Choosing a Workspace clears the branch, whose list is scoped to one Workspace. */
  chooseWorkspace(workspaceId: string): void
  /** Choosing a target branch re-derives the commit the publish expects to find. */
  chooseTargetBranch(targetBranch: string): void
  setSourceRef(sourceRef: string): void
  setRevisionId(revisionId: string): void
  setCredentialId(credentialId: string): void
  setExpectedOldOid(expectedOldOid: string): void
}

/**
 * The external commit a publish expects to find on its target branch. The Binding's
 * snapshot only counts while the target is still the Binding's own branch: every
 * other branch is treated as one the publish creates, which the backend reads as the
 * all-zero oid. Re-deriving it on each branch change is what keeps a stale snapshot
 * from being sent as the expectation.
 */
export function expectedOldOidFor(targetBranch: string, source: ProjectRepository): string {
  if (targetBranch !== source.binding.branch) return ZERO_OID
  return source.snapshot?.commitOid ?? ZERO_OID
}

/**
 * The publish body for the chosen source. A revision id replaces the branch source,
 * because the backend takes exactly one of the two; the branch's commit is read at
 * submit time so the request names the commit the user chose.
 */
export function publishRequest(
  state: PublishFormState,
  sourceCommit: string | undefined,
): PostApiV1TenantsTidProjectsPidRepositoryPublishesBody {
  const body: PostApiV1TenantsTidProjectsPidRepositoryPublishesBody = {
    targetBranch: state.targetBranch,
    expectedOldOid: state.expectedOldOid,
    credentialRefId: state.credentialId,
  }
  if (state.revisionId) {
    body.revisionId = state.revisionId
  } else {
    body.sourceRef = state.sourceRef
    body.sourceCommit = sourceCommit ?? ''
  }
  return body
}

/**
 * Whether the form holds everything a publish request needs: a credential to push with,
 * and a source — a Revision on its own, or a branch the backend can resolve to a commit.
 */
export function publishReady(
  state: PublishFormState,
  branch: WorkspaceBranch | undefined,
): boolean {
  if (state.credentialId === '') return false
  return state.revisionId !== '' || branch !== undefined
}

/**
 * Form state for one explicit publish, seeded from the Binding so the common case —
 * publish the Binding's branch as it stands — needs no edits. The state lives here
 * rather than in the dialog because the two rules above couple fields the dialog
 * only renders.
 */
export function usePublishForm(source: ProjectRepository): PublishForm {
  const [state, setState] = useState<PublishFormState>(() => ({
    workspaceId: '',
    sourceRef: '',
    revisionId: '',
    credentialId: source.binding.credentialRefId ?? '',
    targetBranch: source.binding.branch,
    expectedOldOid: expectedOldOidFor(source.binding.branch, source),
  }))
  return {
    state,
    chooseWorkspace: (workspaceId) => setState((s) => ({ ...s, workspaceId, sourceRef: '' })),
    chooseTargetBranch: (targetBranch) =>
      setState((s) => ({
        ...s,
        targetBranch,
        expectedOldOid: expectedOldOidFor(targetBranch, source),
      })),
    setSourceRef: (sourceRef) => setState((s) => ({ ...s, sourceRef })),
    setRevisionId: (revisionId) => setState((s) => ({ ...s, revisionId })),
    setCredentialId: (credentialId) => setState((s) => ({ ...s, credentialId })),
    setExpectedOldOid: (expectedOldOid) => setState((s) => ({ ...s, expectedOldOid })),
  }
}
