# internal/egress: deployment egress policy

[中文](README.md) | [English](README.en.md)

Shared immutable deployment policy for model requests, workflow asset downloads and external Git access. Consumers own wiring; business authorization is outside this package.

| File | Responsibility |
|---|---|
| `config.go`, `config_errors.go` | Bounded loading and strict schemaVersion 1 JSON parsing; rejects duplicate/unknown fields and trailing data; normalizes hosts, ports, CIDRs and static addresses |
| `policy.go` | `Policy`, target admission, address checks and canonical configuration digest |
| `dns.go`, `public_dns.go` | Controlled resolution and dialing: checks all resolved addresses and connects only to validated numeric addresses |
| `health.go` | Schema/digest readiness metadata |

Public mode accepts only public destinations. Enterprise mode accepts deployment-listed targets and checks static addresses or deployment TCP DNS, allowed CIDRs and internal topology constraints. An empty configuration path selects public policy; read errors, unknown versions and invalid configurations return errors. Readiness may expose the digest, never raw configuration or endpoint details.

Package tests cover configuration rejection, DNS, address ranges, connection pinning and digests. See [Shared deployment egress configuration](../../docs/operations/deployment-egress.en.md) for format and deployment steps.
