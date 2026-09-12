# cmd/cloudctl: 运维与部署管理 CLI

[中文](README.md) | [English](README.en.md)

`cloudctl` 是 Ora Cloud 受限的运维管理工具。它在公共 API 调用路径之外运行，使用数据库操作员凭据管理数据库迁移、初始租户及其管理员的引导配置，以及基础设施机密信息引用的注册。

## 命令与职责

### `migrate`
```sh
cloudctl -config <path> -command migrate
```
- 严格按照数字序号递增顺序，依次执行 `internal/core/migrations` 中所有尚未应用的前向迁移（forward migrations）。
- 计算迁移脚本的 SHA256 校验和（checksum），并记录到 `schema_migrations` 表中。
- 在数据库事务级咨询锁（`pg_advisory_xact_lock(67420911)`）保护下执行，确保并发或重复执行时具备确定性与安全性。
- 如果已应用迁移的校验和不匹配，则报错并拒绝执行。

### `bootstrap`
```sh
cloudctl -config <path> -command bootstrap -name '<tenant-name>' -source '<idp-source>' -subject '<idp-subject>' -display-name '<display-name>'
```
- 在单个数据库事务中原子化地创建一个处于活动状态的初始租户及其第一个管理员用户。
- 将外部 IdP（身份提供商）的身份断言（`source` 与 `subject`）绑定至内部用户记录。
- 在 `tenant_memberships` 表中为该用户授予 `admin` 角色。
- 返回包含 `tenantId` 和 `userId` 的 JSON 数据载荷。

### `credential-ref`
```sh
cloudctl -config <path> -command credential-ref -tenant '<tenant-uuid>' -owner '<user-uuid>' -secret-ref 'infra-secret://git/team/account'
```
- 为 Git 操作注册基础设施凭据引用。
- 强制执行外键约束：目标用户必须是指定租户的活动成员。
- **安全不变量**：只存储 URI/引用字符串（`secret-ref`）；从不接收、记录或存储明文密码、token 或私钥。

## 边界与不变量

- **部署边界**：`cloudctl` 不通过 HTTP 暴露，也不由 server 守护进程调用。它必须能够直接连接 PostgreSQL，并拥有 Schema 迁移权限。
- **结构化输出**：所有命令执行结果均以结构化 JSON 格式输出到 `stdout`，错误信息输出到 `stderr`，便于与 CI/CD 自动化部署流水线集成。

参见 [cmd 入口总览](../README.md)、[数据库迁移目录](../../internal/core/migrations/README.md) 与 [认证配置与凭据](../../docs/authentication.md)。
