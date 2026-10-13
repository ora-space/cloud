# projects: space projects

## Responsibility

Shows the selected tenant space's project list, detail, and actions. Space members share projects; administrators may delete through the existing lifecycle, while ordinary members can read and edit business content. Creating a project allows binding one of the member's own Git credentials, or choosing a public repository with no binding.

It does not select tenants, manage members, or manipulate runtime resources directly.

## Files

| File | Purpose |
| --- | --- |
| `api.ts` | Cloud hooks for project reads, creation, updates, and deletion (creation accepts an optional `credentialRefId`) |
| `provisioning.ts` | The provisioning Operation behind a project, reduced to its terminal failure and the copy for each reason code |
| `projects-page.tsx`, `project-detail-page.tsx` | List and detail screens |
| `create-project-dialog.tsx` | Project creation form with the optional credential picker and credential-refusal copy |
| `status.ts`, `components/` | Status mapping and display components, including the provisioning-failure notice |
| `*.test.tsx` | Page, hook, and operation tests |

## Dependencies and invariants

Depends on `features/spaces/current-space`, `features/settings/git-credentials-api` (to list the member's own credentials), the generated client, and UI components; app routes consume the pages. Tenant ID comes from the selected space. Sensitive deletion controls appear only to `admin`, and the backend checks authorization again. A credential is optional at creation and `credentialRefId` is sent only when one is chosen; the server answers `repository_credential_frozen` for an unavailable credential and `repository_credential_scope_mismatch` for a scope mismatch, and the interface turns both codes into actionable advice.

A project's creation is asynchronous, and its outcome lives on the Operation, not on the project. The public contract has no per-project operations route, so `provisioning.ts` reaches the Operation through the project's Workspace list (`creatorOperationId`) and reads its `errorCode`. Only `state === 'failed'` is a failure: `running`, `retry_wait`, and `blocked` are work in progress, and reporting them as failures would tell a member to abandon something still running. The reason code is shown verbatim next to the copy, and a code with no copy falls back to the raw code rather than a reassuring sentence. Both deterministic refusals of a credentialed clone — no executor installed (`repository_credential_executor_unavailable`) and a reference that does not declare the capability this clone needs (`repository_credential_capability_unsupported`) — carry actionable copy, because each names a fact a member or an administrator can act on. The notice offers no retry, because the public retry route refuses a `failed` Operation.

## Testing

MSW simulates the Cloud project lifecycle, version conflicts, and role controls. Creation requires a concrete branch. The runtime starts and clones asynchronously; shared `CreateFormSubmit` feedback blocks repeated submission while pending. Tests cover no credential, a chosen credential, and both credential refusal codes. They also cover a settled provisioning failure reaching the detail page, and the reasons that must not be reported as one.
