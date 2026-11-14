# 单机离线部署

[English](deployment.en.md)

依据：[Cloud 单进程（A1）](https://github.com/ora-space/specs/blob/main/decisions/cloud/lifecycle/0-single-process-cloud-on-enterprise-intranet.md) D2/D5/D6 和[登录与会话（A6）](https://github.com/ora-space/specs/blob/main/decisions/cloud/login/0-idaas-or-email-login-with-shared-web-and-desktop-sessions.md)。
当前构建不提供 IDaaS 登录，`V2_LOGIN=idaas` 在访问数据前拒绝启动，不能作为可用部署上线，
不得回落到 email。当前版本尚未提供版本化迁移。正式部署须使用支持所需能力的版本，
并配置经过验证的 HTTPS 入口。

## 在外网构建

在与目标机同架构的 Linux 上，安装 Go（版本见 go.mod）、Node >=24、npm、Python 3、Git、curl、
Rust 1.95.0 和已信任来源的 Deno 2.9.5。设置 `DESKTOP_REPOSITORY` 指向包含 prepare-node.sh
固定提交的 desktop 仓库。执行：

```sh
DESKTOP_REPOSITORY=/build/desktop V2_NODE_ASSETS_DIR=/build/assets \
  V2_OFFLINE_OUTPUT=/build/cloud-linux.tar.gz task v2:offline:build
```

入口先调用 `prepare-node.sh`：按固定提交编译 Node/reaper，按固定 SHA-256 校验两个 Agent
插件，检查 Deno 版本并记录本机摘要。构建并发限制为 2。Deno 和本机编译摘要只证明完整性，
不是官方发布签名；构建机及其工具链必须可信。前端强制 `VITE_DEMO_MOCK=0`，使用 offline mode。
包只包含服务端、web 构建、五项运行资源、清单、启动器和本文档，不复制用户配置、数据库或数据目录。
准备目录中的其他内容（包括源码、用户文件）不进入包。`assets.json` 使用相对路径，运行时仍兼容旧的绝对路径开发清单。

将包通过受信任通道传入内网，解压到新的版本目录，在该目录执行 `sha256sum --check SHA256SUMS`。
这份清单检测传输损坏，不代替可信传输或发布签名。解压及启动不下载资源；运行时仍需访问配置的
PostgreSQL、IDaaS、模型和内部 Git/S3 服务。Git 和系统库须预先安装，离线包不包括操作系统依赖。

## 主机与配置

使用专用非 root 系统账号运行 Cloud。第一版 Agent 与 Cloud 同用户、不隔离，仅适用于互相信任的成员。
准备 PostgreSQL 和专用数据库、数据库身份；不要使用演示数据库或演示凭据。当前版本在启动时
应用 schema，没有版本化迁移，不能用于需要保留数据的正式版本升级。

在进程管理器的受保护配置中设置以下变量，不将密钥写入包或命令日志：

- `V2_LOGIN=idaas`：唯一部署登录方式；当前构建不提供此能力，启动会明确拒绝。
- `V2_PUBLIC_URL`：部署公开 HTTPS 地址；`V2_IDAAS_BASE_URL`、`V2_IDAAS_CLIENT_ID`、
  `V2_IDAAS_CLIENT_SECRET` 是 A6 规定的部署配置；当前构建尚不支持 IDaaS 登录。
- `V2_DATABASE_URL`：专用 PostgreSQL 连接，按企业要求启用 TLS 和最小权限。
- `V2_ADDR`：HTTP 监听地址。仅暴露给 HTTPS 终止入口；当前构建不提供 HTTPS 终止配置。
  部署必须使用 HTTPS，不得将这里的 HTTP 监听直接当作正式用户入口。
- `V2_DATA_DIR`：绝对、持久化目录，例如 `/srv/ora/data`；目录和所有父目录均不可被组或其他人写入，
  不能放 `/tmp`。由运行账号创建，建议 `0700`，不要通过宽化权限解决 Node 启动失败。
- `V2_MARKET_CREDENTIAL_KEY`：按 pluginmarket 的密钥格式生成（32 字节 Base64）；作为稳定密钥持久保存，
  与数据一起备份。重建、替换或丢失会使现有市场 AK/SK 无法解密。
- `V2_PLUGIN_MARKETPLACE_URL`：启动器未配置时设为空，关闭官方市场。直接启动 binary 时未设置仍使用开发默认；
  显式空字符串关闭同步和旧官方目录可见性。自定义空间市场支持 S3+AKSK 及管理员在设置页配置的内部 HTTPS Git 认证（用户名与令牌/密码，经 `V2_MARKET_CREDENTIAL_KEY` 加密存库）。
- `V2_PLUGIN_MARKETPLACE_BRANCH`、`V2_PLUGIN_MARKETPLACE_INTERVAL`：仅官方源启用时使用，默认 main、5m。
- `V2_MODEL_*` / `V2_ANTHROPIC_MODEL_*`：模型连接，具体变量见服务 README。
  改变连接、模型或凭据绑定可能让旧对话不能继续；在变更前通知用户，保留历史并新建对话。

`start.sh` 从自身位置确定 `V2_FRONTEND_DIR` 和 `V2_NODE_ASSETS_DIR`，校验包文件后执行服务。
不能传入 `-fixture-check`、`-test-fixtures`、`-cleanup` 作为部署启动方式。程序不会加载开发用户 preset、
准备 demo 项目或预热 demo Node；当前构建因不提供 IDaaS 登录而直接阻止部署启动。

企业 CA 必须安装到运行主机的系统信任库（按企业发行版流程更新信任库），Git 和 Go HTTPS 均使用
系统 CA。不得关闭 TLS 校验。HTTPS 终止集成点是 Cloud HTTP 监听之前；部署需核对安全 Cookie 和回调地址，
公开地址只取配置，不信任转发头。本文不指定反向代理或证书管理方案。

## 首次内网验收

仅对已提供 IDaaS 登录和版本化迁移的目标版本执行以下完整验收，并遵循该版本的管理员初始化和迁移说明；
本构建仅供部署预检，不能上线，也不能以邮箱登录或模型 fixture 替代现场验收。

1. 准备专用 PostgreSQL、持久数据卷、已登记的 IDaaS 应用、管理员和第二名员工账号，以及一个真实项目、
   可用模型连接、含可安装工作流插件的内部 Git 仓库和对应 S3 包。确认包与目标 Linux 架构匹配，执行
   `sha256sum --check SHA256SUMS` 全部通过。为验收使用独立实例，不重启或修改其他实例。
2. 按主机发行版的企业流程将企业 CA 安装到系统信任库。以实际 Cloud 运行身份，使用现场已有的 DNS/TLS
   诊断工具，逐一核对 Git、S3 和模型主机：解析结果符合预期，HTTPS 主机名、证书链和有效期校验通过，
   再经 Cloud 执行下述实际请求。不得关闭证书校验或公网地址检查；若内网解析地址被现有地址策略拒绝，
   记录为阻断条件，不以 TLS 绕过或放宽地址检查继续。凭据只通过受保护的配置或设置页输入，
   不放入命令行、URL、截图或日志。
3. 在验收实例按目标版本说明完成迁移和 IDaaS 管理员初始化，再邀请第二名员工登录并加入团队。
   从浏览器确认 web 与 `/api` 同源，IDaaS 回调使用登记的公开 HTTPS 地址，会话 Cookie 带 `Secure`、
   `HttpOnly` 和 `SameSite=Lax`；核对不存在预置演示账号、项目或内容。HTTPS 终止接入点仍在 Cloud HTTP
   监听之前；具体方案须由部署方确认，本步骤不指定 Cloud 直配或反向代理。
4. 管理员在“设置 → 插件 → 管理市场”添加自定义内部 HTTPS Git 来源，填写分支、Git 用户名和只读令牌，
   包下载选择 S3 签名下载，分别填写 S3 端点、桶、区域及 AK/SK。Git 凭据与 S3 凭据分开，
   持久保存并备份 `V2_MARKET_CREDENTIAL_KEY`。点击同步，确认来源无失败提示、目录出现预期版本；
   安装一个工作流插件，确认工作流已导入并可在工作流页打开。
5. 另建独立验收来源（使用不同仓库地址，不能只改同一仓库 URL 写法），先正确同步得到目录，
   再仅替换该来源的 Git 令牌为错误值并同步，确认出现可读的仓库认证失败提示且上一版目录保留。
   再清除已保存的 Git 认证并同步：需要认证的仓库仍应明确失败，旧目录仍保留。恢复正确凭据后应可再次同步。
   全程不修改正常来源；Git 失败时不要更换 S3 密钥来掩盖问题。
6. 在真实项目上运行已安装的工作流并完成一轮对话，确认实际模型、包下载和 Node 执行均成功，结果与历史可读。
   记录成员、插件、历史和数据卷中的验收文件，然后只重启验收实例；确认登录后这些状态与文件仍在、
   密钥仍可解密来源凭据且可再次同步。重启前未结束的执行应标记中断，不以自动续跑作为成功标准。

每步保存脱敏证据：程序版本、平台架构、SHA 校验结果、时间、操作和结果；不记录密钥、令牌或完整认证响应。
失败时按顺序区分 DNS 解析、TLS 信任/主机名、Git 或 S3/模型认证、包下载/摘要校验、Node/工作流执行链，
记录安全错误码与脱敏提示后停止对应步骤；不得把目录保留或 fixture 输出当作同步、安装或真实执行成功。

## 备份和升级

备份 PostgreSQL、整个 `V2_DATA_DIR` 数据卷和稳定的加密密钥；采用停机或一致性快照确保数据库与数据卷一致，
验证能恢复。Agent 改动只在本机工作目录，不会自动推送到 Git，不能只备份数据库。

升级前停止本实例并确认 Node 已退出，备份后将新包解压到新的程序目录，校验摘要。当前版本没有版本化迁移；需要迁移时，
必须使用提供迁移支持的目标版本，并遵循该版本的迁移说明。
替换程序和资源，保持数据目录、数据库、密钥及主机配置不变；不要删除数据、运行 demo reset、重新 seed 或复制旧包覆盖
新版本文件。未完成的执行在重启后标记中断，不自动续跑。数据库迁移后的回退必须按已验证的备份恢复方案进行，
不能假设替换旧 binary 即可回退。静态 HTML 和资源使用 `no-cache`，让浏览器在升级后重新验证文件。
