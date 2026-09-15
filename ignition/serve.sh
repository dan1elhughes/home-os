#!/usr/bin/env bash
# Serve the rendered Ignition configs over HTTP for netboot.xyz PXE delivery.
# A booting node fetches its config via
#   ignition.config.url=http://<this-host>:<port>/<node>.ign
#
#   ./serve.sh [port]          # default 8000
#
# Serves out/ only. The files carry the VRRP password and the manager join
# token, so run this on the trusted LAN and keep the host awake while a node is
# installing. Listens on 0.0.0.0 by default; override with BIND_IP=<addr>.
set -euo pipefail
cd "$(dirname "$0")"

port="${1:-8000}"
bind="${BIND_IP:-0.0.0.0}"

[ -d out ] || { echo "no out/ yet -- run ./render.sh <node> first" >&2; exit 1; }

# Best-effort LAN address, only to print a usable URL.
detect_ip() {
    if command -v ipconfig >/dev/null 2>&1; then
        # macOS: en0 is usually the wired/wifi primary.
        ipconfig getifaddr en0 2>/dev/null || ipconfig getifaddr en1 2>/dev/null || true
    elif command -v hostname >/dev/null 2>&1; then
        hostname -I 2>/dev/null | awk '{print $1}'
    fi
}

if [ "$bind" = "0.0.0.0" ]; then
    url_ip="$(detect_ip)"
else
    url_ip="$bind"
fi

echo "serving $(pwd)/out on port ${port} (bind ${bind})"
[ -n "${url_ip:-}" ] && echo "  ignition.config.url=http://${url_ip}:${port}/<node>.ign"
echo "Ctrl-C to stop"
exec python3 -m http.server "$port" --bind "$bind" --directory out
