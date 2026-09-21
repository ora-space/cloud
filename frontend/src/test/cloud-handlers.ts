import { http, HttpResponse } from 'msw'
import { server } from '@/test/msw-server'

/**
 * Shared fixtures for tests that exercise the cloud-backed space flow: the
 * tenant list and one space whose slug is `cloud-dev`. Browser authentication
 * is represented by the Gateway's HttpOnly cookie and therefore needs no
 * JavaScript credential fixture.
 */

export const TEST_TENANT_ID = '11111111-1111-1111-1111-111111111111'
export const TEST_SPACE_ID = '22222222-2222-2222-2222-222222222222'

/**
 * Installs MSW handlers for the shared cloud fixtures: one active tenant and
 * one `cloud-dev` space where the signed-in member holds the given role.
 */
export function installCloudSpaceHandlers(role: string): void {
  server.use(
    http.get('/api/v1/me/tenants', () =>
      HttpResponse.json({
        items: [{ id: TEST_TENANT_ID, name: '研发组织', status: 'active', role: 'admin' }],
        nextCursor: '',
      }),
    ),
    http.get(`/api/v1/tenants/${TEST_TENANT_ID}/spaces`, () =>
      HttpResponse.json({
        items: [
          {
            id: TEST_SPACE_ID,
            tenantId: TEST_TENANT_ID,
            name: 'Cloud Dev',
            slug: 'cloud-dev',
            description: '',
            createdBy: TEST_TENANT_ID,
            version: 1,
            createdAt: '2026-09-20T10:00:00+08:00',
            updatedAt: '2026-09-20T10:00:00+08:00',
            archivedAt: null,
            role,
          },
        ],
        nextCursor: '',
      }),
    ),
  )
}
