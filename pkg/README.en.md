# pkg: Public & Reusable Libraries

[中文](README.md) | [English](README.en.md)

`pkg` is reserved for code that is intentionally reusable by external modules and whose API can be maintained and supported as a public surface.

## Architectural policy and boundaries

- **Private by default**: Per `AGENTS.md`, new implementation packages in Ora Cloud must be placed under `internal/`.
- **Public API commitment**: Code is moved or added to `pkg/` only when there is an explicit requirement to expose it as an importable library for external consumers (e.g., client SDKs, shared types, or common utilities).
- **Zero internal coupling**: Packages in `pkg/` must never import anything from `internal/` or `cmd/`. They must depend solely on the standard library and approved external dependencies.

See [AGENTS.md](../AGENTS.md) and [Internal packages](../internal/README.en.md).
