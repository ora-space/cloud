# Session Reads: Session Lists, Archives and Model Request Statistics

English | [中文](session-reads.md)

Besides the live [Thread](threads.en.md), Cloud offers a few read-only entry points for browsing and
analysing Agent sessions that already happened: a tenant's session list, the session archive registered
for a run, and statistics of the HTTP requests that went through the model gateway. They read only
authorization records Cloud has persisted and archives it has registered; they create no session, change
no run state and never scan a user's personal file system.

## Shared rules

- **Read-only identity resolution.** These entry points are dispatched before the general public request
  transaction: they recognize only existing identities and Huawei aliases, check identity conflicts, account
  status and current tenant membership, and never create a user, bind an alias or refresh runtime state.
- **Strict query parameters.** Only the documented single-valued parameters are accepted; unknown, repeated
  or empty ones return `400 invalid_session_filter`. The archive and statistics endpoints accept no query
  parameters at all, so clients cannot pass URLs, paths or object keys. The Gateway already refuses query
  strings it cannot parse before forwarding.
- Responses carry `Cache-Control: private, no-store`.
- **Only existing evidence is shown.** Session records are not guaranteed to contain tokens, cost,
  parent/child session relations or each tool's input, output and duration; missing information should be
  shown as unavailable and never filled with defaults.

## The session list

`GET /api/v1/tenants/:tid/agent-sessions` lists the tenant's persisted Agent IssueRuns, excluding deleted runs
and Issues. Chats and WorkflowRuns are not sessions here.

- Each item carries the run and Issue identifiers, the Issue title and number, the nullable Project and Agent
  plugin, the run and Thread states, the created/started/completed times, a nullable frozen model summary, and
  `historyAvailable`.
- Filters: `projectId` (Project UUID), `status` (one run status), `agent` (exact plugin identifier), and `from`
  (inclusive) and `to` (exclusive) on creation time, in RFC 3339.
- Ordered by descending `(createdAt, id)`, 50 per page by default and at most 100; `after` is the previous
  page's `nextCursor` and must be an Agent run visible in the current tenant; `before` is not supported.
- `historyAvailable` only says an unexpired Revision registration exists; it guarantees neither that object
  storage is reachable now nor that the file is within the read limits.

## Session archives

`GET /api/v1/tenants/:tid/issues/:iid/runs/:rid/history` reads the complete session JSONL registered for this
run:

1. A short transaction checks membership, the Issue, the Agent run and an unexpired Revision registration.
   The object key can only come from the registration and must match
   `revisions/{tenant}/{run}/{work}/session.jsonl` exactly.
2. After that transaction commits, the object is read with a signed `GET` against the private endpoint: no
   redirects, cancellation propagated, a 30-second storage timeout. The actual bytes' size and SHA-256 must
   match the registration, and at most 32 MiB and 100,000 valid UTF-8 JSONL objects are read.
3. Before responding, a new transaction checks **again** that current permissions and the registration still
   hold: a member revoked or a registration replaced during the read cannot let the request go on to return
   session content.

A successful response is `{"source":"archive","records":[...],"sizeBytes":…,"sha256":"…"}`. `records` keeps the
file order, the original fields, JSON number precision and repeated updates of the same `seq`, for consumers to
replay with session record semantics. The `seq` inside records is the session record's own sequence and a
different sequence from the `seq` used for Cloud Thread paging; neither stands in for the other.

| Situation | Response |
|---|---|
| No registration, or the registration expired | `404 history_unavailable` |
| The actual size or record count exceeds the limits | `413 history_too_large` |
| Integrity check failed or the JSONL is corrupt | `502 history_invalid` |
| Storage failure | `502 history_unavailable` |

Neither public responses, errors nor logs contain the private storage address, signed URLs, object keys or
credentials. The archive is not a live stream; live display keeps using the Thread and the Space's SSE
invalidation hints.

## Model request statistics

Every upstream HTTP attempt that goes through [model-gateway](../models/model-gateway.en.md) is recorded on its
own in `model_request_attempts`: the gateway registers it before actually sending the request and writes the
completion observation afterwards. A client retry is a new attempt; SSE chunks and Thread records are not request
counts. The ledger holds no request body, key, headers, raw provider error or user file path. A `started`
observation left behind by a service crash stays unknown; nothing infers success, failure or zero usage from it.

Both read endpoints are read-only, check current membership, the Issue and the Agent run, and do not depend on
whether an archive exists:

- `GET .../runs/:rid/statistics` aggregates every recorded attempt of the run and accepts no query parameters.
- `GET .../runs/:rid/model-calls` pages through the attempts in ascending `(startedAt, id)` and accepts only
  `limit` (50 by default, at most 100) and a UUID `after` from the same run.

The source is fixed as `model_gateway` and the scope as `recorded_http_attempts`: this is neither the complete
request count of historical sessions nor a bill.

- Six kinds of token (input, output, total, cache read, cache write, reasoning) each keep the provider's value
  together with coverage: every metric carries `value`, `knownCount`, `totalCount` and `availability`; unknown is
  `null`, and a measured 0 stays 0.
- Cumulative usage snapshots in SSE replace rather than add; a snapshot without a terminal event is partial data
  only. OpenAI's input includes cache reads while Anthropic's excludes cache reads and writes; reasoning is a
  subset of output and is never added to it again. When the provider reports no total, no total is guessed, and
  cost is not estimated.
- Durations come from the gateway's monotonic clock; the time to the first useful output excludes pings, empty
  roles and usage-only events, and it is not the model's TTFT. Aggregates do not change with paging.
- `started` may be a request still in progress, or an unfinished observation left by an exited process.

An Agent runtime's final `turnEnded` may also carry ACP `token_usage`; that is a different source and is never
added to the gateway statistics.

## Related decisions

- [Session analysis builds a read-only view from Cloud authorization records and registered archives](https://github.com/ora-space/specs/blob/main/decisions/cloud/thread/20261016-session-analysis-read-model.md) (approved)
- [Model usage is recorded per gateway HTTP attempt, keeping partial observations and unknown values](https://github.com/ora-space/specs/blob/main/decisions/cloud/model-access/20261016-recorded-http-attempt-statistics.md) (approved)
