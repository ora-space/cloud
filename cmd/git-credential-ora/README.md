# git-credential-ora：Repo Executor 的固定 Git credential helper

[中文](README.md) | [English](README.en.md)

执行器为外部远端配置唯一的 helper（ADR `repo-executor/0` D3）：

```text
-c credential.helper= -c "credential.helper=/usr/local/bin/git-credential-ora <material-dir>" -c credential.useHttpPath=true
```

Git 以 `git-credential-ora <material-dir> get|store|erase` 调用它，请求写在 stdin。

- `get`：只有当请求 `protocol=https`，且 `https://<host>/<path>` 经 `cloneauth.NormalizeRepository` 后**等于**暂存的作用域
  时，才输出 `username=` 与 `password=`。因此 `.git` 后缀、尾部 `/`、host 大小写、`:443` 的差异视为相同；其他 host、
  端口、路径（含大小写、子路径、上级路径、`..`、百分号编码）、`http`、缺少 path 或重复属性一律**不输出任何内容**，
  按协议即“没有凭据”。重定向、子模块或同 host 的另一个仓库因此拿不到明文。
- `store`、`erase`：什么也不做，也不读取材料。
- 先读作用域，匹配后才读明文；参数错误或材料不可读时退出码 1，stderr 只说明失败原因，从不包含材料或请求内容。

测试直接驱动 `run`：匹配与不匹配矩阵、带端口的作用域、`store`/`erase` 无输出、失败不输出；并用真实
`git credential fill` 验证 Git 实际写出的请求形态。依赖：`internal/credentialmaterial`、`internal/cloneauth`。
