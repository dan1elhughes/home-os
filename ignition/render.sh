#!/usr/bin/env bash
# Render and validate the per-node Flatcar Ignition configs.
#
#   ./render.sh cl01        # one node
#   ./render.sh all         # cl01 cl02 cl03
#
# Output lands in out/<node>.bu and out/<node>.ign (git-ignored). Secrets are
# read from the environment and never written anywhere committed:
#
#   KEEPALIVED_PASSWORD   required; goes into keepalived.conf
#   SWARM_JOIN_TOKEN      required for join nodes (cl02/cl03); baked into the
#                         swarm-join unit
#   UPS_NUT_HOST          required; pi-nut's upsd address, host[:port]
#   UPS_NUT_USER/PASS     optional; empty by default (anonymous read, same
#                         access HA's nut integration uses)
#   SSH_KEYS_FILE         optional; defaults to fetching SSH_KEYS_URL
#   SSH_KEYS_URL          optional; defaults to https://danhughes.dev/keys
#
# Butane and ignition-validate run in containers, so no local install is
# needed. Both pin the `release` tag; bump deliberately.
set -euo pipefail
cd "$(dirname "$0")"

# Render secrets resolve through the Homebase 1Password environment -- same
# source deploy.sh uses. Re-exec empties each invocation's env into the exact
# same variables (KEEPALIVED_PASSWORD, SWARM_JOIN_TOKEN, UPS_NUT_USER,
# UPS_NUT_PASS, UPS_NUT_HOST); the marker keeps re-exec loops away.
OP_ENVIRONMENT="q5pfvfpsh44h7xywiqys2ff5ma"
OP_ACCOUNT="BMVXTKJWJNHKDFO36EZROR44HM"

main() {
    if ! command -v op &> /dev/null; then
        echo "Error: 1Password CLI (op) is required but not found." >&2
        exit 1
    fi
    if ! op environment read "$OP_ENVIRONMENT" --account="$OP_ACCOUNT" &>/dev/null; then
        echo "Error: 1Password environment '$OP_ENVIRONMENT' not found or empty." >&2
        echo "Create it in the 1Password desktop app under Developer > Environments." >&2
        exit 1
    fi
    if [ -z "${HOMEOS_OP_ACTIVE:-}" ]; then
        export HOMEOS_OP_ACTIVE=1
        exec op run --environment "$OP_ENVIRONMENT" --account="$OP_ACCOUNT" -- "$0" "$@"
    fi
    # Butane/ignition-validate mount local paths, so they must run against a
    # LOCAL engine — never the swarm context (its daemon would be ssh'd and
    # every bind mount lands empty on the manager). Override allowed.
    export DOCKER_CONTEXT="${RENDER_DOCKER_CONTEXT:-desktop-linux}"
    run "$@"
}

BUTANE_IMAGE="${BUTANE_IMAGE:-quay.io/coreos/butane:release}"
VALIDATE_IMAGE="${VALIDATE_IMAGE:-quay.io/coreos/ignition-validate:release}"

# envsubst only substitutes the listed names, so `$` sequences inside systemd
# commands stay untouched.
RENDER_VARS='${NODE_NAME} ${NODE_IP} ${SWARM_UNIT_FILE} ${MANAGER_IP} ${NAS_SERVER} ${NAS_EXPORT} ${REBOOT_WINDOW_START} ${KEEPALIVED_INTERFACE}'
KEEPALIVED_VARS='${KEEPALIVED_STATE} ${KEEPALIVED_INTERFACE} ${KEEPALIVED_PRIORITY} ${KEEPALIVED_PASSWORD}'
SWARM_VARS='${NODE_IP} ${SWARM_JOIN_TOKEN} ${MANAGER_IP}'
UPS_VARS='${UPS_ROLE} ${UPS_NUT_HOST} ${UPS_NUT_USER} ${UPS_NUT_PASS}'

usage() {
    echo "usage: $0 <cl01|cl02|cl03|all>" >&2
    exit 1
}

render_node() {
    local node="$1"
    local env_file="nodes/${node}.env"
    [ -f "$env_file" ] || { echo "unknown node: $node" >&2; exit 1; }

    # An exported UPS_NUT_HOST (e.g. from 1Password) wins over the default
    # in the env file below; capture it here because sourcing overwrites.
    local ups_host_exported="${UPS_NUT_HOST:-}"

    # Load per-node values (non-secret) and export them for envsubst.
    set -a
    # shellcheck disable=SC1090
    source "$env_file"
    set +a
    : "${UPS_NUT_HOST:=${ups_host_exported:-}}"

    local out="out/${node}"
    local files="${out}/files"
    rm -rf "$out"
    mkdir -p "$files"

    # Public SSH keys are embedded in the config (Ignition runs before any
    # config delivery is possible).
    if [ -n "${SSH_KEYS_FILE:-}" ]; then
        cp "$SSH_KEYS_FILE" "$files/authorized_keys"
    else
        curl -fsSL --max-time 20 "${SSH_KEYS_URL:-https://danhughes.dev/keys}" \
            > "$files/authorized_keys"
    fi

    # keepalived config carries the shared VRRP password.
    : "${KEEPALIVED_PASSWORD:?set KEEPALIVED_PASSWORD to render keepalived.conf}"
    envsubst "$KEEPALIVED_VARS" < files/keepalived.conf.tmpl > "$files/keepalived.conf"

    # The keepalived sysext (pinned .raw) is embedded into the config by butane.
    cp files/keepalived-v2.3.1-x86-64.raw "$files/"

    # Pinned watcher binary + units land beside their SHA-256 pin
    # (files/SHA256SUMS). The wol-on-boot build stays NAS-side; it is only
    # pinned in the repo, not embedded into node configs.
    cp files/ups-watcher-linux-amd64 "$files/"
    cp files/ups-watcher.service files/ups-undrain.service "$files/"

    # NUT endpoint + role: 0600 creds file rendered per node. Verify the
    # artifacts against their pins on every render.
    (cd files && shasum -a 256 -c SHA256SUMS >/dev/null)
    # UPS_NUT_HOST is not a secret: pi-nut's LAN address, default in
    # nodes/<node>.env. An exported value (e.g. from 1Password) wins.
    : "${UPS_NUT_HOST:=${UPS_NUT_HOST_ENV_FILE:-}}"
    [ -n "$UPS_NUT_HOST" ] || { echo "set UPS_NUT_HOST (nodes/<node>.env) to render ups-watcher-creds" >&2; exit 1; }
    case "${UPS_ROLE:-}" in
        peer|survivor) ;;
        *) echo "set UPS_ROLE=peer|survivor to render ups-watcher-creds" >&2; exit 1 ;;
    esac
    {
        printf 'UPS_ROLE=%s\n' "$UPS_ROLE"
        printf 'UPS_HOST=%s\nUPS_NAME=%s\n' "$UPS_NUT_HOST" "ups"
        # Credentials are usually absent: pi-nut allows anonymous reads
        # (the same access the HA integration uses). Render whatever the
        # env provided, so later lockdown only needs the vars filled in.
        printf 'UPS_USER=%s\nUPS_PASS=%s\n' "${UPS_NUT_USER:-}" "${UPS_NUT_PASS:-}"
    } > "$files/ups-watcher-creds"
    chmod 600 "$files/ups-watcher-creds"

    # Swarm unit: init for cl01, join (with the baked manager token) otherwise.
    if [ "$SWARM_UNIT_FILE" = "swarm-join.service" ]; then
        : "${SWARM_JOIN_TOKEN:?set SWARM_JOIN_TOKEN to render a join node}"
    fi
    envsubst "$SWARM_VARS" < "files/${SWARM_UNIT_FILE}.tmpl" > "$files/${SWARM_UNIT_FILE}"

    envsubst "$RENDER_VARS" < common.bu.tmpl > "${out}.bu"

    local abs_files
    abs_files="$(cd "$files" && pwd)"
    docker run --rm -i -v "${abs_files}:/files:ro" "$BUTANE_IMAGE" \
        --pretty --strict --files-dir /files \
        < "${out}.bu" > "${out}.ign"
    docker run --rm -i "$VALIDATE_IMAGE" - < "${out}.ign"

    echo "OK  ${out}.bu -> ${out}.ign"
}

run() {
    [ $# -eq 1 ] || usage

    if [ "$1" = "all" ]; then
        for node in cl01 cl02 cl03; do
            render_node "$node"
        done
    else
        render_node "$1"
    fi
}

main "$@"
