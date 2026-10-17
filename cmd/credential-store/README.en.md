# credential-store: the independent Credential Store

[中文](README.md) | [English](README.en.md)

Runs separately from Cloud and composes [`internal/credentialstore`](../../internal/credentialstore/README.en.md)
(proposed ADR "Git credentials in an independent Credential Store"). It does not connect to Cloud's
database; it reads only its own volume, the read-only keyring, TLS files and Cloud's two verification
key sets. Startup is refused when the keyring is missing or invalid, a key set cannot be loaded, two
listeners would trust the same identity, or addresses repeat; serving starts only after every listener
is bound. On SIGINT/SIGTERM the four listeners drain for at most 10 seconds. At startup and every 5
minutes after, a sweep destroys superseded versions past the one-hour retention and expired claims.

| Variable | Default / purpose |
|---|---|
| `CREDENTIAL_STORE_ROOT` | required, absolute; the credential volume, mode `0700` |
| `CREDENTIAL_STORE_KEYRING_FILE` | required; the read-only deployment KEK keyring |
| `CREDENTIAL_STORE_CERT_FILE` / `KEY_FILE` | required; service certificate of the three mTLS listeners |
| `CREDENTIAL_STORE_CLIENT_CA_FILE` | required; CA issuing the executor, credential-manager and KEK-operator client certificates |
| `CREDENTIAL_STORE_EXECUTION_PUBLIC_KEYS_FILE` | required; Cloud's Ed25519 execution-authorization key set (PEM) |
| `CREDENTIAL_STORE_SUBMISSION_PUBLIC_KEYS_FILE` | required; Cloud's submission-grant key set, distinct from the execution keys |
| `CREDENTIAL_STORE_REDEMPTION_ADDR` | `:8461`, redemption listener, joins only the executor's internal network |
| `CREDENTIAL_STORE_MANAGEMENT_ADDR` | `:8462`, management listener, Cloud only |
| `CREDENTIAL_STORE_KEY_ADDR` | `:8463`, key listener, KEK operator only |
| `CREDENTIAL_STORE_INGRESS_ADDR` | `:8460`, browser ingress, forwarded directly by the edge reverse proxy |
| `CREDENTIAL_STORE_INGRESS_CERT_FILE` / `INGRESS_KEY_FILE` | required; certificate of the browser ingress origin |
| `CREDENTIAL_STORE_INGRESS_ALLOWED_ORIGINS` | required; comma-separated frontend origins (`https://`, or `http://` on loopback) |
| `CREDENTIAL_STORE_INGRESS_RATE_PER_MINUTE` / `INGRESS_RATE_BURST` | `10` / `5`, submissions per remote IP |
| `CREDENTIAL_STORE_LOG_LEVEL` | `info`; logs go to stdout |

The ingress limits by the TCP peer address; behind a reverse proxy every request shares the proxy's
address, so the proxy must limit per client itself. Run
`go test -race ./internal/credentialstore/... ./cmd/credential-store/...`.
