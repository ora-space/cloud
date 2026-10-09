# issues/components: issue page sub-components

[中文](README.md) | [English](README.en.md)

## Responsibility

Sub-components of the issue board and detail page: cards and rows, the create dialog, the activity column (timeline, comments, @ targets, workflow form configure/review/confirm), the context-refs column and the properties column. Data comes from `features/issues/api`; nothing here sends HTTP directly.

## Files

| File | Purpose |
| --- | --- |
| `issue-card.tsx` / `issue-row.tsx` | Board card and list row |
| `create-issue-dialog.tsx` | New-issue dialog |
| `issue-properties-panel.tsx` | Detail-page properties column: status, priority, assignee, parent/sub-issues, project, labels, properties, context refs |
| `activity-panel.tsx` | Activity column: timeline and new comments/targets |
| `target-picker.tsx` / `pending-targets.tsx` | @ target picking and pending targets |
| `workflow-interaction-composer.tsx` / `dynamic-form-renderer.tsx` / `form-field-renderer.tsx` / `assist-suggestions.tsx` / `confirm-review.tsx` | Workflow form rendering, AI suggestions, review and confirm |
| `context-refs-panel.tsx` | Adding and removing context refs |
| `*.test.tsx` | Component tests |

## Dependencies and invariants

Depends on `features/issues/api`, `features/issues/types`, `features/issues/present`, `components`; used by the `features/issues` pages. Drafts (comments, targets, forms) reach the server only when their own button is pressed; property edits all go through the page's versioned `onCommit`.

## Testing

Component tests use MSW; the properties column is covered by `issue-detail-page.test.tsx`.
