# Scheduled tasks

This module owns the **screens** for scheduled tasks: the list and form, and one task's run history.
It owns no server fact: admission, verdicts and artifact counts all come from Cloud's REST
resources; the module requests, renders and turns user actions into mutations.

## Files

| File | Responsibility |
| --- | --- |
| `api.ts` | The queries and mutations this module uses (list, history, create, enable/disable) |
| `scheduled-tasks-page.tsx` | The page: list, creation entry, opening a task's history |
| `scheduled-task-form.tsx` | Create and edit form |
| `scheduled-task-history.tsx` | History dialog: each occurrence's admission result, verdict and artifact counts |
| `scheduled-tasks-page.test.tsx` | Page behavior tests |
| `scheduled-run-artifacts.tsx` | Expands registered artifacts and offers same-origin attachment links |
| `scheduled-run-artifacts.test.tsx` | Same-origin attachment links, on-demand lists and retry tests |
| `scheduled-task-history.test.tsx` | History rendering tests, including "not reported" |

## Dependencies

- Depends on: the generated client under `@/api`, `@/components/ui`, and the boundary adapters in
  `@/lib`.
- May be depended on by: the route/page composition layer.

## Invariants

- **"Not reported" is a value, not a missing field.** A run without a verdict renders 未报告; do not
  hide the line, and never treat an empty string as a pass.
- **The counts describe registered objects only.** `artifactCount` comes from Cloud's index and
  `rejectedArtifactCount` counts refused candidates; they do not add up, and neither says whether the
  reader may download anything - Cloud authorises downloads separately, by issue visibility.
- **Server facts are not cached in the frontend.** History and counts are read through TanStack Query
  only and are never copied into component state.

## Tests

The history tests serve fixed responses through MSW and assert what renders (the verdict text and the
rejection count). Do not mock internal modules.

The creation entry trusts the Cloud list response’s `acceptanceEnabled`; this module does not choose the gate, and the server independently validates writes. The form may select a model from the creator's default connection; its connection and credential are frozen at admission, while no selection uses the then-current default model. A disabled create is an unauthorised draft and sends no approval; a stopped task must re-check the risk disclosure before it can be enabled.

The card’s “next run” only renders `nextDueAt`, which Cloud recomputes on every read, formatted in the task timezone; the frontend never evaluates the schedule itself, and a disabled task has no value.

Artifact lists contain metadata only. Download links target same-origin Cloud routes; each click rechecks authorization and verifies the file before serving a safe attachment. Browsers receive no storage URL, credentials or expiry; artifact content is never rendered inside the app.

History uses `artifactsExpired` to identify artifacts removed after successful delivery; a run that produced none is never labeled expired.

A failed save renders a bounded message per fault code and never the server's detail. `name_conflict` means the creator already has a task with that name on the same issue (a deleted task still holds its name); the message asks for a rename and the form stays open for the retry.

The failure reasons `task_paused` and `task_handoff_pending` render as a pointer to the task conversation: they are resolved there, not in the task or Agent configuration. Other failure reasons keep their raw code.
