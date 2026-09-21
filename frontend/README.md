# frontend: Ora Cloud Web 前端

[中文](README.md) | [English](README.en.md)

React 19 + TypeScript + Vite 8 + Tailwind CSS 4（shadcn/ui 组件）。API 层不手写：`src/api/` 由 [orval](https://orval.dev) 从后端生成的 [`api/openapi.json`](../api/openapi.json) 编译而来，产出带类型的 [TanStack Query](https://tanstack.com/query) hooks。

## 目录

| 路径 | 说明 |
| --- | --- |
| `src/api/<tag>/<tag>.ts` | **生成物**，按 OpenAPI tag（`me`、`projects`、`workspaces`、`internal` …）分目录，每个 operation 一组 `useXxx` / `getXxxQueryKey` / `getXxxQueryOptions`；禁止手改 |
| `src/api/generated.schemas.ts` | **生成物**，全部请求/响应/参数 TypeScript 类型；禁止手改 |
| `src/api/index.ts` | **生成物**，re-export 所有 tag 目录 |
| `src/lib/api-client.ts` | 所有生成 hook 共用的 axios 实例（`AXIOS_INSTANCE`）与 mutator；浏览器只携带 HttpOnly 会话 Cookie，不保存或注入 token |
| `orval.config.ts` | 生成配置：输入 `../api/openapi.json`，`client: 'react-query'`，`clean: true` |
| `vite.config.ts` | `@` → `src` 别名；dev 代理 `/auth`、`/api`、`/healthz` 到 Gateway `http://localhost:8081`；vitest 与覆盖率阈值 |
| `scripts/` | 门禁脚本：强制模块 README、测试与导出符号文档，见 [`scripts/README.md`](scripts/README.md) |
| `AGENTS.md` | 本目录的工程规则（内聚、体量上限、文档、测试）；`CLAUDE.md` 引用它 |

每个含手写源码的目录都是一个模块，各自带 `README.md` / `README.en.md`；从 [`src/README.md`](src/README.md) 开始读。

## 命令

```sh
npm ci                  # 安装（CI 同款）；Node >= 24，见 .node-version
npm run dev             # Vite dev server，默认 http://localhost:5173
npm run api:generate    # 从 ../api/openapi.json 重新生成 src/api
npm run format          # prettier --write（只管 ts/tsx/css/html，不动 Markdown 和 JSON）
npm run format:check    # prettier --check
npm run lint            # oxlint，type-aware，warning 视为错误
npm run typecheck       # tsc -b（strict、noUncheckedIndexedAccess、exactOptionalPropertyTypes …）
npm run test            # vitest + 覆盖率阈值；npm run test:watch 进入 watch 模式
npm run check:modules   # 每个模块都有 README.md + README.en.md 和测试
npm run check:modules -- --base origin/main   # 改了实现的模块也改了文档和测试
npm run check:docs      # 每个导出符号都有有信息量的 JSDoc
npm run check:dead      # knip：未使用的文件、导出、依赖
npm run check:dup       # jscpd：重复代码
npm run build           # tsc -b && vite build
npm run check           # 以上全部（不含 --base 差异检查），顺序与 CI 一致
```

仓库根目录的 Task 封装：

- `task frontend:install`：`npm ci`。
- `task frontend:dev`：只起 Vite dev server（需要另起 Cloud `task run` 与 Gateway `task run:gateway`）。
- `task dev`：同时启动 Cloud（:8080）、认证 Gateway（:8081）和 Vite（:5173），日常开发的统一入口。
- `task frontend:generate`：先 `task openapi`（Go 契约 → `api/openapi.json`），再 `npm run api:generate`。改了后端接口就跑这个，并把 `api/openapi.json` 和 `frontend/src/api` 一起提交。
- `task frontend:format` / `task frontend:test`：即 `npm run format` / `npm run test`。
- `task frontend:check`：与 CI `frontend` job 相同的门禁：重新生成并检测 `frontend/src/api` 漂移，再跑 `npm run check`。
- `task frontend:check:diff`（可加 `BASE=<ref>`，默认 `origin/main`）：PR 检查——改了实现的模块必须同时改 README 和测试。

## 不变量

- **前后端契约只有一个来源**：`internal/contract` → `api/openapi.json` → `src/api`。任何一环有未提交的漂移，Go 测试（`TestPublishedOpenAPIIsValidAndCurrent`）或 CI `frontend` job 会失败。
- **不要在生成目录里放手写文件**：`clean: true` 会在每次生成前清空 `src/api/`。共用代码放 `src/lib/`。
- **生成物是确定性的**：同一份 `openapi.json` 多次生成得到逐字节相同的输出，这是漂移检测成立的前提。
- **文档和测试随代码走**：每个模块目录有中英双语 README 和测试，每个导出符号有 JSDoc；PR 里改了某模块的实现就必须同时改它的 README 和测试（或在 commit 里写明理由的 `Docs-Unchanged:` / `Tests-Unchanged:` trailer）。完整规则见 [`AGENTS.md`](AGENTS.md)。

## 本地联调

后端需要真实 PostgreSQL 与已登记的 Gateway 密钥，按根目录 [README](../README.md) 起库、迁移并配置 `configs/gateway.yaml`。运行 `task dev` 后只打开 `http://localhost:5173`；该 origin 同源代理登录与 API 请求，配置中的 `public.base_url` 也必须是这个浏览器可见 origin。生产部署同样必须让前端与 Gateway 对浏览器表现为同一公开 origin。
