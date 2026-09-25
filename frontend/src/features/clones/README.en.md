# clones: the 仓库 (repositories) page

[中文](README.md) | [English](README.en.md)

## Responsibility

The page behind the 仓库 sidebar entry. It submits clones through the public clones API (`/api/v1/tenants/{tid}/clones`); a Controller claims them and a Node runs `git clone` onto its disk. The page only shows the facts Cloud records. It owns:

- listing the clones the current member submitted in the tenant behind the current space, newest `createdAt` first. Cloud shows each submitter only their own records, so every space of one tenant shows the same list;
- polling every 2 seconds while a clone is pending or the last read failed, and stopping once every clone is terminal;
- writing the submission identity (`requestId`, repository, branch) to the tab's sessionStorage before sending, and clearing it only once Cloud accepted or refused it. After a lost response or a reload the only action offered is "resend the original request".

Not owned: projects (`features/projects`, a separate operation model), session and login (`features/auth`), tenant and space resolution (`features/spaces`), private-repository credentials.

## Files

| File | Purpose |
|---|---|
| `api.ts` | `useCloneList` (the polling list query), `useSubmitClone` (submission; `mutationHeaders` from `features/spaces` sets `Idempotency-Key` to the `requestId`, and fault codes are read with `faultCode` from `lib/api-client`), and the pure `newestFirst`, `awaitsResult`, `classifySubmitFailure` |
| `pending.ts` | The sessionStorage boundary: `pendingSubmissionKey` (one slot per tenant and user), `readPendingSubmission`, `writePendingSubmission` |
| `use-clone-submission.ts` | The submission state machine: store the identity before sending, keep or clear it by failure kind, treat a listed `requestId` as accepted |
| `status.ts` | The stage derived from Cloud's facts (queued / dispatched / succeeded / failed), badge variants, and failure-reason labels |
| `messages.ts` | Submission notices and `rejectionMessage`, which explains a refusal by its fault code |
| `clone-dialog.tsx` | The "Clone 仓库" dialog: a form for a new submission, or a read-only summary of the unconfirmed one with "重试原请求" |
| `clone-list.tsx` | The results table: repository and branch, stage badge, commit / Node path or failure reason / retained path, submission time |
| `repositories-page.tsx` | Page composition: requests nothing until the space and session resolved; remounts per storage key so a user or tenant switch reads its own unconfirmed submission |
| `*.test.ts(x)` | See Testing |

## Dependencies

Depends on: `src/api` (the generated clones client), `lib/api-client` (`faultCode`), `features/auth/session` (user id), `features/spaces/current-space` (tenant id), `features/spaces/api` (`mutationHeaders`), `components/ui`, `components/layout/page-header`, `components/common/dialog-form-field`, TanStack Query.

May be used by: `routes.tsx` (`/w/:workspaceSlug/repositories`).

## Invariants

- Pending only means "no terminal fact yet". It is never shown as failure, timeout or progress. Queued and dispatched differ only by whether an `executionId` was recorded.
- An unconfirmed submission may only be resent unchanged. Its identity is cleared only when Cloud refused it outright (400/403/404/409/422) or accepted it. A network failure, 5xx, 401 or 429 keeps it.
- The storage key must include both the tenant id and the user id. Otherwise, after another account signs in to the same tab, that account could resend the previous account's request.
- When storage cannot be read (it throws, or holds corrupt content), no new identity is minted; that state is never treated as "nothing pending".
- Repository and branch are trimmed before sending. Cloud refuses any whitespace or control character; the client-side checks only give earlier feedback.

## Testing

`pending.test.ts` covers slot isolation, the read/write round trip and every unreadable case. `api.test.ts` covers ordering, the polling predicate, failure classification and refusal messages. `repositories-page.test.tsx` uses MSW to cover: listed facts and ordering (including the interrupted-failure label and retained path); a trimmed submission whose idempotency key equals its `requestId`; an unchanged resend after a lost response; restoring after a reload without resending; confirming once the list shows the request; clearing and explaining a refusal; never exposing another user's unconfirmed submission; polling that recovers from a failed read and stops at terminal states; and sending nothing when storage cannot be read or written. The polling test fakes only `setInterval` and keeps every other timer real, so MSW and findBy do not wait on each other.
