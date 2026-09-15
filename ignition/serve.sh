#!/usr/bin/env bash
# Serve the rendered Ignition configs over HTTP for netboot.xyz PXE delivery.
# A booting node fetches its config via
#   ignition.config.url=http://<this-host>:<port>/<node>.ign
#
#   ./serve.sh [port]          # default 8000
#
# Serves out/ only, and binds to this host's LAN address rather than 0.0.0.0:
# the files contain the VRRP password and the manager join token. Keep it on
# the internal network, and keep the host awake while a node is installing.
# Override the detected address with BIND_IP=<addr>.
set -euo pipefail
cd "$(dirname "$0")"

port="${1:-8000}"

[ -d out ] || { echo "no out/ yet -- run ./render.sh <node> first" >&2; exit 1; }

detect_ip() {
    if command -v ipconfig >/dev/null 2>&1; then
        # macOS: en0 is usually the wired/wifi primary.
        ipconfig getifaddr en0 2>/dev/null || ipconfig getifaddr en1 2>/dev/null || true
    elif command -v hostname >/dev/null 2>&1; then
        hostname -I 2>/dev/null | awk '{print $1}'
    fi
}

ip="${BIND_IP:-$(detect_ip)}"
[ -n "$ip" ] || { echo "could not detect a LAN address; set BIND_IP=<addr>" >&2; exit 1; }

echo "serving $(pwd)/out on http://${ip}:${port}/"
echo "  ignition.config.url=http://${ip}:${port}/<node>.ign"
echo "Ctrl-C to stop"
exec python3 -m http.server "$port" --bind "$ip" --directory out
