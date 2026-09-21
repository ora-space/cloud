import { create } from 'axios'
import { db } from '@/mocks/data/store'

/**
 * Client for the MSW-mocked domain (`/mock-api/*`), kept separate from the
 * orval-generated client in `src/api` which talks to the real Go backend.
 * Mock pages keep using the seeded workspace even when the authenticated URL
 * carries a real Cloud space slug.
 */
export const mockApi = create({ baseURL: '/mock-api' })

mockApi.interceptors.request.use((config) => {
  if (config.url?.startsWith('/workspaces/')) {
    config.url = config.url.replace(/^\/workspaces\/[^/]+/, `/workspaces/${db.workspace.slug}`)
  }
  return config
})
