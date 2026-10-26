# internal/collab: development collaboration fixtures

[中文](README.md) | [English](README.en.md)

Development doubles for collaboration ports declared by `core`. They do not access the database or perform real Agent, Team, model or workflow work.

| File / type | Responsibility |
|---|---|
| `FixtureAgentTeamDirectory` | Two fixed Agents and one Team; name filtering and type/ID resolution |
| `FallbackDirectory` | Resolves through the real directory first; combines fixture and real lists, with Workflows always from the Store |
| `DeterministicContextBuilder` | Arranges Store-supplied task, Issue, recent comments, confirmed input and context references |
| `MockExecutionDispatcher` | Accepts then synchronously reports simulated start, messages and completion through the observer; Workflow messages become system activities |
| `MockInputAssistProvider` | Suggests deterministic values only for recognized, declared, unfilled fields; no LLM calls |
| `workflow-run-simulator.go` | Traverses edges breadth-first from sorted start nodes; emits explicitly simulated node states and leaves unreachable nodes without state |

`WireDevelopmentFixtures` is the composition point: it layers the existing Directory, sets Context/Dispatcher/Assist/Simulator and preserves real Forms. The server calls it only with explicit `collaboration.development_fixtures`; `cmd/ora-web` and tests opt in. External login configuration never enables fixtures implicitly.

Fixture UUIDs are stable for repeated demos; trace IDs and timestamps are not fixed. `wire_test.go` covers port composition and production defaults. See [Issue collaboration](../../docs/collaboration/interactions.en.md) and [Workflows](../../docs/workflows/workflows.en.md).
