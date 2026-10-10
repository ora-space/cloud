# projects: space projects

## Responsibility

Shows the selected tenant space's project list, detail, and actions. Space members share projects; administrators may delete through the existing lifecycle, while ordinary members can read and edit business content. Creating a project allows binding one of the member's own Git credentials, or choosing a public repository with no binding.

It does not select tenants, manage members, or manipulate runtime resources directly.

## Files

| File | Purpose |
| --- | --- |
| `api.ts` | Cloud hooks for project reads, creation, updates, and deletion (creation accepts an optional `credentialRefId`) |
| `projects-page.tsx`, `project-detail-page.tsx` | List and detail screens |
| `create-project-dialog.tsx` | Project creation form with the optional credential picker and credential-refusal copy |
| `status.ts`, `components/` | Status mapping and display components |
| `*.test.tsx` | Page and operation tests |

## Dependencies and invariants

Depends on `features/spaces/current-space`, `features/settings/git-credentials-api` (to list the member's own credentials), the generated client, and UI components; app routes consume the pages. Tenant ID comes from the selected space. Sensitive deletion controls appear only to `admin`, and the backend checks authorization again. A credential is optional at creation and `credentialRefId` is sent only when one is chosen; the server answers `repository_credential_frozen` for an unavailable credential and `repository_credential_scope_mismatch` for a scope mismatch, and the interface turns both codes into actionable advice.

## Testing

MSW simulates the Cloud project lifecycle, version conflicts, and role controls. Creation requires a concrete branch. The runtime starts and clones asynchronously; shared `CreateFormSubmit` feedback blocks repeated submission while pending. Tests cover no credential, a chosen credential, and both credential refusal codes.
