#!/bin/sh
set -eu

conf_dir=${DNS_ADGUARD_CONF_DIR:-/opt/adguardhome/conf}
work_dir=${DNS_ADGUARD_WORK_DIR:-/opt/adguardhome/work}
config=${DNS_ADGUARD_CONFIG:-/run/dns/AdGuardHome.yaml}
binary=${DNS_ADGUARD_BINARY:-/opt/adguardhome/AdGuardHome}

# Swarm configs mount read-only, but AdGuard Home rewrites its yaml (schema
# migrations, filter metadata). Seed a writable copy at startup, then run.
umask 022
mkdir -p "$conf_dir" "$work_dir"
cp "$config" "$conf_dir/AdGuardHome.yaml"
exec "$binary" -c "$conf_dir/AdGuardHome.yaml" -w "$work_dir" --no-check-update
