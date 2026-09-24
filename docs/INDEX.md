# Ora Cloud — Documentation Index

Entry point for anyone (human or agent) picking up this codebase. Two reading paths:

| I am… | Start here | Then |
| --- | --- | --- |
| **An AI agent** continuing development | [`development/agent/architecture.md`](development/agent/architecture.md) | [`development/agent/adding-features.md`](development/agent/adding-features.md) when adding an endpoint |
| **A new teammate** getting oriented | [`development/onboarding/overview.md`](development/onboarding/overview.md) | [`development/onboarding/progress.md`](development/onboarding/progress.md) for what's done vs. pending |

## Doc map

```
docs/
  INDEX.md                      ← you are here (map + pointers)
  plugins.md                    ← 插件市场:选择状态权威、catalog 快照、同步、effect 链路

  development/
    agent/                      ← for AI agents: dense, precise, convention-focused
      architecture.md           ← request path, core conventions, invariants, traps
      api-reference.md          ← every endpoint (public + internal), fields, error codes
      database.md               ← all tables & migrations, add/alter rules
      adding-features.md        ← how-to: add an endpoint / sub-resource safely
    onboarding/                 ← for new teammates: readable, tables & diagrams
      overview.md               ← what Ora Cloud is, the moving parts, glossary
      progress.md               ← what's built / in-progress / deferred (status board)
  migrations/
    multica-issue-board/        ← single-migration archive (analysis → design → test → final)
    workspace-integration-stage-a.md ← Stage A：origin/main → workspace合并 上游对齐融合决策记录（含迁移重编号 + 前端验证）
    workspace-integration-stage-b.md ← Stage B：Collaboration Space 集成 + D1–D7 ADR（决策已实施）
    workspace-integration-stage-d-coworker-login-workspace.md ← Stage D：同事 Login + Workspace UX 恢复 + SD1–SD7（COMPLETE）
    workspace-integration-stage-d-report.md ← Stage D 最终报告（§38：恢复内容、作用域矩阵、结论）
    user-registration.md              ← User Registration：创建 User Identity（SD1–SD5，IMPLEMENTED）
    workspace-membership.md           ← Workspace Add Member：email 添加已注册用户（SD1–SD6，IMPLEMENTED；SD6 已被取代）
    workspace-sharing-model.md        ← Workspace Sharing Model：Workspace=资源共享边界 + 统一删除规则（SS1–SS5，IMPLEMENTED；PS 已落地）
    project-workspace-sharing.md      ← Project Workspace Sharing：Project/Runtime 访问切换为 workspace-shared（PS1–PS7，IMPLEMENTED）
    workspace-member-management.md    ← Workspace Member Management & Onboarding：0-Workspace onboarding + 成员角色 owner-only + owner immutable + 移除 owner-only + Project delete UI 对齐（MM1–MM9，IMPLEMENTED）
  acceptance.md                 ← product acceptance criteria (pre-existing)
  authentication.md             ← auth model (pre-existing)
  core-contract.md              ← core behavioural contract (pre-existing)
  execution-contract.md         ← execution/operations contract (pre-existing)
```

**Integration stages.** **Stage A** = upstream main alignment (`origin/main` → `workspace合并`). **Stage B** = Collaboration Space integration + the D1–D7 ADR. **Stage C** = final integration review (a review phase; its record is not yet a repo document). **Stage D** = coworker Login + Workspace UX restoration (SD1–SD7), a product correction that restores the coworker's product shell on top of the merged Space backend — **COMPLETE** (commits `c4a7510`…`c570b9e`, branch `workspace合并`; `main` stays `1c5b9b4`, not merged/pushed at the time). **922GithubAuth ← main（2026-09-23）** = feature 分支 `922GithubAuth` 合并 main 的 gateway-会话架构（`SessionProvider`/`/w/:slug`/`/onboarding`），删除 feature zustand 商店，保留 Step 3A 前端能力；开发拓扑双入口（ora-web :8080 + gateway :8081）；项目模型混合 optional-space。**已合并提交 `9e887d4`，待人工测试验收**。

## Source of truth (one doc per topic)

| Topic | Where | Notes |
| --- | --- | --- |
| **Project status / roadmap** | [progress.md](development/onboarding/progress.md) | done / in-progress / planned / deferred / blocked |
| **Issue architecture** | [agent/architecture.md](development/agent/architecture.md) + [12-collaboration-architecture.md](migrations/multica-issue-board/12-collaboration-architecture.md) | the latter is the frozen Wave-3 design (rev. 2, plus §36/§37 revisions) |
| **API** | [agent/api-reference.md](development/agent/api-reference.md) | live endpoint/field reference; `api/openapi.json` is the machine truth |
| **Database** | [agent/database.md](development/agent/database.md) | live table + migration inventory |
| **Collaboration interaction model** | [12-collaboration-architecture.md §37](migrations/multica-issue-board/12-collaboration-architecture.md#37-wave-3b-0--collaboration-interaction-model-frozen) | **authoritative product semantics** — `@`, the four target modes, cardinality, context, timeline, fixtures. Frozen by Wave 3B-0 |
| **Collaboration / integration ports** | [12-collaboration-architecture.md §6.4](migrations/multica-issue-board/12-collaboration-architecture.md#64-canonical-port-inventory-unified-by-wave-3b-0) | the **single** canonical port inventory (15 ports; `FormDescriptorProvider` added by 3B-2). Other docs must point here, not repeat a list |
| **Workflow interaction (Issues-facing)** | [12-collaboration-architecture.md §38](migrations/multica-issue-board/12-collaboration-architecture.md#38-wave-3b-2--workflow-interaction-design-frozen) | the **contract** (`FormDescriptor`, single Confirm boundary, AI Assist authority, draft decision, API surface, `0010`) + **§38.37** implementation record |
| **Workspace integration (Stage A)** | [workspace-integration-stage-a.md](migrations/workspace-integration-stage-a.md) | upstream main alignment: origin/main → `workspace合并` fusion, migration renumbering (0005→0006…), dual-JWT contract, frontend resolutions, decisions D-Auth / D4 |
| **Workspace integration (Stage B)** | [workspace-integration-stage-b.md](migrations/workspace-integration-stage-b.md) | Collaboration Space integration into the merged tree + ADR: decisions D1–D7 (owner authorization, optional Space association, terminology, routing, session, isolation, default-Space protection) |
| **Workspace integration (Stage D)** | [workspace-integration-stage-d-coworker-login-workspace.md](migrations/workspace-integration-stage-d-coworker-login-workspace.md) | coworker Login + Workspace UX restoration + ADR: SD1–SD7 (product shell, ora-web cookie session, two-plane login, tenant-scoped Issues, space-scoped Projects, switch semantics, demo-mode gating) — **COMPLETE** |
| **User Registration** | [user-registration.md](migrations/user-registration.md) | create a User Identity (name + email) at the ora-web boundary → session → current-user flow. ADR: SD1–SD5 (ora-web `POST /auth/register`, strict-create store, email trim+lowercase case-insensitive uniqueness, register mode in the existing Login page) — **IMPLEMENTED**; AUTHENTICATION not fully implemented, WORKSPACE ADD MEMBER / PROJECT SHARING implemented (next two rows) |
| **Workspace Add Member** | [workspace-membership.md](migrations/workspace-membership.md) | owner/admin adds an already-registered user to a Collaboration Space by email (`POST /spaces/:sid/members`); atomic tenant+space enrollment in one tx, fixed `member` role, idempotent existing-member return, 404 `user_not_registered` for unknown email. ADR: SD1–SD6 (enrollment endpoint, atomic dual-write, fixed role, dispatch/routing, email dialog frontend, **Workspace Membership ≠ Project Access** — SD6 superseded, see next row) — **IMPLEMENTED**; PROJECT SHARING implemented (Step 3, next row); EMAIL INVITATION not implemented |
| **Workspace Sharing Model** | [workspace-sharing-model.md](migrations/workspace-sharing-model.md) | **Workspace = resource-sharing boundary**: a Workspace member may access the resources shared inside that Workspace (Projects/Issues/Agents/Teams/Workflows/MCPs/Skills); per-resource membership (`project_members`/…) NOT used; unified delete rule `CanDeleteWorkspaceResource = creator OR workspace owner/admin`; minimal predicates `IsWorkspaceMember`/`IsWorkspaceAdmin`/`CanDeleteWorkspaceResource`. ADR: SS1–SS5. **Supersedes the old D1 owner-isolation product rule**. — **IMPLEMENTED**; Project Workspace Scoping **IMPLEMENTED (Step 3)**; Issue/Agent/Team/Workflow/MCP/Skill Workspace Scoping NOT IMPLEMENTED |
| **Project Workspace Sharing** | [project-workspace-sharing.md](migrations/project-workspace-sharing.md) | **Project = workspace-shared resource**: `CanAccessProject(user,P) = P.space_id = W AND user active member of W`; Project detail/runtime Workspace/list access switched from owner-only to workspace membership; delete rule `project creator OR Workspace owner/admin` (member-denial 403, non-member 404); legacy `space_id=NULL` stays owner-only; Runtime Workspaces inherit the parent Project's access. ADR: PS1–PS7. — **IMPLEMENTED (Step 3)**; Issue/Agent/Team/Workflow/MCP/Skill Workspace Scoping NOT IMPLEMENTED |
| **How to add code** | [agent/adding-features.md](development/agent/adding-features.md) | endpoint / sub-resource how-to + verify |

## Repo layout (one glance)

| Path | What lives there |
| --- | --- |
| `cmd/` | Entry points: `server`, `cloudctl`, `openapi`, `simulator`, demos. |
| `internal/api/router/` | HTTP layer — route allowlist + dual-JWT auth + strict JSON. |
| `internal/core/` | Business logic — `Store.Public`/`Control`, raw SQL in advisory-locked tx, migrations. |
| `internal/contract/` | OpenAPI generator (`openapi.go`) → `api/openapi.json`. |
| `internal/simulator/` | Test/dev substrate (in-memory node + credential signing). |
| `integration/` | End-to-end tests against real PostgreSQL (isolated schema per test). |
| `api/openapi.json` | **Generated** — do not hand-edit; run `go run ./cmd/openapi`. |
| `scripts/` | Dev/demo shell wrappers. |

## Current status (see [progress.md](development/onboarding/progress.md) for detail)

- ✅ Core platform: tenants, memberships, identity, projects, workspaces, operations.
- ✅ **Issue Board** — wave 1 (core Kanban), wave 2 (status catalog, comments, labels, subscribers,
  numbers, properties, search, batch, saved views, groups) and **Wave 3A** (collaboration
  foundation: polymorphic assignee, comment author actors, IssueRun, timeline projection, context
  refs). Migrations 0006–0010 (renumbered from 0005–0009 in the workspace integration — see
  [workspace-integration-stage-a.md](migrations/workspace-integration-stage-a.md)); the formal React frontend in
  `frontend/` is migrated and speaks to the real API.
- ✅ **Wave 3B-0** — Collaboration Architecture Alignment (docs only): the interaction model is frozen
  in [§37](migrations/multica-issue-board/12-collaboration-architecture.md#37-wave-3b-0--collaboration-interaction-model-frozen)
  and the 14 integration ports are unified in
  [§6.4](migrations/multica-issue-board/12-collaboration-architecture.md#64-canonical-port-inventory-unified-by-wave-3b-0).
- ✅ **Wave 3B-1** — Collaboration Interaction Foundation (migration `0009`, formerly `0008`): the first real end-to-end
  `@` collaboration chain — `GET /collaboration/targets` → `@` picker → Human Mention / Agent & Team
  Task → deterministic context → mock execution → IssueRun lifecycle / Activity / reply Comment →
  `GET /issues/{iid}/timeline` — with no real Agent/Team/Workflow/Runtime modules.
- ✅ **Wave 3B-2 — Workflow Interaction Shell** (migration `0010`, formerly `0009`): `@Workflow` → `FormDescriptor` →
  dynamic form → optional AI Assist → **Review → Confirm** → `IssueRun` → mock execution → Timeline.
  New ports `FormDescriptorProvider` / `InputAssistProvider`; workflow output lands as a `system`
  activity. Real Workflow / AI providers remain **blocked on external design**
  ([§38.37](migrations/multica-issue-board/12-collaboration-architecture.md#3837-implementation-record-2026-09-20--implemented--verified)).
  Dev fixture targets are served by `cmd/server` only when `collaboration.development_fixtures` is
  explicitly on (env `CLOUD_COLLABORATION_DEVELOPMENT_FIXTURES=true`), **production default OFF**,
  independent of auth; see
  [architecture.md](development/agent/architecture.md#collaboration--implemented-vs-planned).
- ✅ **Workspace integration Stage D** — coworker Login + Workspace UX restoration (SD1–SD7): ora-web
  cookie-session 双 tab 登录（真实 + 演示）、Current Workspace shell、左上工作区选择器/创建/切换、
  space 级 Projects/成员/设置 + tenant 级 Issues 挂回外壳；记录 **WORKSPACE SCOPING GAP**（后端
  Issues 无 `space_id`，不伪造）。`main` 保持 `1c5b9b4`，未 merge/push（见
  [stage-d](migrations/workspace-integration-stage-d-coworker-login-workspace.md)）。
- ✅ **User Registration** — create a User Identity (name + email) through the ora-web edge
  (`POST /auth/register`, SD1–SD5): strict-create store semantics (no new migration, reuses
  `users`/`user_identities`/`tenant_memberships`), email trim+lowercase with case-insensitive
  uniqueness (`Alice@Example.com` == `alice@example.com`), stable `409 user_already_exists`,
  and register mode in the existing Login page → new user enters the current-user flow directly.
  Registration is **account creation only** — AUTHENTICATION not fully implemented,
  PROJECT SHARING not implemented. **922GithubAuth 合并 main 后**：前端 Login 页改为 gateway
  provider-button 形态（`SessionProvider`），**register 模式从前端移除**；`POST /auth/register` 仅保留为
  ora-web 开发边界 API（见 [user-registration.md](migrations/user-registration.md) 合并注记）。
- ✅ **Workspace Add Member** — owner/admin adds an already-registered user to a Collaboration Space by
  email (`POST /spaces/:sid/members`, SD1–SD6): atomic tenant+space enrollment in one tx (no
  half-state), fixed `member` role, idempotent existing-member return, 404 `user_not_registered` for
  unknown/inactive email, business-level role enforcement (owner/admin 200, member 403). The new member
  sees and can switch to the Workspace after refresh. PROJECT SHARING was implemented in Step 3 (see the
  next rows); EMAIL INVITATION remains not implemented (see
  [workspace-membership.md](migrations/workspace-membership.md)).
- ✅ **Workspace Sharing Model (Step 2B)** — alignment, not resource migration. Workspace is now the
  **resource-sharing boundary** (member may access the Workspace's shared resources); per-resource
  membership is **not used**; the unified delete rule is **creator OR workspace owner/admin**, backed by
  minimal predicates `IsWorkspaceMember`/`IsWorkspaceAdmin`/`CanDeleteWorkspaceResource`. The old D1
  owner-isolation product rule is **superseded** (see
  [workspace-sharing-model.md](migrations/workspace-sharing-model.md)).
- ✅ **Project Workspace Sharing (Step 3)** — the first real resource migration: Project detail, space
  project list, and Runtime Workspaces switch from **owner-only** to **workspace-shared**
  (`CanAccessProject = P.space_id = W AND user active member of W`); delete rule **project creator OR
  Workspace owner/admin** (member-denial 403, non-member 404 existence-hidden); legacy `space_id=NULL`
  Projects stay **owner-only** (no auto-backfill, no scope widening); `owner_user_id` continues as
  creator; **no** `project_members`; frontend zero code change. (see
  [project-workspace-sharing.md](migrations/project-workspace-sharing.md)).
- ✅ **Workspace Member Management & Onboarding (Step 3A)** — Workspace foundation 收口: new registered
  users start with **0 workspaces (a legal state)** and see an onboarding empty state with a Create
  Workspace CTA (reusing `POST /spaces`; the creator becomes owner) plus "ask a workspace owner to add
  you"; role changes are **owner-only** (`PUT /members/:uid`), the **owner role is immutable** through
  the member API (any ownership transfer → 409 `ownership_transfer_not_supported`), and removal is an
  **owner-only hard delete** (`DELETE /members/:uid`, 409 `cannot_remove_workspace_owner`) that removes
  workspace membership alone — the user account, tenant membership, and created resources remain.
  Project delete UI now matches the backend (`creator OR owner/admin`). **922GithubAuth 合并 main 后**
  onboarding 为独立路由 `/onboarding`（`OnboardingPage`，`useCreateTenant`/`useCreateSpace`），当前用户
  身份来自 `useSession()`（`useAuthStore` 已删除），Step 3A 前端能力保留（见
  [workspace-member-management.md](migrations/workspace-member-management.md)）。
- ✅ **922GithubAuth ← main 会话架构合入** — feature 分支合并 main 的 gateway-会话架构（已合并提交
  `9e887d4`，待人工测试验收）：`SessionProvider`/`useSession`/`RequireSession` + `/onboarding` + `/w/:workspaceSlug` 路由；
  删除 feature 的 zustand 商店；保留 Step 3A 前端能力（members owner-immutable、`canDeleteProject`、
  issues 接真实后端）。开发拓扑 = **双入口**（ora-web :8080 DEV-only 本地演示 + gateway :8081 生产
  规范）；项目模型 = **混合/optional space**（`space_id` 可空 + 默认空间，保留 `0012`）。
- 🧭 Planned — **Wave 3C** (Issue Detail & Collaboration UI). Real Agent/Team/Workflow modules are
  **blocked on external design**.
- ⏸️ Deferred: attachments, issue↔project binding, PR links, realtime, bots/squads, Autopilot.

## ⚠️ Specs governance

The ADR-first rule requires an approved ADR before coding; `specs/` is maintained in the separate
`ora-space/specs.git` repository (see `AGENTS.md`). The Cloud ADRs referenced by the migration docs below
live under `specs/decisions/cloud/`; their publication to the upstream specs repo is an open governance
decision.

## Golden rules (both audiences)

1. **Additive first** — new files/routes/migrations; never refactor core to add a feature.
2. **Never edit an applied migration** — add a new `NNNN_*.sql`; alter tables there.
3. **`api/openapi.json` is generated** — after changing `internal/contract/openapi.go`, run
   `go run ./cmd/openapi` or the contract test fails.
4. **Verify before claiming done** — `go build ./...`, `go test ./internal/... ./cmd/...`, and the
   integration suite against Docker PostgreSQL (see [adding-features.md](development/agent/adding-features.md) §Verify).
