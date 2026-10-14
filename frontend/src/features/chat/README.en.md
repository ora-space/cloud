# chat: standalone chat entry

Standalone chat is unavailable. Both the list and old session URLs explain this and direct members to real issue Agent Threads. They display no contacts, history or composer and issue no demo chat requests.

| File | Responsibility |
| --- | --- |
| `chat-page.tsx` | Retained chat routes with current-workspace task navigation |
| `chat-page.test.tsx` | List and old detail routes never render seeded sessions |

Depends on unavailable and lib/paths; consumed by the route composition root. Real sessions belong to issues/thread; this module owns neither execution nor message storage.
