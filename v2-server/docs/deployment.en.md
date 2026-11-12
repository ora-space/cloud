# Single-machine offline deployment

[中文](deployment.md)

Basis: [Single-process Cloud (A1)](https://github.com/ora-space/specs/blob/main/decisions/cloud/lifecycle/0-single-process-cloud-on-enterprise-intranet.md) D2/D5/D6 and [Login and sessions (A6)](https://github.com/ora-space/specs/blob/main/decisions/cloud/login/0-idaas-or-email-login-with-shared-web-and-desktop-sessions.md).
This build does not provide IDaaS login. `V2_LOGIN=idaas` refuses startup before touching data;
it is not a deployable release and must not fall back to email. The current version does not
yet provide versioned migrations. Production deployment
requires a version supporting the required capabilities and a verified HTTPS entrypoint.

## Build online

Use Linux matching the target architecture. Install Go (go.mod), Node >=24, npm, Python 3, Git,
curl, Rust 1.95.0 and a trusted Deno 2.9.5 installation. The desktop repository must contain the
commit pinned in prepare-node.sh.

```sh
DESKTOP_REPOSITORY=/build/desktop V2_NODE_ASSETS_DIR=/build/assets \
  V2_OFFLINE_OUTPUT=/build/cloud-linux.tar.gz task v2:offline:build
```

The builder runs prepare-node.sh first: it compiles Node/reaper at the pinned commit,
checks both Agent plugins against pinned SHA-256 digests, and checks Deno's version before recording
its local digest. Build concurrency is limited to two. Local executable and Deno digests prove
integrity, not publisher authenticity; trust the build machine and toolchain. The frontend uses
offline mode with `VITE_DEMO_MOCK=0`. Only the server, built web files, five runtime resources,
manifest, launcher and deployment guides are packaged. User configuration, databases, data volumes
and other files from the preparation directory are excluded. Manifest paths are relative; legacy
absolute development manifests remain supported.

Transfer through a trusted channel, unpack into a new version directory and run
`sha256sum --check SHA256SUMS` there. Checksums detect corruption and do not replace trusted delivery
or release signing. Unpacking and startup download nothing. Runtime access to configured PostgreSQL,
IDaaS, model, internal Git and S3 services is still required. Install Git and system libraries on
the target host beforehand; operating system dependencies are not bundled.

## Host and configuration

Use a dedicated non-root account. Agents run as the Cloud account without isolation; the first
release is suitable only for mutually trusted members. Provision a dedicated PostgreSQL database
and identity without demo credentials. This version applies schema directly at startup and has
no versioned migrations; it cannot be used for production version upgrades that must preserve data.

Keep configuration and secrets in protected process-manager configuration, outside the archive:

- `V2_LOGIN=idaas`: the only deployment login. This build does not provide it and refuses startup.
- `V2_PUBLIC_URL`: public HTTPS address; configure `V2_IDAAS_BASE_URL`, `V2_IDAAS_CLIENT_ID` and
  `V2_IDAAS_CLIENT_SECRET` as specified by A6. This build does not yet support IDaaS login.
- `V2_DATABASE_URL`: dedicated PostgreSQL connection with required TLS and least privilege.
- `V2_ADDR`: HTTP listener, reachable only through the HTTPS termination entrypoint. HTTPS is
  mandatory; this build does not provide HTTPS termination configuration.
- `V2_DATA_DIR`: an absolute persistent directory such as `/srv/ora/data`. Neither it nor any parent
  may be writable by group or others. Do not use `/tmp`. Prefer mode `0700` owned by the service account.
- `V2_MARKET_CREDENTIAL_KEY`: persist a stable 32-byte Base64-encoded key in the format required by pluginmarket.
  Back it up securely; replacing or losing it makes existing marketplace AK/SK credentials unreadable.
- `V2_PLUGIN_MARKETPLACE_URL`: the launcher defaults to empty, disabling the official market.
  Direct binary startup retains the development default when unset; explicit empty disables sync
  and visibility of saved official catalog entries. Custom space markets support S3+AKSK and internal HTTPS Git authentication
  (username plus token or password configured in space plugin settings, sealed with `V2_MARKET_CREDENTIAL_KEY`).
- `V2_PLUGIN_MARKETPLACE_BRANCH` and `V2_PLUGIN_MARKETPLACE_INTERVAL`: used only with the official
  source enabled; defaults are main and 5m.
- `V2_MODEL_*` / `V2_ANTHROPIC_MODEL_*`: see the service README for model connections.
  Changing model connections or credential bindings may prevent
  existing conversations from continuing; notify users and start new conversations while retaining history.

The launcher resolves `V2_FRONTEND_DIR` and `V2_NODE_ASSETS_DIR` relative to itself, checks archive
contents and executes the server. Deployment startup must not use cleanup or fixture flags. It
skips development presets, demo preparation and demo warming; this build blocks deployment startup
because it does not provide IDaaS login.

Install enterprise CAs in the host system trust store using your distribution's enterprise process.
Git and Go HTTPS use system CAs. Never disable certificate verification. HTTPS termination belongs
before the Cloud HTTP listener. Verify secure cookies and callback URLs for deployment. Derive
public URLs only from configuration and do not trust forwarding headers. This guide does not
choose a proxy or certificate management solution.

## Backup and upgrade

Back up PostgreSQL, the complete `V2_DATA_DIR` volume and the stable encryption key. Stop the service
or use consistent snapshots, and test restoration. Agent changes exist only in local working trees
and are not pushed to Git; a database-only backup is insufficient.

Stop this instance and confirm its Nodes exited. Back up, unpack a new version into a separate
program directory and verify checksums. This version has no versioned migrations; when migration
is required, use a target version that supports it and follow that version's migration instructions. Replace
programs and resources while retaining database, data directory, keys and host configuration. Do
not reset demos, seed again or overwrite new resources with the old package. Unfinished executions
become interrupted on restart and do not resume automatically. After migration, rollback requires
a verified backup recovery procedure, not merely an older binary. Static files use `no-cache` so
browsers revalidate after upgrades.
