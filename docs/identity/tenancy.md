# 用户、租户与成员

[English](tenancy.en.md) | 中文

租户（tenant）是 Cloud 里一切资源的隔离边界。每个租户恰有一个对用户可见的**协作空间**（Space，表名
`collab_workspaces`），用户在界面上看到和切换的就是这个空间。租户成员身份（`tenant_memberships`）是
唯一的角色与授权来源：一个人能看到哪些 Project、能做哪些管理操作，都由他在该租户里的当前成员状态决定。

> "Workspace"一词在 Cloud 里有两种含义：协作空间（`collab_workspaces`，即本文的 Space）和运行时 Workspace
> （`workspaces`，一个 Project 下独立的运行环境，见 [Workspace 生命周期](../runtime/workspaces.md)）。两者是
> 不同的聚合，文档里分别称为 Space 和 Workspace。

## 模型

```text
users ──< user_identities          一个用户可以有多条外部身份映射
  │
  └──< tenant_memberships >── tenants ── collab_workspaces（恰好一个）
         role: admin | member              │
         status: active | disabled         └──< projects ──< workspaces
```

- **用户**由 [身份解析](authentication.md#从凭据到用户) 得到。用户可以属于多个租户，并在它们之间切换。
- **租户与 Space 一一对应。** Space 有全局唯一、不可修改、不可复用的 `slug`；修改 Space 名称会同时修改
  租户名称。
- **角色只有 `admin` 和 `member`**，管理员之间平权。成员状态为 `active` 或 `disabled`；没有物理删除成员的
  接口，停用保留所有历史引用。
- **当前成员**指成员行为 `active`、用户为 `active` 且未删除、租户为 `active` 且未删除，三者同时成立。所有
  租户内的请求都先做这项检查，失败返回 `403 membership_required`；需要管理员的操作再检查角色，失败返回
  `403 admin_required`。

## 创建租户

租户总是和它的首位管理员、唯一的 Space 在同一个事务里创建，所以不存在没有管理员或没有 Space 的租户。

| 方式 | 入口 | 说明 |
|---|---|---|
| 自助创建 | `POST /api/v1/tenants`，请求体 `name` 与 `slug` | 任何已登录用户都可以创建，调用者成为管理员。这是唯一不需要租户作用域的写请求：已验证的身份就是全部授权。幂等记录按用户匹配（见 [幂等记录](../core/persistence.md#一致性手段)） |
| 部署引导 | `cloudctl -command bootstrap` | 部署操作，为指定身份创建租户和管理员，Space 使用生成的全局唯一 slug；重复执行会再建一个新租户 |

`slug` 只能是小写字母、数字和连字符，以字母或数字开头，最长 64 个字符；与已有 Space 冲突时返回
`409 space_slug_conflict`。

首次登录、还没有任何租户的用户，前端会引导他创建自己的第一个空间。`GET /api/v1/me/spaces` 返回用户的全部
活动空间，用于切换。

## 加入租户

部署分两种，加入方式不同，由是否配置了企业员工目录（`directory.endpoint`）决定：

**公网部署（未配置目录）：私有链接。** 管理员创建两类链接，令牌由浏览器生成 32 个随机字节，只放在请求体里
传给 Cloud，数据库只保存 SHA-256 摘要，分享 URL 由创建者在本地拼出：

| 链接 | 有效期 | 效果 |
|---|---|---|
| 邀请（`/tenants/:tid/invitations`） | 7 天，**单次** | 被邀请人兑换（`POST /api/v1/join/invitations/redeem`）后直接成为普通成员 |
| 申请链接（`/tenants/:tid/join-links`） | 30 天，可重复使用 | 访问者提交申请（`POST /api/v1/join/requests`），管理员批准（`.../join-requests/:rid/approve`）后成为普通成员，或拒绝 |

- 链接可以撤销（带版本的 `DELETE`）。已过期、已撤销、已兑换的令牌一律返回 `404 join_link_unavailable`。
- 已是成员的人再提交申请返回 `409 already_member`；同一人的待处理申请只保留一条。
- 兑换和申请发生在成为成员之前，幂等记录按用户保存（`join_idempotency_records`）。用户可以通过
  `GET /api/v1/me/join-requests` 查看自己的申请。
- 没有公开的空间目录，不能搜索或浏览别人的空间。

**企业部署（配置了目录）：员工目录核验。** 上面三组私有链接接口全部返回 `404`，加入只能经过目录：

1. 管理员通过 `GET /api/v1/tenants/:tid/people?keyword=…` 在员工目录里搜索。Cloud 在服务端调用目录，浏览器
   不持有目录的机器凭据；关键字 2 到 100 个字符，只返回在职人员，最多 20 条。
2. 管理员通过 `POST /api/v1/tenants/:tid/members/huawei` 添加。Cloud 用同一个关键字**重新查询**目录，确认所选
   人员仍然在职，再以 `globalUserId` 建立成员关系；角色可选 `admin` 或 `member`。
3. 被添加的人第一次用 IDaaS 登录时，经 `globalUserId` 自动关联到这个预先建立的用户（见
   [身份解析](authentication.md#从凭据到用户)）。

调用目录之前和写入之前各校验一次管理员身份，因为两次之间隔着一次外部调用。工号只用于展示，不参与授权或
身份匹配。目录不可用时返回 `503 directory_unavailable`。

## 管理成员

`PUT /api/v1/tenants/:tid/members/:uid`（仅管理员，带版本）只能修改**已有**成员的角色或状态：

- 不能用它新增成员，也不能把已停用的成员直接改回 `active`（`409 member_rejoin_required`）。重新加入必须再走
  一次邀请、申请或目录添加；这些路径会复用原成员行并重新激活，保留历史引用。
- 最后一个有效管理员不能被降级或停用（`409 last_admin`）。数据库的延迟约束触发器保证同一条规则，直接改表
  也绕不过去。
- 停用立即生效：被停用的成员之后的每个请求和执行准入都被拒绝，已建立的 SSE 连接在推送下一条事件前关闭，
  他持有的运行时操作资格被撤回（见 [运行时控制](../runtime/runtime-control.md)）。已经开始的任务按原生命周期
  收尾，审计引用保留。

`GET /api/v1/tenants/:tid/members` 列出租户成员，任何当前成员都可以读取。

## 什么需要管理员

普通成员可以读取租户内的共享资源、创建 Project 和运行时、在 Issue 上协作。需要管理员的操作包括：

- 成员管理：修改成员、邀请与申请链接、审批申请、目录搜索与添加；
- 修改 Space 名称与描述；
- 删除 Project、强停运行时、读取全租户的资源状态概况；
- 修改空间的插件选择（见 [插件市场](../plugins/marketplace.md)）。

运行时内容（一个具体 Workspace 里的东西）还有更细的规则：只有它的创建者和当前管理员可以使用，见
[运行时控制](../runtime/runtime-control.md)。

## 相关决策

- [每个可见协作空间对应一个租户，以租户成员身份授权](https://github.com/ora-space/specs/blob/main/decisions/cloud/tenancy/20260923-one-space-per-tenant.md)
- [已登录用户自助创建租户，租户成为协作空间的隐式容器](https://github.com/ora-space/specs/blob/main/decisions/cloud/tenancy/0-self-serve-tenant-provisioning.md)
- [协作 Space 是租户内的共享、可见性与授权边界](https://github.com/ora-space/specs/blob/main/decisions/cloud/workspace/0-collab-space-boundary.md)
- [IDaaS UUID 继续作为登录键，globalUserId 作为人员目录关联键](https://github.com/ora-space/specs/blob/main/decisions/cloud/identity-access/20260923-huawei-global-id-alias.md)
