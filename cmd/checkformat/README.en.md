# cmd/checkformat: Code Formatting Gate

[中文](README.md) | [English](README.en.md)

`cmd/checkformat` is a formatting validation tool used in local checks and CI pipelines to enforce strict Go code layout.

## Responsibilities

- **Non-mutating format inspection**: Executes `gofumpt -l -extra .` across the repository to list any files that violate formatting rules without altering them on disk.
- **Strict gate enforcement**: Exits with code 0 if all Go source files adhere to formatting standards. If any improperly formatted file is detected, it prints the violating filenames to `stderr` and exits with code 1, prompting the developer to run `task format`.

## Boundaries and invariants

- **Read-only**: `cmd/checkformat` never writes to or modifies any source files. Automated formatting is performed separately via `task format` (`gofumpt` and `goimports`).
- **Standardized check**: Integrates directly with `task format:check` and `task check`.

See [cmd overview](../README.en.md) and [Taskfile.yml](../../Taskfile.yml).
