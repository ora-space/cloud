import { Navigate, Outlet, type RouteObject } from 'react-router'
import { features } from './features'
import { Layout } from './layout'
import { SessionGate } from './auth/session'

export const routes: RouteObject[] = [
  // An invitation needs a signed-in account but no workspace yet, so it sits outside the shell.
  {
    path: '/invite/:token',
    element: <SessionGate><Outlet /></SessionGate>,
    hydrateFallbackElement: <p className="p-4 text-sm text-muted-foreground">加载中…</p>,
    children: [{ index: true, lazy: async () => ({ Component: (await import('@/features/invite/page')).default }) }],
  },
  // Ora desktop's sign-in lands here (A6 D6): it needs the browser's session, not a workspace.
  {
    path: '/desktop/authorize',
    element: <SessionGate><Outlet /></SessionGate>,
    hydrateFallbackElement: <p className="p-4 text-sm text-muted-foreground">加载中…</p>,
    children: [{ index: true, lazy: async () => ({ Component: (await import('@/features/desktop-authorize/page')).default }) }],
  },
  {
    path: '/',
    element: <Layout />,
    // Shown while the first lazily loaded page arrives.
    hydrateFallbackElement: (
      <p className="p-4 text-sm text-muted-foreground">加载中…</p>
    ),
    children: [
      {
        index: true,
        element: <Navigate to={`/${features[0].path}`} replace />,
      },
      ...features.map((f) => ({
        path: f.path,
        children: f.children,
        lazy: async () => ({ Component: (await f.page()).default }),
      })),
      { path: '*', element: <Navigate to="/" replace /> },
    ],
  },
]
