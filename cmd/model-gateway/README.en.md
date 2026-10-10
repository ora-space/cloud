# model-gateway: personal model access service

[中文](README.md) | [English](README.en.md)

This trusted process runs separately from Cloud's business server and composes modelgateway, Core
and PostgreSQL. It reads Cloud database/public verification-key configuration and its own deployment
TLS/master-key file references. Startup validates migrations and the previous encryption identity;
shutdown cancels model requests and joins all three listeners.

| Environment | Default / responsibility |
|---|---|
| `MODEL_GATEWAY_CREDENTIAL_ADDR` | `:8083`, Gateway-only key writes |
| `MODEL_GATEWAY_RUNTIME_ADDR` | `:8443`, token-authorized model data |
| `MODEL_GATEWAY_GRANT_ADDR` | `:8444`, dedicated Node mTLS grants |
| `MODEL_GATEWAY_PUBLIC_ORIGIN` | `https://ora-model-gateway:8443` |
| `MODEL_GATEWAY_CERTIFICATE_FILE` / `PRIVATE_KEY_FILE` / `CA_FILE` | `/etc/ora-model-management/` file references |
| `MODEL_GATEWAY_MASTER_KEY_FILE` / `MASTER_KEY_ID` | `/etc/ora-model-secrets/master-key` / `v1` |

`-healthcheck` verifies HTTPS/public trust and database readiness without opening keys. Exact private
fixture hosts require both `MODEL_GATEWAY_DEVELOPMENT=true` and `MODEL_GATEWAY_DEVELOPMENT_ALLOW_HOSTS`;
the production default is public HTTPS only. Run package tests, Cloud `task check`, `task test:race`
and `task build`; cluster M4 provides real CLI acceptance.
