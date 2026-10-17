import { http, HttpResponse } from 'msw'
import type { Workflow } from '@/api/generated.schemas'
import { server } from '@/test/msw-server'
import type { Issue, IssueStatusColumn, TenantMember } from '@/features/issues/types'

/**
 * Shared fixtures for tests that exercise the cloud-backed flow: a signed-in
 * session and one joined space whose slug is `cloud-dev`. Tests
 * install them with {@link installSignedInSession} (session only) or
 * {@link installCloudSpaceHandlers} (session and space). The
 * baseline server already answers the probe with 401, so "signed out" needs
 * no handler.
 */

export const TEST_TENANT_ID = '11111111-1111-1111-1111-111111111111'
export const TEST_SPACE_ID = '22222222-2222-2222-2222-222222222222'
export const TEST_USER_ID = '33333333-3333-3333-3333-333333333333'

/** The user `GET /api/v1/me` answers with for a signed-in test session. */
export const TEST_USER = {
  id: TEST_USER_ID,
  displayName: 'Alice',
  status: 'active',
  version: 1,
  createdAt: '2026-09-20T10:00:00+08:00',
  deletedAt: null,
}

/** Makes `GET /api/v1/me` answer as a signed-in member (the gateway cookie is implied). */
export function installSignedInSession(): void {
  server.use(http.get('/api/v1/me', () => HttpResponse.json(TEST_USER)))
}

/**
 * Installs MSW handlers for the shared cloud fixtures: a signed-in session,
 * one `cloud-dev` tenant space where the member holds the
 * given role.
 */
export function installCloudSpaceHandlers(role: string): void {
  installSignedInSession()
  server.use(
    http.get('/api/v1/tenants/:tid/git-credentials', () =>
      HttpResponse.json({ items: [], nextCursor: '' }),
    ),
    http.get('/api/v1/tenants/:tid/projects/:pid/repository', () =>
      HttpResponse.json({ code: 'not_configured' }, { status: 503 }),
    ),
    http.get('/api/v1/tenants/:tid/projects/:pid/repository/operations', () =>
      HttpResponse.json({ items: [], nextCursor: '' }),
    ),
    http.get('/api/v1/me/spaces', () =>
      HttpResponse.json({
        items: [
          {
            id: TEST_SPACE_ID,
            tenantId: TEST_TENANT_ID,
            name: 'Cloud Dev',
            slug: 'cloud-dev',
            description: '',
            createdBy: TEST_USER_ID,
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

/**
 * Installs the three tenant queries an issue board/list mounts — issues,
 * statuses and members — for tests that exercise the `t1` tenant the fixtures
 * assume. Sharing one helper (instead of a per-file `serve*` trio) keeps the
 * clone detector quiet and makes the shape of the trio visible in one place.
 */
export function installTenantIssueHandlers(
  issues: Issue[] = [],
  statuses: IssueStatusColumn[] = [],
  members: TenantMember[] = [],
): void {
  server.use(
    http.get('/api/v1/tenants/t1/issues', () =>
      HttpResponse.json({ items: issues, nextCursor: '' }),
    ),
    http.get('/api/v1/tenants/t1/issue-statuses', () =>
      HttpResponse.json({ items: statuses, nextCursor: '' }),
    ),
    http.get('/api/v1/tenants/t1/members', () =>
      HttpResponse.json({ items: members, nextCursor: '' }),
    ),
  )
}

/** A stored workflow document, as the tenant workflow endpoints return it. */
export function workflowFixture(overrides: Partial<Workflow> = {}): Workflow {
  return {
    id: '44444444-4444-4444-4444-444444444444',
    tenantId: TEST_TENANT_ID,
    name: '安全审查',
    description: '',
    graph: { nodes: [], edges: [], viewport: { x: 0, y: 0, zoom: 1 } },
    version: 1,
    createdAt: '2026-09-20T10:00:00+08:00',
    updatedAt: '2026-09-20T10:00:00+08:00',
    deletedAt: null,
    ...overrides,
  }
}

/**
 * Installs the tenant workflow endpoints for tests of the workflow surface:
 * the list, and one detail route resolving against `workflows` by id — an id
 * that is not in the list answers 404, the way the backend does for an
 * archived or foreign workflow. `installCloudSpaceHandlers` still supplies the
 * tenant and space the page resolves its scope from.
 */
export function installWorkflowHandlers(workflows: Workflow[]): void {
  const byId = new Map(workflows.map((workflow) => [workflow.id, workflow]))
  server.use(
    http.get(`/api/v1/tenants/${TEST_TENANT_ID}/workflows`, () =>
      HttpResponse.json({ items: workflows, nextCursor: '' }),
    ),
    http.get(`/api/v1/tenants/${TEST_TENANT_ID}/workflows/:wfid`, ({ params }) => {
      const found = byId.get(String(params['wfid']))
      if (!found) {
        return HttpResponse.json({ code: 'not_found', message: 'not_found' }, { status: 404 })
      }
      return HttpResponse.json(found)
    }),
  )
}

/**
 * Installs the publish endpoint used by publish dialogs and the import flow,
 * feeding `onBody` the request it received. The shared shape keeps the two
 * suites that publish workflows from each carrying their own copy.
 */
export function installWorkflowPublish(
  onBody: (body: unknown) => void,
  resource: unknown,
  status = 200,
): void {
  server.use(
    http.post(`/api/v1/tenants/${TEST_TENANT_ID}/workflows/:wfid/publish`, async ({ request }) => {
      onBody(await request.json())
      if (status >= 400) {
        return HttpResponse.json({ code: 'workflow_conflict', message: 'conflict' }, { status })
      }
      return HttpResponse.json({ resource })
    }),
  )
}
