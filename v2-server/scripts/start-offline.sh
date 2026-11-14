#!/usr/bin/env bash
set -euo pipefail
root=$(cd "$(dirname "$0")" && pwd -P)
: "${V2_LOGIN:?Set V2_LOGIN=idaas; IDaaS integration is required}"
[[ $V2_LOGIN == idaas ]] || { echo 'The offline deployment launcher requires V2_LOGIN=idaas.' >&2; exit 1; }
: "${V2_DATABASE_URL:?Set V2_DATABASE_URL}"
: "${V2_DATA_DIR:?Set a persistent, private V2_DATA_DIR}"
: "${V2_MARKET_CREDENTIAL_KEY:?Set the stable V2_MARKET_CREDENTIAL_KEY}"
export V2_NODE_ASSETS_DIR="$root/assets" V2_FRONTEND_DIR="$root/web"
# Unset defaults to offline-safe disabled; an explicit internal source may be configured.
export V2_PLUGIN_MARKETPLACE_URL=${V2_PLUGIN_MARKETPLACE_URL-}
(cd "$root" && sha256sum --check --status SHA256SUMS)
exec "$root/bin/cloud" "$@"
