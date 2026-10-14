import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { render } from '@testing-library/react'
import type { ReactElement } from 'react'
import { createMemoryRouter, matchPath, MemoryRouter, RouterProvider } from 'react-router-dom'
import { SidebarProvider } from '@/components/ui/sidebar'
import { SessionProvider } from '@/features/auth/session'
import { CurrentSpaceProvider } from '@/features/spaces/current-space'
import { WORKSPACE_ROUTE_PATTERN } from '@/lib/paths'

/** Fresh QueryClient per render so cached data never leaks between tests. */
function newQueryClient() {
  return new QueryClient({ defaultOptions: { queries: { retry: false } } })
}

/**
 * Renders `ui` under the providers the app mounts: QueryClient, the session
 * probe, the sidebar context and the current-space resolver for `slug`.
 * Tests that need a signed-in member install the `/api/v1/me` handler first
 * (see `cloud-handlers`); the baseline server answers it with 401, so a page
 * renders as signed out unless a test says otherwise.
 */
export function renderWithProviders(
  ui: ReactElement,
  { route = '/', slug = '' }: { route?: string; slug?: string } = {},
) {
  return render(
    <QueryClientProvider client={newQueryClient()}>
      <SessionProvider>
        <MemoryRouter initialEntries={[route]}>
          <SidebarProvider>
            <CurrentSpaceProvider slug={slug}>{ui}</CurrentSpaceProvider>
          </SidebarProvider>
        </MemoryRouter>
      </SessionProvider>
    </QueryClientProvider>,
  )
}

/**
 * The workspace slug the app router would read from `path`: the segment after
 * the reserved `/w/` prefix, or `''` outside the workspace subtree.
 */
function slugOf(path: string): string {
  return matchPath({ path: WORKSPACE_ROUTE_PATTERN, end: false }, path)?.params.workspaceSlug ?? ''
}

/**
 * Mounts `element` behind a real route match (`path`), so hooks like
 * `useParams` resolve from `initialPath` the way they do in the app router.
 */
export function renderAtRoute(path: string, element: ReactElement, initialPath: string) {
  const router = createMemoryRouter([{ path, element }], { initialEntries: [initialPath] })
  return render(
    <QueryClientProvider client={newQueryClient()}>
      <SessionProvider>
        <SidebarProvider>
          <CurrentSpaceProvider slug={slugOf(initialPath)}>
            <RouterProvider router={router} />
          </CurrentSpaceProvider>
        </SidebarProvider>
      </SessionProvider>
    </QueryClientProvider>,
  )
}

/**
 * Mounts the given routes in a memory router at `initialPath` under the
 * QueryClient and session providers, for tests of route gates and layouts.
 * Returns the router alongside the render result, so a test can assert where
 * navigation ended up (`router.state.location.pathname`) — the memory router
 * keeps the URL out of `window.location`.
 */
export function renderRoutes(
  routes: Parameters<typeof createMemoryRouter>[0],
  initialPath: string,
) {
  const router = createMemoryRouter(routes, { initialEntries: [initialPath] })
  return {
    router,
    ...render(
      <QueryClientProvider client={newQueryClient()}>
        <SessionProvider>
          <SidebarProvider>
            <RouterProvider router={router} />
          </SidebarProvider>
        </SessionProvider>
      </QueryClientProvider>,
    ),
  }
}
