#!/usr/bin/env bash
set -euo pipefail

OP_ENVIRONMENT="q5pfvfpsh44h7xywiqys2ff5ma"
OP_ACCOUNT="BMVXTKJWJNHKDFO36EZROR44HM"

# Swarm configs are immutable, so we content-hash any predbat apps.yaml and
# expose the hash to compose via PREDBAT_APPS_HASH. Editing the file changes
# the hash, which rolls out a new config object automatically.
export_predbat_apps_hash() {
    local dir="$1"
    local apps="$dir/predbat/apps.yaml"
    unset PREDBAT_APPS_HASH
    if [ -f "$apps" ]; then
        PREDBAT_APPS_HASH="$(shasum -a 256 "$apps" | cut -c1-12)"
        export PREDBAT_APPS_HASH
    fi
}

# Remove superseded predbat_apps_* configs, keeping the one we just deployed.
# The deploy runs without --detach, so it has already converged and detached
# the previous config by the time we get here. docker config rm safely refuses
# any config still in use.
prune_predbat_configs() {
    local keep="predbat_apps_${PREDBAT_APPS_HASH:-dev}"
    for cfg in $(docker config ls --format '{{.Name}}' | grep '^predbat_apps_' || true); do
        if [ "$cfg" != "$keep" ]; then
            docker config rm "$cfg" >/dev/null 2>&1 && echo "Pruned old config $cfg" || true
        fi
    done
}

# Hash file contents and their relative names in a fixed order. One hash
# versions the entire set so a change to any input rolls both services.
export_dns_config_hash() {
    local dir="$1"
    unset DNS_CONFIG_HASH
    if [ -f "$dir/dns/docker-compose.yml" ] || [ -d "$dir/dns" ]; then
        DNS_CONFIG_HASH="$(
            cd "$dir"
            sha256sum dns/unbound.conf dns/entrypoint.sh dns/generator.py | sha256sum | cut -c1-12
        )"
        export DNS_CONFIG_HASH
    fi
}

# docker config rm refuses to remove configs still attached to a service.
prune_dns_configs() {
    local cfg
    for cfg in $(docker config ls --format '{{.Name}}' | grep '^dns_' || true); do
        case "$cfg" in
            *"_${DNS_CONFIG_HASH}") continue ;;
        esac
        docker config rm "$cfg" >/dev/null 2>&1 && echo "Pruned old config $cfg" || true
    done
}

deploy_stack() {
    local dir="$1"
    if [ -f "$dir/docker-compose.yml" ]; then
        echo "Deploying stack for $dir"
        export_predbat_apps_hash "$dir"
        if [ "${dir%/}" = dns ]; then
            export_dns_config_hash "$dir/.."
        else
            unset DNS_CONFIG_HASH
        fi
        docker stack deploy --compose-file "$dir/docker-compose.yml" "${dir%/}" --detach=false --with-registry-auth
        prune_predbat_configs
        if [ "${dir%/}" = dns ]; then
            prune_dns_configs
        fi
    else
        echo "No docker-compose.yml found in $dir, skipping."
    fi
}

main() {
    if ! command -v op &> /dev/null; then
        echo "Error: 1Password CLI (op) is required but not found."
        echo "Install: brew install --cask 1password-cli@beta"
        echo "Then sign in: op account add"
        exit 1
    fi

    if ! op environment read "$OP_ENVIRONMENT" --account="$OP_ACCOUNT" &>/dev/null; then
        echo "Error: 1Password environment '$OP_ENVIRONMENT' not found."
        echo "Create it in the 1Password desktop app under Developer > Environments."
        exit 1
    fi

    if [ -z "${HOMEBASE_OP_ACTIVE:-}" ]; then
        echo "Starting 1Password session..."
        export HOMEBASE_OP_ACTIVE=1
        exec op run --environment "$OP_ENVIRONMENT" --account="$OP_ACCOUNT" -- "$0" "$@"
    fi

    if [ $# -gt 0 ]; then
        for stack in "$@"; do
            deploy_stack "$stack"
        done
    else
        for dir in */; do
            deploy_stack "$dir"
        done
    fi
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
    main "$@"
fi
