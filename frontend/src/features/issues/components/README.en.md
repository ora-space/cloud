# issues/components: issue page sub-components

[中文](README.md) | [English](README.en.md)

## Responsibility

Sub-components of the issue board and detail page: cards and rows, the create dialog, the activity column (timeline, comments, @ targets, workflow form configure/review/confirm), the context-refs column and the properties column. Data comes from `features/issues/api`; nothing here sends HTTP directly.

## Files

| File | Purpose |
| --- | --- |
| `issue-card.tsx` / `issue-row.tsx` | Board card and list row |
| `create-issue-dialog.tsx` / `issue-project-select.tsx` | Creation and detail project selection; loading or failure never clears a stored reference |
| `issue-properties-panel.tsx` | Detail-page properties column: status, priority, assignee, parent/sub-issues, project, labels, properties, context refs |
| `activity-panel.tsx` | Activity column: timeline and new comments/targets |
| `target-picker.tsx` / `pending-targets.tsx` | Loading, empty, search and retry states of the real @ directory, plus pending targets |
| `workflow-interaction-composer.tsx` / `dynamic-form-renderer.tsx` / `form-field-renderer.tsx` / `assist-suggestions.tsx` / `confirm-review.tsx` | Workflow form rendering, AI suggestions, review and confirm |
| `context-refs-panel.tsx` | Adding and removing context refs |
| `agent-request-failure.tsx` | Safe task-refusal messages with project, target and personal-model guidance |
| `*.test.tsx` | Component tests, including refused tasks retaining their draft and actionable settings links |

## Dependencies and invariants

Depends on `features/issues/api`, `features/issues/types`, `features/issues/present`, the real project list in `features/projects/api`, `features/spaces/current-space`, and `components`; used by the `features/issues` pages. Drafts (comments, targets, forms) reach the server only when their own button is pressed; property edits all go through the page's versioned `onCommit`. Project choices come from the current joined space and never fall back to demo data.

Creation drafts remount by tenant/space; activity drafts by tenant/space/issue, including navigation to cached scopes. Project association follows server results; versioned updates disable property edits until settled and keep stored values on refusal. Project guidance targets the `issue-project` selector. Targets come only from the real directory; the server remains authoritative for space compatibility.

## Testing

Component tests use MSW; properties and Task Mode→Thread are covered by `issue-detail-page.test.tsx`. Loading, empty, refused and retry states are distinct; creation, ordinary-comment and target failures retain drafts, while scope changes clear them.
