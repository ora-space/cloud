# repositories: external Git line

## Responsibility

Manages a project's binding to an external Git repository: shows the current code source (repository, branch, snapshot, latest sync), triggers a sync a member asked for, changes the reading credential, and starts, approves, cancels and checks one explicit publish. Credential secrets go straight to the Credential Store; only metadata appears here.

It does not clone repositories, run pushes or decide fast-forwardness (Cloud and repo-executor do), and it does not select tenants or spaces.

## Files

| File | Purpose |
| --- | --- |
| `api.ts` | Cloud hooks for the code source, operation history, credentials and publish actions |
| `repository-panel.tsx` | Code source panel and publication history |
| `publish-dialog.tsx`, `publish-form.ts` | Explicit publish form; `publish-form.ts` owns the field state and the two derived rules |
| `credential-page.tsx`, `credential-select.tsx`, `submission.ts` | Credential list, binding selection and direct submission |
| `credential-authority.ts` | The single "who may act on this credential" decision |
| `*.test.ts(x)` | Module tests |

## Dependencies and invariants

Depends on `features/spaces/current-space` (tenant id and role), the generated client and UI components; project screens consume this module.

- The expected external commit (`expectedOldOid`) is derived from the target branch: the snapshot counts only while the target is still the Binding's branch, and every other branch is treated as one the publish creates (the all-zero oid).
- The submit label and the approval controls express permission only; they are not authorization, which the backend decides again.
- The credential list shows every credential Cloud returns and narrows only the controls, by the decision in `credential-authority.ts` (revocation additionally admits a tenant administrator, because revoking only tightens access); a token never reaches the frontend or its query cache.
- One request describes one source: a filled Revision stops the branch from being sent.
- The lists (credentials, operation history, publishable Workspaces) follow their cursor to the last page: the backend returns at most 100 rows per page and rejects a larger `limit` with 400 `invalid_pagination` rather than truncating.

## Testing

MSW serves the code source, operation history, credentials, workspaces and branches; the `repositoryFixture`, `repositoryOperationFixture` and `installRepositoryHandlers` fixtures in `src/test/cloud-handlers.ts` are shared with later modules. Tests compare whole request bodies and assert the role differences a member sees; no internal module is mocked.
