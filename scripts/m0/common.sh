# Shared settings for the M0 spike scripts. Override by exporting.
WH_BUS="${WH_BUS:-nats://bus.example.internal:4222}"
WH_ME="${WH_ME:-alice}"
WH_PEER="${WH_PEER:-bob}"
WH_PASSWORD_FILE="${WH_PASSWORD_FILE:-$HOME/.config/wormhole/password}"
WH_WORKDIR="${WH_WORKDIR:-$HOME/wormhole}"

need() { command -v "$1" >/dev/null 2>&1 || { echo "missing: $1" >&2; exit 1; }; }
note() { printf '\n== %s\n' "$*"; }
pw() { tr -d '\n' < "$WH_PASSWORD_FILE"; }
