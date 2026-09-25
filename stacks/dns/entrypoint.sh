#!/bin/sh
set -eu

runtime=${DNS_RUNTIME_DIR:-/opt/unbound/etc/unbound/runtime}
generated=${DNS_GENERATED_FILE:-/opt/unbound/etc/unbound/generated/hosts.conf}
config=${DNS_CONFIG_FILE:-/opt/unbound/etc/unbound/unbound.conf}
server=${DNS_UNBOUND_SCRIPT:-/unbound.sh}
interval=${DNS_POLL_INTERVAL:-5}
startup_attempts=${DNS_STARTUP_ATTEMPTS:-15}
startup_interval=${DNS_STARTUP_INTERVAL:-2}

case "${NEXTDNS_PROFILE_ID:-}" in
    ''|*[!a-zA-Z0-9]*) echo 'Invalid NextDNS profile ID' >&2; exit 1 ;;
esac
[ "${#NEXTDNS_PROFILE_ID}" -eq 6 ] || { echo 'Invalid NextDNS profile ID' >&2; exit 1; }
case "$startup_attempts" in
    ''|*[!0-9]*) echo 'Invalid DNS startup attempts' >&2; exit 1 ;;
esac
if ! [ "$startup_attempts" -gt 0 ] 2>/dev/null; then
    echo 'Invalid DNS startup attempts' >&2
    exit 1
fi

umask 022
mkdir -p "$runtime"
chmod 755 "$runtime"
cat > "$runtime/forward.conf" <<EOF
forward-zone:
    name: "."
    forward-tls-upstream: yes
    forward-first: no
    forward-addr: 45.90.28.0@853#${NEXTDNS_PROFILE_ID}.dns.nextdns.io
    forward-addr: 45.90.30.0@853#${NEXTDNS_PROFILE_ID}.dns.nextdns.io
EOF
chmod 644 "$runtime/forward.conf"

last_hash=
has_home_record() {
    grep -Fqx 'local-data: "home.danhughes.dev. 60 IN A 10.10.10.20"' "$runtime/hosts.conf"
}
apply_generated() {
    [ -f "$generated" ] || return 0
    hash=$(sha256sum "$generated" | cut -d ' ' -f 1) || return 0
    [ "$hash" != "$last_hash" ] || return 0
    [ -s "$generated" ] || { last_hash=$hash; echo 'Ignored empty DNS records' >&2; return 0; }
    if cmp -s "$generated" "$runtime/hosts.conf"; then
        last_hash=$hash
        return 0
    fi
    cp "$runtime/hosts.conf" "$runtime/hosts.conf.previous"
    cp "$generated" "$runtime/hosts.conf"
    chmod 644 "$runtime/hosts.conf"
    if ! has_home_record || ! unbound-checkconf "$config" >/dev/null 2>&1; then
        mv "$runtime/hosts.conf.previous" "$runtime/hosts.conf"
        last_hash=$hash
        echo 'Ignored invalid DNS records' >&2
        return 0
    fi
    if [ "$started" = yes ]; then
        if ! unbound-control -c "$config" reload_keep_cache; then
            mv "$runtime/hosts.conf.previous" "$runtime/hosts.conf"
            echo 'DNS reload failed; will retry' >&2
            return 0
        fi
    fi
    rm "$runtime/hosts.conf.previous"
    last_hash=$hash
}

started=no
# The image normally creates this file in /unbound.sh. Create it before
# checkconf so the full configuration can be checked before the server starts.
if command -v unbound-anchor >/dev/null 2>&1; then
    anchor=/opt/unbound/etc/unbound/var/root.key
    mkdir -p /opt/unbound/etc/unbound/var
    chown _unbound:_unbound /opt/unbound/etc/unbound/var
    unbound-anchor -a "$anchor" || :
    [ -s "$anchor" ] || { echo 'DNS trust anchor is missing' >&2; exit 1; }
    chown _unbound:_unbound "$anchor"
fi
attempt=1
while [ "$attempt" -le "$startup_attempts" ]; do
    if [ -s "$generated" ] && cp "$generated" "$runtime/hosts.conf"; then
        chmod 644 "$runtime/hosts.conf"
        if has_home_record && unbound-checkconf "$config" >/dev/null 2>&1; then
            break
        fi
    fi
    if [ "$attempt" -eq "$startup_attempts" ]; then
        echo "DNS records missing, empty, or invalid after $startup_attempts attempts" >&2
        exit 1
    fi
    attempt=$((attempt + 1))
    sleep "$startup_interval"
done
"$server" &
server_pid=$!
started=yes

stop() {
    kill -TERM "$server_pid" 2>/dev/null || :
    wait "$server_pid" 2>/dev/null || :
    exit 0
}
trap stop TERM INT

while kill -0 "$server_pid" 2>/dev/null; do
    apply_generated
    sleep "$interval" &
    wait $! || :
done
wait "$server_pid"
