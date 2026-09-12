# pkg: 公共可复用库

[中文](README.md) | [English](README.en.md)

`pkg` 专用于存放明确供外部模块复用、且其 API 能够作为公共接口进行长期维护与支持的代码。

## 架构策略与边界规范

- **默认私有**：根据 `AGENTS.md`，Ora Cloud 的新实现包必须放在 `internal/` 下。
- **公共 API 承诺**：只有在明确需要将代码作为可供外部使用者导入的库时（例如客户端 SDK、共享类型或通用工具），才会将代码添加或移动到 `pkg/`。
- **零内部反向耦合**：`pkg/` 下的包绝对禁止导入 `internal/` 或 `cmd/` 中的任何内容。它们只能依赖 Go 标准库以及经过批准的第三方外部依赖。

参见 [AGENTS.md](../AGENTS.md) 与 [内部包规范](../internal/README.md)。
