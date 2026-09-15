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
#   SSH_KEYS_FILE         optional; defaults to fetching SSH_KEYS_URL
#   SSH_KEYS_URL          optional; defaults to https://danhughes.dev/keys
#
# Butane and ignition-validate run in containers, so no local install is
# needed. Both pin the `release` tag; bump deliberately.
set -euo pipefail
cd "$(dirname "$0")"

BUTANE_IMAGE="${BUTANE_IMAGE:-quay.io/coreos/butane:release}"
VALIDATE_IMAGE="${VALIDATE_IMAGE:-quay.io/coreos/ignition-validate:release}"

# envsubst only substitutes the listed names, so `$` sequences inside systemd
# commands stay untouched.
RENDER_VARS='${NODE_NAME} ${NODE_IP} ${SWARM_UNIT_FILE} ${MANAGER_IP} ${NAS_SERVER} ${NAS_EXPORT}'
KEEPALIVED_VARS='${KEEPALIVED_STATE} ${KEEPALIVED_INTERFACE} ${KEEPALIVED_PRIORITY} ${KEEPALIVED_PASSWORD}'
SWARM_VARS='${NODE_IP} ${SWARM_JOIN_TOKEN} ${MANAGER_IP}'

usage() {
    echo "usage: $0 <cl01|cl02|cl03|all>" >&2
    exit 1
}

render_node() {
    local node="$1"
    local env_file="nodes/${node}.env"
    [ -f "$env_file" ] || { echo "unknown node: $node" >&2; exit 1; }

    # Load per-node values (non-secret) and export them for envsubst.
    set -a
    # shellcheck disable=SC1090
    source "$env_file"
    set +a

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

[ $# -eq 1 ] || usage

if [ "$1" = "all" ]; then
    for node in cl01 cl02 cl03; do
        render_node "$node"
    done
else
    render_node "$1"
fi
