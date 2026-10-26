import type { Project } from '@/api/generated.schemas'
import type { CollaborationTargetSummary, Issue, IssueStatusColumn } from '@/features/issues/types'

/**
 * Builds a complete Cloud `Issue` so a test only names the fields it varies
 * (`id`, `title`, and any `overrides`). Everything else gets a neutral default.
 */
export function makeIssue(id: string, title: string, overrides: Partial<Issue> = {}): Issue {
  return {
    id,
    title,
    tenantId: 't1',
    creatorUserId: 'u1',
    assigneeType: 'user',
    assigneeId: null,
    assigneeUserId: null,
    parentIssueId: null,
    projectRef: null,
    description: '',
    status: 'backlog',
    priority: 'none',
    position: 0,
    number: 1,
    properties: {},
    version: 1,
    createdAt: '2026-01-01T00:00:00.000Z',
    updatedAt: '2026-01-01T00:00:00.000Z',
    labels: [],
    ...overrides,
  }
}

/** Builds a status column; `key` is the only field tests normally vary. */
export function makeStatus(
  key: string,
  overrides: Partial<IssueStatusColumn> = {},
): IssueStatusColumn {
  return {
    id: `st-${key}`,
    tenantId: 't1',
    key,
    name: key,
    description: '',
    category: 'unstarted',
    color: '',
    icon: '',
    isSystem: true,
    position: 0,
    version: 1,
    createdAt: '',
    updatedAt: '',
    ...overrides,
  }
}

/** A real Cloud Project shape; overrides can place it in the tenant/space under test. */
export function makeProject(id: string, overrides: Partial<Project> = {}): Project {
  return {
    id,
    tenantId: 't1',
    spaceId: 's1',
    name: `Repository ${id}`,
    repositoryUrl: 'https://example.invalid/repository.git',
    defaultBranch: 'main',
    credentialRefId: null,
    deletedAt: null,
    ownerUserId: 'u1',
    lifecycle: 'active',
    version: 1,
    createdAt: '2026-10-01T00:00:00Z',
    ...overrides,
  }
}

/** Installed-Agent directory entry with the server's Task Mode descriptor. */
export function makeAgentTarget(
  id: string,
  displayName = 'Installed Agent',
): CollaborationTargetSummary {
  return {
    type: 'agent',
    id,
    displayName,
    description: 'Installed Agent plugin',
    interactionDescriptor: { mode: 'task', requiresTask: true },
  }
}
