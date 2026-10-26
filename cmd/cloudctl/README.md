# cmd/cloudctl: 运维与部署管理 CLI

[中文](README.md) | [English](README.en.md)

`cloudctl` 是 Ora Cloud 受限的运维管理工具。它在公共 API 调用路径之外运行，使用数据库操作员凭据管理数据库迁移、初始租户和管理员的引导配置，以及残留资源的显式处置。

## 命令与职责

### `migrate`
```sh
cloudctl -config <path> -command migrate
```
- 按完整文件名字典序，依次执行 `internal/core/migrations` 中所有尚未应用的前向迁移（forward migrations）。
- 计算迁移脚本的 SHA256 校验和（checksum），并记录到 `schema_migrations` 表中。
- 在数据库事务级咨询锁（`pg_advisory_xact_lock(67420911)`）保护下执行，确保并发或重复执行时具备确定性与安全性。
- 如果已应用迁移的校验和不匹配，则报错并拒绝执行。

### `bootstrap`
```sh
cloudctl -config <path> -command bootstrap -name '<tenant-name>' -source '<idp-source>' -subject '<idp-subject>' -display-name '<display-name>'
```
- 在单个数据库事务中原子化地创建一个处于活动状态的初始租户及其第一个管理员用户。
- 将外部 IdP（身份提供商）的身份断言（`source` 与 `subject`）绑定至内部用户记录。
- **注意（华为 IDaaS）**：当 `-source` 为 `huawei-corp` 时，`-subject` 必须是 IDaaS 返回的内部 `uuid`（形如 `uuid~...`），**绝不能填写员工工号或 W3 账号**，否则用户通过 IDaaS 登录时将因 subject 不匹配而生成未授权的新用户。
- 在 `tenant_memberships` 表中为该用户授予 `admin` 角色。
- 返回包含 `tenantId` 和 `userId` 的 JSON 数据载荷。

Git 凭据的提交与引用管理见 [外部 Git 线](../../docs/repositories/external-git.md)。

### `residual`

全局 `-config` 必须放在 `residual` 前，动作参数放在动作后。所有动作都必须提供 `--tenant-id`。查询及业务命令不需要 DDL 权限；受控主机和数据库凭据决定谁能使用这个入口。core 取 PostgreSQL `session_user` 作为认证主体；可选 `--operator-label` 只是人工自述，不能选择或覆盖认证主体。CLI 不接收数据库密码参数，沿用现有配置加载方式。

```sh
cloudctl -config <path> residual list --tenant-id <tenant> --project-id <project> --limit 100
cloudctl -config <path> residual get --tenant-id <tenant> --workspace-id <workspace>
cloudctl -config <path> residual get --tenant-id <tenant> --intent-id <intent>
```

- `list` 支持 `--project-id`、`--run-id`、`--workspace-id`、`--operation-id`、`--reclaim-id` 相交过滤，以及 `--limit`（默认 100）和 `--after`。返回 `items/nextCursor`；operation 过滤可命中 D8 原 failed delete 或当前绑定的删除操作。
- `get` 接受一个根/从属 `--intent-id`，或精确的 `--run-id`、`--workspace-id`、`--reclaim-id`。原资源选择器可用其他资源 ID 进一步限制范围；意图选择器不能混用资源选择器。查询返回审计、目标证据和 `openTargets`，供外部确认引用。

`list` 的每个条目、`get` 的实际 residual 账本行，以及存在时的资源输出都通过 `stopEvidence` 展示持久化的 `stop_evidence`；资源仅在未选择意图时返回。没有证据时输出空对象 `{}`；没有账本的历史 D8 列表条目也输出 `{}`，但 `get` 中合成的历史 D8 residual 可能省略该字段。已有停止未确认记录会保留 `kind=stop_unconfirmed`、`source=cloud`、`observedAt` 和原身份的 `executionId`。此字段仅供查看，不授予终止、删除或清账权限，也不改变回收责任。

终止和删数据是两次独立命令，各自需要 reason、幂等 key 和真实确认。`terminate` 至少指定 Run 或 Workspace；提供的其他 ID 必须属于同一个原资源。D8 有历史删除操作时必须提供原 failed delete 的 `--operation-id`。Project/tenant 不能作为批量写入范围。

对已按 D8 达到 `done/workspace_unavailable`、却从未创建任何删除操作的残留，必须额外显式选择 `--allow-missing-delete-operation`，同时提供精确的 `--run-id` 和 `--workspace-id`，且不提供 `--operation-id`。此选择不能用于仍在运行、没有交付结论、有删除历史、admission 开放或已有残留账本的资源。意图记录 `legacyDeleteOperationMissing=true`，不补造历史 operation；默认命令仍拒绝这一变体。后续终止、围栏和单独删数据的证据要求相同。

```sh
cloudctl -config <path> residual terminate --tenant-id <tenant> --run-id <run> --workspace-id <workspace> --reason 'operator abandoned execution' --key <terminate-key> --impact-confirmed
cloudctl -config <path> residual terminate --tenant-id <tenant> --run-id <run> --workspace-id <workspace> --allow-missing-delete-operation --reason 'verified recovery copies retained' --key <missing-delete-key> --impact-confirmed
cloudctl -config <path> residual delete-data --tenant-id <tenant> --intent-id <root-intent> --reason 'delete retained data' --key <new-delete-key> --data-delete-confirmed
```

`terminate` 登记终止授权，默认保留数据；命令受理不代表沙盒已经终止。只有每个目标都有真实终止及迟到 ensure 已被封闭的双确认，根意图才可成功并报告 `terminated_data_retained`。后续 `delete-data` 只接受这个已完成的根意图，单独登记从属意图和唯一删除操作；实际删除确认后才能报告 `released`。它不接受裸 Workspace 删除，也不以外部确认代替真实终止证据。

无法由系统确认的未完成意图可通过下列命令显式转移剩余责任。先从 `get` 读取 `openTargets`，为每个未完成目标各提供一次 `--external-fact`；键必须是原样的 `sandbox:<id>` 或 `data:<workspace-id>`，值是工单或其他外部处置事实引用。重复目标、空引用、空说明及未确认责任转移会被拒绝；core 继续核对是否遗漏目标。

```sh
cloudctl -config <path> residual confirm-external --tenant-id <tenant> --intent-id <unfinished-intent> --reason 'external disposal' --key <external-key> --evidence-note 'host decommissioned; data responsibility accepted' --external-fact 'sandbox:<id>=ticket-123' --external-fact 'data:<workspace-id>=ticket-456' --responsibility-transferred
```

外部确认只接受已存在且未完成的意图，保存独立的 `externally_confirmed` 处置，不制造 `terminated`、`lateEnsureFenced` 或 `removed` 物理证据，不授权同卷复用。幂等 key 在租户内跨写动作唯一：相同输入返回同一责任的当前进度；同键改动作、目标、reason 或确认返回 `idempotency_conflict`。

## 边界与不变量

- **部署边界**：`cloudctl` 不通过 HTTP 暴露，也不由 server 守护进程调用。它必须能够直接连接 PostgreSQL；只有迁移命令需要 Schema 迁移权限。残留管理不新增 tenant admin/member 的公共权限，隐藏 Run Workspace 的公开 HTTP 入口继续拒绝访问。
- **结构化输出**：所有命令执行结果均以结构化 JSON 格式输出到 `stdout`，错误信息输出到 `stderr`，便于与 CI/CD 自动化部署流水线集成。
- **错误与凭据**：残留命令保留 core 的稳定 Fault code；未知基础设施错误仅输出 `internal_error`，配置或连接失败输出安全的通用 code，不输出连接串或密码。参数错误为 `invalid_cli_arguments`；`--impact-confirmed=false` 等同于未确认。

参见 [cmd 入口总览](../README.md)、[数据库迁移目录](../../internal/core/migrations/README.md) 与 [内部凭据与服务信任](../../docs/identity/authentication.md)。
