#!/usr/bin/env bash
# Starts one recording environment of the 11-06 demo from a clean state: scripts/demo.sh
# newcomer|lead|ops|all. Each has its own database, data directory and ports (demo/env.go), so the
# environments run side by side and a rerun always starts over. Run it through `task v2:demo[:...]`,
# which checks the model key and starts PostgreSQL first.
set -euo pipefail
export V2_LOGIN=email
cd "$(dirname "$0")/.."
name=${1:?usage: scripts/demo.sh newcomer|lead|ops|all}

# Build once instead of `go run`, so stopping the demo stops the server process itself.
bin=$HOME/ora-demo/bin
mkdir -p "$bin"
go build -o "$bin/v2-server-$name" .
go build -o "$bin/v2-demo-$name" ./demo/cmd/v2-demo
demo=$bin/v2-demo-$name
eval "$("$demo" env "$name")"

# Refuse before deleting anything when a port is taken, e.g. by another checkout's demo.
port_taken() { (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null; }
port_taken "${V2_ADDR##*:}" && { echo "Port ${V2_ADDR##*:} is in use: stop whatever serves it, then rerun." >&2; exit 1; }
port_taken "$DEMO_WEB_PORT" && { echo "Port $DEMO_WEB_PORT is in use: stop it, or set ORA_DEMO_WEB_PORT to a free port." >&2; exit 1; }

"$demo" reset "$name"
V2_SERVER_BIN=$bin/v2-server-$name bash scripts/deepseek.sh &
server=$!
trap 'kill -TERM "$server" 2>/dev/null || true; wait "$server" 2>/dev/null || true' EXIT
# /healthz answers once the schema is applied and the preset accounts exist.
until curl -fsS "http://$V2_ADDR/healthz" >/dev/null 2>&1; do
  kill -0 "$server" 2>/dev/null || { echo "v2-server exited before it was ready." >&2; exit 1; }
  sleep 1
done
"$demo" seed "$name"
echo "Demo $name seeded. Accounts (sign in by email):"
grep -o '"email": "[^"]*"' "$V2_USERS_FILE" | cut -d'"' -f4 | sed 's/^/  /'

cd ../frontend-v2
# The environments share this checkout's node_modules; install it once, never concurrently.
if [[ ! node_modules/.package-lock.json -nt package-lock.json ]]; then
  flock "$bin/npm-ci.lock" bash -c '[[ node_modules/.package-lock.json -nt package-lock.json ]] || npm ci'
fi
V2_SERVER=http://$V2_ADDR VITE_DEMO_MOCK=$DEMO_MOCK npx vite --host 0.0.0.0 --port "$DEMO_WEB_PORT" --strictPort
