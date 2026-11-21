import { useState } from 'react'
import { useMutation } from '@tanstack/react-query'
import { useSearchParams } from 'react-router'
import { OraMark } from '@/components/ora-mark'
import { Button } from '@/components/ui/button'
import { useUser } from '@/app/auth/user'
import { SignOutButton } from '@/app/auth/sign-out-button'
import { approveDesktopLogin, type DesktopApproval } from '@/lib/auth-api'
import { codeOf } from '@/lib/api-client'
import { navigation } from './navigation'

/** Reads the parameters Ora desktop opened this page with; null when any is missing. */
function approvalFrom(params: URLSearchParams): DesktopApproval | null {
  const redirectUri = params.get('redirect_uri')
  const state = params.get('state')
  const codeChallenge = params.get('code_challenge')
  const codeChallengeMethod = params.get('code_challenge_method')
  if (!redirectUri || !state || !codeChallenge || !codeChallengeMethod) return null
  return { redirectUri, state, codeChallenge, codeChallengeMethod }
}

/**
 * `/desktop/authorize`: where Ora desktop sends the system browser to sign in (A6 D6). The session
 * gate around it has already signed the visitor in. Nothing is issued until the person clicks
 * 「允许」, so a session left open in the browser never hands out a desktop token silently; the
 * server checks the parameters again and decides whether the loopback address is acceptable.
 */
export default function DesktopAuthorizePage() {
  const [params] = useSearchParams()
  const user = useUser()
  const approval = approvalFrom(params)
  const [declined, setDeclined] = useState(false)
  const approve = useMutation({
    mutationFn: () => approveDesktopLogin(approval!),
    onSuccess: (redirectUrl) => navigation.assign(redirectUrl),
  })

  let body
  if (!approval) {
    body = <p role="alert" className="text-sm text-destructive">登录链接不完整，请回到 Ora desktop 重新点「登录」。</p>
  } else if (declined) {
    body = <p>已取消。可以关闭这个页面，回到 Ora desktop。</p>
  } else if (approve.isSuccess) {
    body = <p>正在回到 Ora desktop…</p>
  } else {
    body = <>
      <p>允许 Ora desktop 以「{user.displayName}」的身份登录？</p>
      <p className="text-muted-foreground">desktop 将以你的身份访问这里的团队和数据，直到你在 desktop 上退出登录。</p>
      <div className="flex gap-2">
        <Button disabled={approve.isPending} onClick={() => approve.mutate()}>允许</Button>
        <Button variant="outline" disabled={approve.isPending} onClick={() => setDeclined(true)}>取消</Button>
      </div>
      {approve.isError && <p role="alert" className="text-sm text-destructive">{failure(codeOf(approve.error))}</p>}
    </>
  }

  return <div className="flex h-svh items-center justify-center bg-sidebar px-4">
    <div className="flex w-full max-w-sm flex-col gap-4 rounded-2xl border bg-card p-8 text-sm">
      <OraMark className="size-10" />
      <h1 className="text-xl font-semibold tracking-tight">登录 Ora desktop</h1>
      {body}
      <SignOutButton />
    </div>
  </div>
}

function failure(code: string | undefined): string {
  switch (code) {
    case 'invalid_redirect_uri':
    case 'invalid_code_challenge':
    case 'invalid_state':
      return '这个登录链接不是 Ora desktop 发起的，已拒绝。请回到 Ora desktop 重新点「登录」。'
    default:
      return '没有完成授权，请重试。'
  }
}
