import { screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { expect } from 'vitest'
import type {
  GitCredential,
  Project,
  ProjectRepository,
  RepositoryOperation,
  Workflow,
  WorkflowSnapshot,
  WorkspaceBranch,
  WorkspaceListItem,
} from '@/api/generated.schemas'
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
 * A published version of the fixture workflow, as the snapshot endpoints return it.
 *
 * Versions are numbered per workflow and immutable, so the id and the default name both derive
 * from the number; a test that cares about the label passes one.
 */
export function workflowSnapshotFixture(
  version: number,
  overrides: Partial<WorkflowSnapshot> = {},
): WorkflowSnapshot {
  return {
    id: `snap-${version}`,
    tenantId: TEST_TENANT_ID,
    workflowId: '44444444-4444-4444-4444-444444444444',
    name: `版本 ${version}`,
    version,
    graph: { nodes: [], edges: [], viewport: { x: 0, y: 0, zoom: 1 } },
    createdAt: '2026-09-20T10:00:00+08:00',
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

/** The Project the repository fixtures belong to. */
export const TEST_PROJECT_ID = '55555555-5555-5555-5555-555555555555'

/** One Project of the shared `cloud-dev` space, active unless a test says otherwise. */
export function projectFixture(overrides: Partial<Project> = {}): Project {
  return {
    id: TEST_PROJECT_ID,
    tenantId: TEST_TENANT_ID,
    spaceId: TEST_SPACE_ID,
    name: 'Cloud Dev',
    repositoryUrl: 'https://example.com/acme/cloud.git',
    defaultBranch: 'main',
    credentialRefId: null,
    lifecycle: 'active',
    ownerUserId: TEST_USER_ID,
    version: 1,
    createdAt: '2026-09-20T10:00:00+08:00',
    deletedAt: null,
    ...overrides,
  }
}

/**
 * Installs the space's Project list, which surfaces that pick a Project to act
 * in read. {@link installCloudSpaceHandlers} supplies the space itself; this
 * answers the projects query scoped by its id.
 */
export function installSpaceProjectHandlers(projects: Project[] = [projectFixture()]): void {
  server.use(
    http.get(`/api/v1/tenants/${TEST_TENANT_ID}/spaces/${TEST_SPACE_ID}/projects`, () =>
      HttpResponse.json({ items: projects, nextCursor: '' }),
    ),
  )
}

/**
 * Picks the fixture Project in a run's start dialog.
 *
 * A run must state the Project its Workspace provisions in, and the dialog offers no default, so a
 * test that starts a run goes through this first. The trigger stays disabled until the space's
 * Project list resolves, hence the wait before opening it.
 */
export async function chooseProject(name = 'Cloud Dev'): Promise<void> {
  const user = userEvent.setup()
  const picker = await screen.findByRole('combobox', { name: '项目' })
  await waitFor(() => expect(picker).toBeEnabled())
  await user.click(picker)
  await user.click(await screen.findByRole('option', { name }))
}

/** The Binding the repository fixtures expose. */
export const TEST_BINDING_ID = '66666666-6666-6666-6666-666666666666'
/** The credential the repository fixtures bind and offer for publication. */
export const TEST_CREDENTIAL_ID = '77777777-7777-7777-7777-777777777777'
/** The commit the Binding's snapshot holds, and what a publish expects to find. */
export const TEST_SNAPSHOT_OID = 'a'.repeat(40)
/** The commit of a Workspace branch that is not the Binding's snapshot. */
export const TEST_BRANCH_OID = 'b'.repeat(40)

/**
 * A stored, push-capable credential, owned by the signed-in test member unless
 * a test says otherwise, as the tenant credential list returns it.
 */
export function gitCredentialFixture(overrides: Partial<GitCredential> = {}): GitCredential {
  return {
    id: TEST_CREDENTIAL_ID,
    tenantId: TEST_TENANT_ID,
    ownerUserId: TEST_USER_ID,
    name: 'deploy key',
    attribution: 'personal',
    repositoryScope: 'https://example.invalid/r.git',
    secretState: 'stored',
    availability: 'available',
    capabilities: ['read', 'publish'],
    requestedCapabilities: ['read', 'publish'],
    capabilityEvidence: {},
    storeVersion: 1,
    version: 1,
    createdAt: '2026-09-20T10:00:00+08:00',
    ...overrides,
  }
}

/**
 * The Project's code source: one Binding on `main` whose credential is stored
 * and whose snapshot no publish has superseded.
 */
export function repositoryFixture(overrides: Partial<ProjectRepository> = {}): ProjectRepository {
  return {
    binding: {
      id: TEST_BINDING_ID,
      projectId: TEST_PROJECT_ID,
      repositoryUrl: 'https://example.invalid/r.git',
      branch: 'main',
      credentialRefId: TEST_CREDENTIAL_ID,
      resyncRequested: false,
      version: 4,
    },
    credential: gitCredentialFixture(),
    snapshot: {
      commitOid: TEST_SNAPSHOT_OID,
      forced: false,
      recordedAt: '2026-10-01T09:00:00+08:00',
      snapshotRef: 'refs/ora/snapshots/binding',
      syncOperationId: '88888888-8888-8888-8888-888888888888',
    },
    latestSync: { ...operationBase(), type: 'repository_sync', state: 'succeeded' },
    ...overrides,
  }
}

/** One publish Operation in `awaiting_approval`, as the Operations list returns it. */
export function repositoryOperationFixture(
  overrides: Partial<RepositoryOperation> = {},
): RepositoryOperation {
  return { ...operationBase(), ...overrides }
}

function operationBase(): RepositoryOperation {
  return {
    id: '99999999-9999-9999-9999-999999999999',
    tenantId: TEST_TENANT_ID,
    projectId: TEST_PROJECT_ID,
    bindingId: TEST_BINDING_ID,
    type: 'repository_publish',
    state: 'awaiting_approval',
    trigger: 'preset',
    requestedBy: TEST_USER_ID,
    approvedBy: null,
    credentialRefId: TEST_CREDENTIAL_ID,
    credentialVersion: 1,
    targetBranch: 'main',
    expectedOldOid: TEST_SNAPSHOT_OID,
    sourceRef: 'refs/ora/deliveries/one',
    sourceCommit: TEST_SNAPSHOT_OID,
    errorCode: null,
    checkAttempts: 0,
    replayCount: 0,
    result: {},
    createdAt: '2026-10-02T09:00:00+08:00',
  }
}

/** One Workspace a publish may take its source branch from. */
export function workspaceFixture(overrides: Partial<WorkspaceListItem> = {}): WorkspaceListItem {
  return {
    id: 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa',
    tenantId: TEST_TENANT_ID,
    projectId: TEST_PROJECT_ID,
    ownerUserId: TEST_USER_ID,
    creatorUserId: TEST_USER_ID,
    creatorEvidence: 'member',
    kind: 'main',
    desiredState: 'running',
    observedState: 'ready',
    canUse: true,
    runtimeGeneration: 1,
    version: 1,
    createdAt: '2026-09-20T10:00:00+08:00',
    deletedAt: null,
    ...overrides,
  }
}

/** One branch of a Workspace, as the publish dialog lists it. */
export function branchFixture(overrides: Partial<WorkspaceBranch> = {}): WorkspaceBranch {
  return { ref: 'refs/heads/topic', branch: 'topic', commit: TEST_BRANCH_OID, ...overrides }
}

/**
 * Installs the reads the repository panel mounts — the Binding, its Operations
 * and the tenant credential list — for the fixture Project.
 */
export function installRepositoryHandlers({
  source = repositoryFixture(),
  operations = [],
  credentials = [gitCredentialFixture()],
}: {
  source?: ProjectRepository
  operations?: RepositoryOperation[]
  credentials?: GitCredential[]
} = {}): void {
  server.use(
    http.get(`/api/v1/tenants/${TEST_TENANT_ID}/projects/${TEST_PROJECT_ID}/repository`, () =>
      HttpResponse.json(source),
    ),
    http.get(
      `/api/v1/tenants/${TEST_TENANT_ID}/projects/${TEST_PROJECT_ID}/repository/operations`,
      () => HttpResponse.json({ items: operations, nextCursor: '' }),
    ),
    http.get(`/api/v1/tenants/${TEST_TENANT_ID}/git-credentials`, () =>
      HttpResponse.json({ items: credentials, nextCursor: '' }),
    ),
  )
}

/** Installs the two lists the publish dialog reads its source branch from. */
export function installPublishSourceHandlers({
  workspaces = [workspaceFixture()],
  branches = [branchFixture()],
}: { workspaces?: WorkspaceListItem[]; branches?: WorkspaceBranch[] } = {}): void {
  server.use(
    http.get(`/api/v1/tenants/${TEST_TENANT_ID}/projects/${TEST_PROJECT_ID}/workspaces`, () =>
      HttpResponse.json({ items: workspaces, nextCursor: '' }),
    ),
    http.get(
      `/api/v1/tenants/${TEST_TENANT_ID}/projects/${TEST_PROJECT_ID}/repository/workspaces/:wid/branches`,
      () => HttpResponse.json({ items: branches, nextCursor: '' }),
    ),
  )
}
