# internal/config: configuration loading and validation

[中文](README.md) | [English](README.en.md)

`Load` uses a local Viper instance to load and validate service configuration, returning `Config`. There is no global mutable configuration.

An empty `plugins.personal_agent_target` disables declared personal-Agent admission. An explicit supported Linux target opts in only after Controller/Node and plugin adapters are deployed; execution still checks the current incarnation's target, contract and protocol.

| File | Responsibility |
|---|---|
| `config.go` | Main types, defaults, explicit environment bindings and common validation |
| `env.go` | Maps configuration keys to `CLOUD_*` variables |
| `control_tls.go` | Constructs TLS 1.3 mutual-authentication material for the control listener |
| `object_store.go` | Validates storage configuration, reads key files and constructs in-memory `objectstore.Config` |
| `repository.go` | Deployment references and switches for Git refs, executor and credential services |

Without an explicit path it searches `./configs`, `../configs` and `.` for `config.yaml`; commands pass explicit `-config` paths. Variables such as `CLOUD_DATABASE_DSN` override leaf keys. Source code and the [Configuration reference](../../docs/operations/configuration.en.md) own complete defaults and ranges.

Configuration stores deployment references; raw private keys and secrets load from separate files and never enter configuration bodies, logs or databases. This package validates startup configuration; Core owns durable window freezing, database clocks and recovery decisions. Enabled object storage does not establish online health.

`issue_runs.residual_reconcile_interval` sets the cadence of Cloud's residual-ledger recovery pass and defaults to `1m`; `CLOUD_ISSUE_RUNS_RESIDUAL_RECONCILE_INTERVAL` overrides it. Explicit zero, negative and malformed durations reject startup, and there is no disable switch. The interval only schedules scans; it is not a timeout proving successful resource cleanup. Cloud consumes real facts through existing entrypoints without adding a Controller discovery contract; missing evidence leaves the ledger open.

Tests cover defaults, environment overrides, invalid durations, TLS, storage and repository configuration. See [server](../../cmd/server/README.en.md), [Bounded startup](../../docs/agent-runs/startup-settlement.en.md) and [Deployment egress](../../docs/operations/deployment-egress.en.md).
