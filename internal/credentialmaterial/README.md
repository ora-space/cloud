# credentialmaterial：一次任务的凭据材料目录

[中文](README.md) | [English](README.en.md)

Repo Executor 把兑换得到的明文凭据写入私有目录，固定 helper `git-credential-ora` 从中读取（ADR `repo-executor/0` D3）。
两个程序必须对布局一致，又必须彼此分开：helper 在 Git 执行的路径上，只链接本包与仓库规范化。

```text
<root>/                 部署所有
└── <随机名>/           一次任务的材料，0700
    ├── scope           0600，凭据可交付的规范化仓库 URL
    ├── username        0600
    └── password        0600
```

| 接口 | 职责 |
|---|---|
| `NewScope(repository)` | 经 `cloneauth.NormalizeRepository` 规范化后拆成 `Scope{Host, Path}` |
| `Stage(root, scope, username, password)` | `Mkdir` 0700 随机目录；文件 `O_CREATE\|O_EXCL` 0600、逐个 fsync，再 fsync 目录与 root；失败时删除已建内容 |
| `Material.Dir()` / `Material.Destroy()` | 材料目录；删除并以 `Lstat` 确认不存在，再 fsync root；可重复调用 |
| `Sweep(root)` | 执行器启动时删除 root 下全部条目（崩溃遗留） |
| `ReadScope(dir)` / `ReadSecret(dir)` | helper 侧读取；先读作用域，匹配后才读明文 |

不变量：

- `scope` 必须已是规范化形式，`Stage` 不接受 helper 会读成另一种拼写的作用域。
- 用户名与密码非空且不含 CR、LF、NUL：helper 按凭据协议逐行输出，换行会注入额外属性。
- 每次读取都校验目录是 0700 目录、文件是 0600 普通文件（不跟随符号链接）；否则返回 `ErrTampered`，不交付任何内容。
- 根与材料目录都必须是绝对、干净的路径；错误信息不包含任何材料内容。

测试覆盖权限与布局、umask 无关、拒绝的输入不留下文件、`Destroy` 的确认与失败报告、`Sweep`（含不跟随符号链接）、
被篡改的权限 / 符号链接 / 作用域。依赖：标准库与 `internal/cloneauth`。
