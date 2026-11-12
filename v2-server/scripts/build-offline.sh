#!/usr/bin/env bash
set -euo pipefail
# Build on the target Linux architecture, with network access. No installation step downloads.
root=$(cd "$(dirname "$0")/../.." && pwd -P)
: "${V2_OFFLINE_OUTPUT:?Set V2_OFFLINE_OUTPUT to a new archive path}"
[[ ! -e $V2_OFFLINE_OUTPUT ]] || { echo 'Output already exists.' >&2; exit 1; }
[[ $(node -p 'Number(process.versions.node.split(".")[0])') -ge 24 ]] || { echo 'Node >=24 required.' >&2; exit 1; }
export GOMAXPROCS=2 CARGO_BUILD_JOBS=2 V2_NODE_PROFILE=release
bash "$root/v2-server/scripts/prepare-node.sh"
assets=${V2_NODE_ASSETS_DIR:-$HOME/.cache/ora-v2-node}
stage=$(mktemp -d)
trap 'rm -rf -- "$stage"' EXIT
mkdir -p "$stage/cloud/bin" "$stage/cloud/assets" "$stage/cloud/web" "$stage/cloud/docs"
(cd "$root/v2-server" && go build -p 2 -trimpath -o "$stage/cloud/bin/cloud" .)
# Use a separate mode and explicitly override the mock flag from every Vite env file.
# Shared .env files may still be read by Vite; none are copied into the package.
(cd "$root/frontend-v2" && npm ci && VITE_DEMO_MOCK=0 npm run build -- --mode offline)
cp -R "$root/frontend-v2/dist/." "$stage/cloud/web/"
python3 "$root/v2-server/scripts/package-offline.py" "$assets" "$stage/cloud/assets"
cp "$root/v2-server/scripts/start-offline.sh" "$stage/cloud/start.sh"
chmod 755 "$stage/cloud/start.sh"
cp "$root/v2-server/docs/deployment.md" "$root/v2-server/docs/deployment.en.md" "$stage/cloud/docs/"
(cd "$stage/cloud" && find . -type f ! -name SHA256SUMS -print0 | sort -z | xargs -0 sha256sum > SHA256SUMS)
tar -czf "$V2_OFFLINE_OUTPUT" -C "$stage" cloud
echo 'Offline package built. This build does not provide IDaaS login or versioned migrations; HTTPS is required for deployment; see docs/deployment.md.'
