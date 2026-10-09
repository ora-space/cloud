# internal/objectstore: Revision 对象存储签名与核验

[中文](README.md) | [English](README.en.md)

`internal/objectstore` 只服务 Revision 这一条链路:为一次交付尝试签发**单对象键、单方法、限时**的上传
授权(Cloud Revision D2/D3),以及在注册 Revision 之前用 HEAD 校验已上传对象的**存在性、大小与
SHA-256**(D4 step 2)。它不决定任何业务语义——授权给谁、何时给、校验失败后怎么处置都由 `internal/core`
持有;本包只把"签名"与"核验"这两件事做对。

签名是自带实现的 AWS Signature Version 4(SigV4),不引入新依赖:交付链路只需要两个操作(预签一个 PUT、
签一个 HEAD),仓库约定优先标准库而非新增模块。

## 文件

- `presign.go`:`Config`(端点、`public_endpoint`、region、bucket、路径风格、AK/SK、授权 TTL)与
  `Grant`(URL、方法、必须原样发送的请求头、到期时刻);`PresignPUT`(签一个 `PUT`,带
  `x-amz-sdk-checksum-algorithm: SHA256` 与 `if-none-match: *`)、`PresignPUTChecksum`(额外把 Node
  上报的 SHA-256 以 base64 签进 `x-amz-checksum-sha256`,长度或编码不符即拒绝),以及两者共用的
  SigV4 规范化:逐段 URI 编码(保留 `/`)、查询参数按名排序、`UNSIGNED-PAYLOAD`、`X-Amz-SignedHeaders`
  覆盖全部被签头。
- `verify.go`:`Verify` 用私有端点发 `HEAD`(带 `x-amz-checksum-mode: ENABLED`),比对 `Content-Length`
  与 `X-Amz-Checksum-Sha256`;不跟随重定向,超时 30s,任何基础设施细节都不越过本边界。
- `presign_test.go`:离线确定性单测,见"测试"。

## 依赖与调用方

- 依赖:仅标准库(`net/http`、`net/url`、`crypto/hmac`、`crypto/sha256`、`encoding/base64`、
  `encoding/hex`)。本包不导入 `internal/core`。
- 调用方:`internal/config`(`ObjectStoreConfig.Open` 构造 `*Config`,并用一次 `PresignPUT` 做启动期
  配置自检);`cmd/server`(把它装入 `store.ObjectStore`);`internal/core`(`revision_grants.go` 签发
  授权、`revision.go` 在事务外核验对象)。
- 凭据:只以**文件路径**进入配置(`access_key_id_file`/`secret_access_key_file`),由 `internal/config`
  读取并去首尾空白后填入 `Config`;值只存在于进程内——不落库、不打日志、不出现在授权返回值里。

## 不变量

- **授权即能力**:一个授权只覆盖一个对象键、一个方法、一段时限;URL 里没有的东西,Node 做不到。`PUT` 的
  签名覆盖 `host`、`if-none-match` 与校验和头,因此授权本身不可被改写成覆盖已有对象的请求。
- **已签发的授权不改写对象**:每个 `PUT` 都带 `if-none-match: *`。授权可以存活到结算之后,创建语义把
  第二次上传挡在存储侧,Cloud 的外部核验因此不会被事后替换的对象骗过。
- **不信任线上声明**:`Verify` 的 `size`/`sha256` 来自 Node 上报。形状校验(摘要必须是 32 字节的十六进制、
  大小非负)属于 `internal/core` 的 `validateDeliveryDeclaration`,在探测之前完成;`Verify` 只做事实比对:
  大小、摘要任一不等即失败(`false`),且失败原因不区分——对象缺失、摘要不符、存储没有摘要对交付是同一件事
  (D1:交付失败,D5 重试)。端点不可达、非 200 等瞬时故障返回 error,不构成"校验失败"的定论。
- **无 checksum 即失败**:HEAD 带 `x-amz-checksum-mode: ENABLED`;存储没有返回可解码的摘要(它从未收到过
  checksum,因而无法为内容背书)时,校验失败而不是"对象存在即通过"。
- **授权时限有界**:非正 TTL 取默认 15 分钟;落在 `[1s, 7d]` 之外的 TTL 直接拒绝,避免存储接受签名后在
  请求时以 403 拒绝,把配置错误伪装成现象不明的上传失败。
- **签发的时限即服务的时限**:签名与到期时刻都按秒截断,`Grant.Expires` 报出的就是存储实际执行的边界。
- **凭据不泄漏**:授权只返回 URL、方法与请求头,不含任何凭据;测试断言 URL 里不出现 secret。

## 测试

- 单测全部离线且确定性:真实 `httptest` S3 双端。覆盖授权对对象键的绑定与本地签名、`PresignPUTChecksum`
  对校验和头的绑定与非法摘要的拒绝、`public_endpoint` 与按秒截断的到期时刻、以及规范化对象路径。
- 上传授权的签发入口(交付执行、校验和映射)与对象核验在交付状态机中的位置由 `internal/core` 的单测和
  `integration/agent_run_delivery_test.go` 覆盖。
- 真实存储接受这些签名由 `integration/revision_test.go` 与 `integration/revision_sandbox_test.go` 对真实
  S3(RustFS)证明:签名 PUT 与 HEAD 核验、错误校验和与过期授权被存储拒绝、已存对象不可被仍有效的授权覆盖,
  以及沙盒网络内凭公开端点上传。它们由 `task test:revision`(`REQUIRE_S3=1`)与 cluster 的
  `task agent:acceptance` 运行,CI 的 Backend 工作流也以固定版本的 RustFS 运行它们。
