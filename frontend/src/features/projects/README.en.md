# projects: space projects

## Responsibility

Shows the selected tenant space's project list, detail, and actions. Space members share projects; administrators may delete through the existing lifecycle, while ordinary members can read and edit business content. Creating a project allows binding one of the member's own Git credentials, or choosing a public repository with no binding.

It does not select tenants, manage members, or manipulate runtime resources directly.

## Files

| File | Purpose |
| --- | --- |
| `api.ts` | Cloud hooks for project reads, creation, updates, and deletion (creation accepts an optional `credentialRefId`) |
| `provisioning.ts` | The provisioning Operation behind a project, reduced to a provisioning phase, its terminal failure, and the copy for each reason code |
| `deletion.ts` | The reason codes a project deletion can be refused with, and the actionable sentence for each |
| `projects-page.tsx`, `project-detail-page.tsx` | List and detail screens |
| `create-project-dialog.tsx` | Project creation form with the optional credential picker and credential-refusal copy |
| `status.ts`, `components/` | Status mapping and display components, including the provisioning-failure notice |
| `*.test.tsx`, `*.test.ts` | Page, hook, operation, and copy tests |

## Dependencies and invariants

Depends on `features/spaces/current-space`, `features/settings/git-credentials-api` (to list the member's own credentials), the generated client, and UI components; app routes consume the pages. Tenant ID comes from the selected space. Sensitive deletion controls appear only to `admin`, and the backend checks authorization again. A credential is optional at creation and `credentialRefId` is sent only when one is chosen; the server answers `repository_credential_frozen` for an unavailable credential and `repository_credential_scope_mismatch` for a scope mismatch, and the interface turns both codes into actionable advice.

A refusal of the delete request is a different fact from a provisioning failure, and `deletion.ts` owns its copy. It reads the code through `faultCode` in `lib/api-client` — the one boundary wrapper for Cloud's `Fault` body — and renders a sentence beside the code in the delete dialog. A deletion refused because a Workspace is in use, because the same project is already being deleted, or because the member's role changed says so and says what ends it, rather than rendering the bare code; a rejection that carries no readable code renders nothing, because claiming Cloud named a reason it did not would send the member after the wrong cause. The delete refused for a project in use leaves the page where it is, so the member can act and submit again; only a deletion the Cloud accepted navigates away.

A project's creation is asynchronous, and its outcome lives on the Operation, not on the project. The public contract has no per-project operations route, so `provisioning.ts` reaches the Operation through the project's Workspace list (`creatorOperationId`) and reads its `errorCode`. The hook reduces the Operation state to four phases (`provisioning`, `retrying`, `failed`, `succeeded`), and the status badge on both the list and the detail page shows the phase first: the lifecycle has no failure state, so a project whose creation failed for good stays `provisioning` forever and the lifecycle alone would keep reading "规划中". An unknown phase — no Workspace yet, or a state this build has not been taught — falls back to the lifecycle status rather than asserting a state the frontend cannot prove. Only `state === 'failed'` is a failure: `running` is `provisioning`, `retry_wait` and `blocked` are `retrying`, and reporting them as failures would tell a member to abandon something still running. The list page asks about a provisioning Operation only for a project still being created (`status === 'planned'`): a settled project has no answer left to change, and asking would cost two pointless requests per card. The reason code is shown verbatim next to the copy, and a code with no copy falls back to the raw code rather than a reassuring sentence. Both deterministic refusals of a credentialed clone — no executor installed (`repository_credential_executor_unavailable`), a reference that does not declare the capability this clone needs (`repository_credential_capability_unsupported`), and a credential the member deleted (`repository_credential_revoked`, whose token is destroyed and whose refusal can therefore never end) — carry actionable copy, because each names a fact a member or an administrator can act on. The notice offers no retry, because the public retry route refuses a `failed` Operation.

## Testing

MSW simulates the Cloud project lifecycle, version conflicts, and role controls. Creation requires a concrete branch. The runtime starts and clones asynchronously; shared `CreateFormSubmit` feedback blocks repeated submission while pending. Tests cover no credential, a chosen credential, and both credential refusal codes. They also cover a settled provisioning failure replacing "规划中" on both the list and the detail page, a retry being kept apart from a terminal failure, and a settled project never being asked about. For deletion they cover the accepted path, a refusal shown with its actionable sentence and code, and a duplicate deletion reading as already in progress; `deletion.test.ts` pins that every reason a delete may answer has copy of its own, that a duplicate and a refusal do not read alike, and that an untaught code falls back to the code.
