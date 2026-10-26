import { create } from 'axios'
import { db } from '@/mocks/data/store'

/**
 * Client for the MSW-mocked domain (`/mock-api/*`), kept separate from the
 * orval-generated client in `src/api` which talks to the real Go backend.
 *
 * Pages without a backend counterpart yet (issues, agents, chat, ...) run on
 * this client. Their routes carry the real space slug, which the demo store
 * does not know, so the slug is rewritten to the seeded workspace before the
 * request reaches MSW: every real space shows the same demo data until the
 * page gets a real API. The mock domain has no authentication; the real
 * session lives in the gateway cookie and never reaches these handlers.
 */
export const mockApi = create({ baseURL: '/mock-api' })

// A missing/restarting mock worker lets the SPA fallback answer with HTML and
// HTTP 200. Reject it before query caching; consumers expect domain JSON. An empty
// body (204) also arrives as a string and stays valid.
mockApi.interceptors.response.use((response) => {
  if (typeof response.data === 'string' && response.data !== '') {
    throw new Error('Mock API returned a non-JSON response')
  }
  return response
})

mockApi.interceptors.request.use((config) => {
  if (config.url?.startsWith('/workspaces/')) {
    config.url = config.url.replace(/^\/workspaces\/[^/]+/, `/workspaces/${db.workspace.slug}`)
  }
  return config
})
