# cmd/credential-store: the independent Credential Service process

[English](README.en.md) | [中文](README.md)

`cmd/credential-store` runs the independent Credential Service. It is a **separate deployment unit** from
Cloud: its own process, its own volume, its own key material, its own trust root (Secret Store D1).
Nothing it holds is reachable from Cloud except through the mutually authenticated ingress, and it shares
no database, connection, volume or credential with Cloud's business store (this command connects to no
PostgreSQL at all).

## Responsibilities

- **Configuration**: `config.LoadCredentialStoreConfig` reads the service's own configuration file and
  honors only the `CREDENTIAL_STORE_*` environment prefix, so nothing that configures Cloud can set this
  service's listen address or volume, and the service cannot inherit Cloud's values.
- **Startup gate**: before the listener is bound, `CredentialStoreServiceConfig.Store()` (the volume must
  exist and not be readable by other local accounts; the KEK must be delivered by the deployment and
  usable), `ServerTLS()` (server certificate, private key and client authority, always
  `RequireAndVerifyClientCert` with TLS 1.3 as the floor) and `credentialstore.NewServer` (the three fixed
  identities must be present and distinct) must all succeed. D6/D8 require an unavailable KEK to be a
  startup refusal that can never present itself as an empty store.
- **Audit logging**: D7 makes every operation an audit record, so the process refuses to start with no log
  destination (neither console nor file) rather than discarding them silently. A line names the operation,
  credential id, service identity and result — never a token, ciphertext, wrapped data key or KEK.
- **Lifecycle**: `signal.NotifyContext` handles `SIGINT`/`SIGTERM`, `http.Server.Shutdown` drains within
  10 seconds, and the log buffer is `logger.Sync`ed on exit.

## Invariants

- **No domain logic**: the process does configuration, wiring, lifecycle and exit codes only. Ordering,
  idempotency, retention windows and failure mapping live in `internal/credentialstore`; the identity
  table and startup validation live in `internal/config`.
- **To listen is to require mutual authentication**: `tls.NewListener` wraps the listener directly and
  demands a client certificate, so no path lets an anonymous request reach the handler.
- **Locations are logged, never secrets**: the startup line names the listen address, the volume path and
  the KEK identifier (`keyId`), all of which are locations rather than secrets.

## Configuration

`configs/credential-store.yaml` or `CREDENTIAL_STORE_*` environment variables; the fields are those of
`internal/config.CredentialStoreServiceConfig`: `listen`, `volume`, `kek_file`, `certificate_file`,
`private_key_file`, `client_ca_file`, `ingress_writer_identity`, `broker_resolver_identity`,
`kek_operator_identity`, `superseded_retention` (0 selects the 1-hour default) and `logger`. Certificates
and KEKs are given as **file paths** only, never inline in a configuration file or a command line.

See the [cmd entry overview](../README.md), the
[credential service package](../../internal/credentialstore/README.md) and
[configuration loading](../../internal/config/README.md).
