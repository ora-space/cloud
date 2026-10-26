# Task conversation

[中文](README.md) | English

Task-level conversation reads, sends, pending handoffs, and Agent selection. The current session initiator can confirm a graceful end on the task page; “Stop current execution” still cancels and pauses the queue. A run's Thread remains owned by `issues/thread`; this module does not own execution state or Node connections.

| File                                             | Responsibility                                                                                                                       |
| ------------------------------------------------ | ------------------------------------------------------------------------------------------------------------------------------------ |
| `conversation-footer.tsx`                        | Current-execution controls and persistent input footer                                                                               |
| `api.ts`                                         | Bidirectional task message paging, send/stop/cancel/retry/resume/explicit-discard API and safe error copy.                           |
| `conversation-panel.tsx`                         | Combines task comments, Agent Runs and pending messages; offers authorized ending only for the session currently occupying the task. |
| `conversation-composer.tsx`                      | One Agent target, explicit mention picker, Agent message/plain comment modes and handoff hint.                                       |
| `use-conversation-draft.ts`                      | One task-scoped draft and history quote; changing the Agent only changes the input target.                                           |
| `task-stop.tsx` / `task-recovery.tsx`            | Stop the current execution, retain a task pause and explicitly recover saved/discarded results independently of history browsing.    |
| `agent-readiness.tsx` / `use-composer-submit.ts` | Show current-user model/project gaps before sending, share submission eligibility and preserve refused drafts.                       |
| `*.test.ts(x)`                                   | MSW and component scenarios for closed-session continuation, switching, failure and queueing.                                        |

Depends on generated `src/api`, `features/issues/api`, `features/issues/thread`, `features/spaces/api`, `features/conversation-content`, and `components/ui`. Only server-authorized roster targets are selectable; the selector and `@` picker change recipient only on explicit selection, while an ordinary typed `@name` has no routing effect. Changing a local selection never starts a run. Historical Run selection is read-only and the task composer always addresses the current target. The end action belongs only to the server's `activeRunId` when Thread `canEnd` is true and the current user initiated that session; a later queued Run cannot hide the active session's end action. Pages merge by server seq, and a retry after an unknown response reuses its idempotency key.

`conversation-footer.tsx` keeps current-session end, task stop/recovery and the composer outside scrolling content. Controls always target the active execution. Lightweight Run separators preserve transcript ordering; authors derive from persisted messages or Thread initiators, and only the current actor is called “you”. Agent names are supplied explicitly. The thread viewport owns scrolling and denied reads hide stale messages.

Conversation reads continue polling while activeRunId exists or messages are waiting. The server clears active ownership after release; an empty queue must not leave stale execution controls visible.

A failed or cancelled preparation may have no delivery result. It keeps the failure pause and exposes the same explicit discard-and-continue action. The server requires confirmed release, the initiator's or an administrator's authority, and explicit acknowledgement; it never silently advances the queue or rewrites the failed run as successful.

A configuration gap for the currently selected personal Agent also refreshes readiness every five seconds, even before a Run or queued message exists. Repairing it in another window restores sending while preserving the draft. Once repaired, in comment-only mode, or after choosing another Agent, the old gap no longer drives polling. Authorization refusals stop polling, and the server still rechecks every send.

The composer occupies normal document flow, never overlays task history, and requires no fixed-height spacer to read the latest message.

Readiness failures link to the owner of the missing configuration: missing plugin declarations, model bindings and stale declarations lead to plugin settings, while missing connections, credentials and default models lead to personal model connections. An unavailable adapter directs the user to the deployment administrator instead of presenting every refusal as a transient failure or asking them to configure the model again.

Unavailable connections, missing keys and withdrawn selected models consume the server's `model_connection_unavailable`, `model_credential_required` and `model_not_found` codes; internal workflow resolver reasons are not part of the task conversation protocol. An unconfigured plugin model selection uses `agent_configuration_required` and retains the plugin settings link.

Task messages, comments and Runs mark output with stable identities; Threads mark their own tail sequence/live revision. The shared viewport uses these signals to distinguish older history from new output, without treating paging controls or historical disclosures as unread content.

The recipient and @ menus distinguish Agents from workflows. Selecting a workflow opens the existing version form; only explicit Confirm starts a run. Cancelling retains the Agent draft and retry identity. conversation-workflow.tsx composes the existing form; workflow-run-card.tsx reads the linked WorkflowRun status, frozen version and detail link, without reading an Agent Thread. Display ordering includes both kinds, while Agent send/stop/handoff controls still receive only Agent runs; this does not add a mixed queue or result handoff. Additional dependencies are features/issues/components/workflow-interaction-composer and features/workflows/api. conversation-workflow.test.tsx covers mutation-free selection, cancellation, Confirm and historical workflow identity.
