import type { GitCredential } from '@/api/generated.schemas'
import { useSession } from '@/features/auth/session'
import { useCurrentSpace } from '@/features/spaces/current-space'

/**
 * The acting user as the credential rules see them. `userId` is empty while nobody
 * is signed in, so an anonymous viewer matches no owner and only a tenant
 * administrator can still act.
 */
export type CredentialViewer = {
  userId: string
  isTenantAdmin: boolean
}

/** Reads the acting user's identity from the session and the current space. */
export function useCredentialViewer(): CredentialViewer {
  const { session } = useSession()
  const { space } = useCurrentSpace()
  return {
    userId: session.status === 'signed-in' ? session.user.id : '',
    isTenantAdmin: space?.role === 'admin',
  }
}

/**
 * Mirrors the backend's rule for who may publish with, rotate or verify a
 * credential: a personal credential belongs to its owner alone — an administrator
 * cannot act for them — and a team credential to a tenant administrator. An unknown
 * credential, and one whose attribution Cloud could not determine, grant nothing.
 *
 * Revocation is deliberately outside this rule: it only tightens access, so the
 * backend also lets a tenant administrator revoke another member's personal
 * credential, and callers that offer a revoke control add that case themselves.
 *
 * This decides only which controls the UI offers. The backend decides what is
 * actually allowed, so a wrong answer here can hide a button but never grant one.
 */
export function mayManageCredential(
  credential: Pick<GitCredential, 'attribution' | 'ownerUserId'> | undefined,
  viewer: CredentialViewer,
): boolean {
  if (!credential) return false
  if (credential.attribution === 'personal') {
    return viewer.userId !== '' && credential.ownerUserId === viewer.userId
  }
  return credential.attribution === 'team' && viewer.isTenantAdmin
}
