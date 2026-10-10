# projects: space projects

## Responsibility

Shows the selected tenant space's project list, detail, and actions. Space members share projects; administrators may delete through the existing lifecycle, while ordinary members can read and edit business content.

It does not select tenants, manage members, or manipulate runtime resources directly.

## Files

| File | Purpose |
| --- | --- |
| `api.ts` | Cloud hooks for project reads, creation, updates, and deletion |
| `projects-page.tsx`, `project-detail-page.tsx` | List and detail screens |
| `create-project-dialog.tsx` | Project creation form |
| `status.ts`, `components/` | Status mapping and display components |
| `*.test.tsx` | Page and operation tests |

## Dependencies and invariants

Depends on `features/spaces/current-space`, the generated client, and UI components; app routes consume the pages. Tenant ID comes from the selected space. Sensitive deletion controls appear only to `admin`, and the backend checks authorization again.

## Testing

MSW simulates Cloud project lifecycle, version conflicts, and role controls. Rename tests paste the complete name and verify the optimistic-version request.

Creation requires a concrete branch. The runtime starts and clones asynchronously; shared `CreateFormSubmit` feedback blocks repeated submission while pending.
