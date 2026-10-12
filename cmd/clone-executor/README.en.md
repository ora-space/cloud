# cmd/clone-executor: the credential executor process

[English](README.en.md) | [中文](README.md)

`cmd/clone-executor` runs the credentialed-clone executor — the **credential-executing identity C** of the
approved Node isolation decision (`20261010-controlled-node-git-authentication-isolation` D1/D7/D11) — as a
**process of the current product** rather than as part of the historical Desktop Node. It runs with its own
operating-system identity, its own volume and its own trust root: it redeems one Cloud-signed single-use
authorization as the `broker-resolver` identity, materializes the token where only it can read it, runs one
constrained `git clone` through a deployment-fixed credential helper, and destroys the material before it
answers.

## Responsibilities

- **Configuration**: `config.LoadCloneExecutorConfig` reads the service's own configuration file and honors
  only the `CLONE_EXECUTOR_*` environment prefix, so nothing that configures Cloud can set this service's
  volumes, helper path or listen address.
- **Startup gate**: before the listener is bound, `CloneExecutorConfig.Executor()` (the materialization and
  checkout roots must be different absolute paths; the helper and Git must be absolute paths free of shell
  metacharacters; `HOME` must exist and be exactly 0700; a verification key must be present; the
  `broker-resolver` Store client must be constructible), `ServerTLS()` (server certificate, private key and
  client authority) and `cloneexec.NewServer` (the dispatching identity must be present) must all succeed.
  D17 requires a missing input to be a startup refusal rather than something the first job discovers.
- **Startup sweep**: `executor.Sweep()` runs **before the listener**. It repairs the one failure a running
  process cannot — a previous crash or a killed container left a token on disk with no process left to
  destroy it, and only a start can (D13).
- **Lifecycle**: `signal.NotifyContext` handles `SIGINT`/`SIGTERM`, `http.Server.Shutdown` drains within 10
  seconds, and the log buffer is `logger.Sync`ed on exit. The write timeout has to outlast a clone, because
  the response is what the clone produces, so it is `Timeout + 1 minute`.

## Invariants

- **No domain logic**: the process does configuration, wiring, lifecycle and exit codes only. The
  constrained Git invocation, the binding comparisons, the ordering of redemption and cleanup, and the
  startup gate itself all live in `internal/cloneexec` and `internal/config`.
- **To listen is to require mutual authentication**: `tls.NewListener` wraps the listener directly (rather
  than going through `ServeTLS`, whose empty-file-name form silently depends on the certificate already
  being in the config) and the dispatching identity in the client certificate is checked above the handler,
  so no path lets an anonymous request reach it.
- **Locations are logged, never secrets**: the startup line names the listen address, the two roots and how
  many entries the sweep removed. A credential and an authorization never reach a log line.

## Configuration

`configs/clone-executor.yaml` or `CLONE_EXECUTOR_*` environment variables; the fields are those of
`internal/config.CloneExecutorServiceConfig`: `listen`, `material_root`, `checkout_root`, `home_directory`,
`helper_path`, `git_path`, `ssl_ca_info`, `authorization_public_key_file`, `certificate_file`,
`private_key_file`, `client_ca_file`, `dispatcher_identity`, `store_endpoint`, `store_certificate_file`,
`store_private_key_file`, `store_ca_file`, `timeout` and `logger`. Certificates, private keys and public
keys are given as **file paths** only, never inline in a configuration file or a command line.

It holds the **public half only**: Cloud's signing key is neither in this process nor on any volume it
mounts.

See the [cmd entry overview](../README.md), the [executor package](../../internal/cloneexec/README.md), the
[materialization layout](../../internal/credentialmaterial/README.md) and
[configuration loading](../../internal/config/README.md).
