# settings: settings pages

## Responsibility

Owns settings navigation, the space general page and the signed-in user's "Git 身份" (Git identity) page. Administrators can rename the space with a version guard, which also updates the tenant name; the slug stays fixed. The Git identity belongs to the user, not the space: Agent commits use it as Git author and committer, and a change applies only to sessions started afterwards. Member and billing pages belong to their own feature modules.

It does not implement membership authorization or space archival; `features/model-connections` owns personal model configuration.

## Files

| File | Purpose |
| --- | --- |
| `settings-layout.tsx` | Settings navigation and nested-route container, including personal model connections |
| `general-settings-page.tsx` | Space name editing and read-only slug |
| `git-identity-api.ts` | Hooks for `/api/v1/me/git-identity`: read, versioned save (PUT), restore default (DELETE with a stable idempotency key), and fault messages |
| `git-identity-page.tsx` | The "Git 身份" card: name/email, the "默认身份" badge, "保存" and "恢复默认" |
| `*.test.tsx` | Tests for navigation, administrator editing, member read-only state, and Git identity save/conflict/restore |

## Dependencies and invariants

Depends on `features/spaces` (including `mutationHeaders` and `useIdempotencyKeys`), the generated client, `lib/api-client` (`faultCode`), routing, and UI components; app routes consume it (`settings`, `settings/git-identity`). The server still checks role and version; disabled UI controls are not authorization. The Git identity form remounts per `version`: after a save or restore the drafts come from the server's answer and the next save carries the new version; a version conflict only asks for a refresh and never overwrites the user's draft.

## Testing

MSW simulates the rename endpoint and verifies ordinary members cannot submit edits; the Git identity tests check the version in PUT/DELETE bodies, each fault message, and that "恢复默认" is disabled for the default identity.
