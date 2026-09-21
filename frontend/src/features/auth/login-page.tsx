import { useCallback, useEffect, useRef } from 'react'
import { Navigate, useSearchParams } from 'react-router-dom'
import { Button } from '@/components/ui/button'
import { classifyAuthenticationFailure, useCurrentUser, useStartLogin } from '@/features/auth/api'
import { safeReturnTo } from '@/features/auth/return-to'

function replaceBrowserLocation(target: string) {
  window.location.replace(target)
}

/** Dependencies for the automatic external navigation owned by the login screen. */
export interface LoginPageProps {
  replaceLocation?: (target: string) => void
}

/** Automatically starts corporate login for an unauthenticated browser and stops on failures. */
export function LoginPage({ replaceLocation = replaceBrowserLocation }: LoginPageProps) {
  const [search] = useSearchParams()
  const returnTo = safeReturnTo(search.get('returnTo'))
  const currentUser = useCurrentUser()
  const login = useStartLogin()
  const attempted = useRef(false)
  const failure = currentUser.isError ? classifyAuthenticationFailure(currentUser.error) : undefined

  const beginLogin = useCallback(() => {
    attempted.current = true
    login.mutate(returnTo, {
      onSuccess: ({ authorizationUrl }) => replaceLocation(authorizationUrl),
    })
  }, [login, replaceLocation, returnTo])

  useEffect(() => {
    if (failure === 'unauthenticated' && !attempted.current) beginLogin()
  }, [beginLogin, failure])

  if (currentUser.isSuccess) return <Navigate to={returnTo} replace />

  let title = '正在验证登录状态…'
  let description = '请稍候。'
  let action: React.ReactNode

  if (failure === 'forbidden') {
    title = '账号已被停用'
    description = 'Cloud 已拒绝当前账号，请联系管理员恢复访问。'
  } else if (failure === 'unavailable') {
    title = '暂时无法验证登录状态'
    description = 'Cloud 或认证网关暂时不可用。'
    action = <Button onClick={() => void currentUser.refetch()}>重新检查</Button>
  } else if (login.isError) {
    title = '无法连接华为统一登录'
    description = '登录尚未开始，可以安全重试。'
    action = (
      <Button
        onClick={() => {
          login.reset()
          beginLogin()
        }}
      >
        重新登录
      </Button>
    )
  } else if (failure === 'unauthenticated') {
    title = '正在跳转华为统一登录…'
    description = '登录成功后将自动返回 Cloud。'
  }

  return (
    <div className="flex min-h-svh items-center justify-center bg-muted/30 px-4">
      <div className="w-full max-w-sm space-y-6 text-center">
        <div className="mx-auto flex size-10 items-center justify-center rounded-lg bg-primary text-primary-foreground font-semibold">
          O
        </div>
        <div className="space-y-1">
          <h1 className="text-lg font-semibold">{title}</h1>
          <p className="text-sm text-muted-foreground">{description}</p>
        </div>
        {action}
      </div>
    </div>
  )
}
