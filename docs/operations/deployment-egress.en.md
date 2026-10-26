# Shared Deployment Egress

English | [中文](deployment-egress.md)

Personal model-connection admission, workflow-asset HTTPS downloads, model-gateway forwarding and the Repo Executor's
external Git access all read one out-of-package `deployment-egress.json`. The Cloud server and model-gateway take the
same `-deployment-egress /absolute/path/deployment-egress.json` flag, or the same `ORA_DEPLOYMENT_EGRESS_FILE`
environment variable; the flag wins. The Repo Executor reads the environment only: `REPO_EXECUTOR_DEPLOYMENT_EGRESS_FILE`
wins, otherwise `ORA_DEPLOYMENT_EGRESS_FILE`, and the path must be absolute. The file is read only at startup, must be
mounted read-only, and cannot be changed by a user, a plugin or the personal-connection API. With no path configured,
public mode stays in effect. The policy shape and
refusal rules follow
[Model forwarding and Cloud asset downloads share a deployment-owned egress allowlist and explicit resolution](https://github.com/ora-space/specs/blob/main/decisions/cloud/model-access/20261017-deployment-owned-egress-and-explicit-resolution.md)
(approved).

## Enterprise mode

```json
{
  "schemaVersion": 1,
  "mode": "enterprise",
  "resolver": {"transport": "tcp", "address": "192.0.2.53:53"},
  "targets": [
    {"host": "model.example.invalid", "port": 443, "allowPrivate": true, "allowedCidrs": ["10.60.0.0/16"]},
    {"host": "assets.example.invalid", "port": 443, "allowPrivate": true, "staticAddresses": ["10.61.0.10"]}
  ],
  "topology": {
    "clusterCidrs": ["172.30.0.0/16"],
    "hostAddresses": ["192.0.2.10"]
  }
}
```

The example shows the shape only; its addresses and names are not site parameters. The deployment entry writes
`topology` after collecting Docker's default bridge, Compose networks, the dynamic Node/Workspace pools, and the
host interfaces, bridges and gateways on both stacks, then merging administrator supplements. Cloud only consumes the
file and does not access the Docker socket.

- `targets` is the exact host:port set shared by model forwarding and asset downloads. The two callers cannot be
  configured apart. Case, one trailing root dot, and default 443 versus explicit 443 are equivalent; any other port,
  suffix, alias or numeric-IP URL is not permitted. A model baseUrl forbids a query; an asset URL keeps its original
  path and a legitimate signed query. Permission carries no personal model key and does not replace asset-install
  authorization or checksum verification.
- Targets that are not listed, public or private, are refused before DNS. `allowPrivate` defaults to false and allows
  only an RFC1918/ULA exception; `allowedCidrs` may be omitted, but when present it cannot be an empty array, and one
  target cannot borrow another's CIDR.
- Cluster ranges, host addresses, loopback, link-local and the other reserved ranges already refused are always
  refused, ahead of the allowlist and the private-network permission. A static address that points at them makes the
  file fail to load. Both `topology` tables must exist and be non-empty.
- `resolver` accepts only DNS-over-TCP at an explicit numeric IP:port. A and AAAA share a 5-second budget, with at
  most 8 CNAME hops per family; each TCP DNS message is at most 65535 bytes and 256 resource records. An error in
  either family, or any answer outside the permitted ranges, refuses the target; one family with a definite empty
  answer and a valid other family may pass. Authority and additional records are not connection addresses.
- A non-empty `staticAddresses` replaces dynamic resolution for that target completely, with no fallback on failure.
  The resolver may be omitted when every target is static. Enterprise resolution does not read system DNS, a search
  domain or the default Docker DNS, and does not fall back to public DoH. The resolver's own address cannot be used as
  a model or asset HTTP target.
- The file is limited to 1 MiB. An unknown version or field, a duplicate key, null, a malformed address, a duplicate
  target, an empty CIDR or static array, or a missing required topology makes startup fail. Enterprise mode together
  with `MODEL_GATEWAY_DEVELOPMENT_ALLOW_HOSTS` refuses to start.

## Public mode and resolver agreement

An explicit public file is `{"schemaVersion":1,"mode":"public"}` and rejects enterprise-only fields. Cloud, model-gateway
and the Repo Executor read `MODEL_GATEWAY_DNS_HTTPS_URL` and `MODEL_GATEWAY_DNS_BOOTSTRAP_IPS` (comma-separated public
numeric addresses). All three processes must receive the same values. When unset, all three use the Cloudflare DoH
defaults `https://cloudflare-dns.com/dns-query` and `1.1.1.1,1.0.0.1`. Enterprise mode ignores both variables.

Every new application connection resolves and checks all answers again, dials a numeric address, and verifies TLS
against the normalized original name. Environment HTTP/SOCKS proxies and redirects are disabled. Enterprise
application transport does not attempt HTTP/2. Only enterprise connections have a 30-minute hard lifetime;
expiry closes active connections without automatically resending model generations. Public mode retains its
existing behavior without a hard lifetime. Both modes use a 60-second idle timeout.

Checked addresses are still dialed in DNS order inside one whole-dial budget. Each attempt gets the remaining budget
divided by the addresses left, with a roughly two-second floor clamped by the total deadline, so one unresponsive
edge address cannot spend the time reserved for the others. When every address fails, the bounded error and its
structured-log detail list a reason per numeric address (timeout, connection refused, network unreachable, or not
attempted); they never include the URL, query, headers or credentials.

## Updates, revocation and health

Startup configuration errors identify the field path and reason, such as `targets[1].allowedCidrs: empty`,
without field values, addresses or raw configuration. Unknown field names are not echoed either, so arbitrary
operator input cannot become diagnostic content.

To change the file, stop the Cloud server, every model-gateway and every Repo Executor, confirm the old processes have
exited, then replace the shared file and start again. Editing the file alone does not change the snapshot already
loaded; a failed load does not restore the old file or switch back to public mode, and a Repo Executor that fails to
load refuses to start. Rollback is the same stop-and-start with the previous file chosen explicitly. Process exit
cancels in-flight model streams, asset requests and external Git tasks.

The Cloud server's `GET /healthz`, `GET /healthz` on model-gateway's **credential listener**, and `GET /healthz` on the
Repo Executor's **health listener** (`REPO_EXECUTOR_HEALTH_ADDR`, `:8084` by default, plaintext, deployment network only)
return `X-Ora-Egress-Schema: 1` and `X-Ora-Egress-Digest` (SHA-256). The digest covers the canonical policy and, in
public mode, the DoH settings actually in use. It contains no key and does not return the policy body. `model-gateway
-healthcheck` does not report either header. A deployment entry checks these processes' health and that the digests
match; a matching digest does not replace the per-request check on each outbound call.

In enterprise mode, creating or changing a personal model connection checks identity, ownership, version and a
completed idempotent replay inside a short transaction, performs DNS and address admission outside that transaction,
then rechecks current authoritative state in the write transaction. A DNS refusal returns `400 invalid_model_url` and
writes no new connection version. A completed legal replay, a pure disable, deletion and clearing the key do not
depend on that admission. A save that passed still does not let a later forward skip its own outbound check.

Object-key fetches from the configured internal object store keep using the deployment client and are not a fallback
for a refused HTTPS URL. This policy does not cover Node plugin installation, marketplace Git sync or authentication.

A deployment tool can first run `/app/model-gateway -check-egress -deployment-egress <file>` or
`/app/repo-executor -check-egress -deployment-egress <file>`. That entry uses the real service's parser and the
public-DNS environment, prints only a JSON object with `schemaVersion` and `digest` to standard output, and does not
query DNS, connect to the database, load credentials or listen. An invalid file exits non-zero. After applying the
file, compare that digest with the healthz responses.
