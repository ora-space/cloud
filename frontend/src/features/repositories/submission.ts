import type { GitCredentialSubmission } from '@/api/generated.schemas'

/** Sends a token directly to the Store, with no gateway cookies, redirects or retries. */
export async function submitGitToken(
  submission: GitCredentialSubmission,
  token: string,
): Promise<void> {
  const url = new URL(submission.ingressUrl)
  if (url.protocol !== 'https:' || url.username || url.password) {
    throw new Error('凭据入口必须使用 HTTPS')
  }
  const response = await fetch(url, {
    method: 'PUT',
    credentials: 'omit',
    redirect: 'error',
    referrerPolicy: 'no-referrer',
    headers: {
      'Content-Type': 'application/json',
      Authorization: `Ora-Submission ${submission.grant}`,
    },
    body: JSON.stringify({ token }),
  })
  if (!response.ok) throw new Error('凭据提交失败，请重新申请提交授权')
}
