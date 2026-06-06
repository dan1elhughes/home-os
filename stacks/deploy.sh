#!/usr/bin/env bash
set -euo pipefail

OP_ENVIRONMENT="q5pfvfpsh44h7xywiqys2ff5ma"
OP_ACCOUNT="BMVXTKJWJNHKDFO36EZROR44HM"

# Check op CLI is available
if ! command -v op &> /dev/null; then
    echo "Error: 1Password CLI (op) is required but not found."
    echo "Install: brew install --cask 1password-cli@beta"
    echo "Then sign in: op account add"
    exit 1
fi

# Check the environment exists
if ! op environment read "$OP_ENVIRONMENT" --account="$OP_ACCOUNT" &>/dev/null; then
    echo "Error: 1Password environment '$OP_ENVIRONMENT' not found."
    echo "Create it in the 1Password desktop app under Developer > Environments."
    exit 1
fi

# If we're not already inside an op run, re-execute wrapped
if [ -z "${HOMEBASE_OP_ACTIVE:-}" ]; then
    echo "Starting 1Password session..."
    export HOMEBASE_OP_ACTIVE=1
    exec op run --environment "$OP_ENVIRONMENT" --account="$OP_ACCOUNT" -- "$0" "$@"
fi

deploy_stack() {
    local dir="$1"
    if [ -f "$dir/docker-compose.yml" ]; then
        echo "Deploying stack for $dir"
        docker stack deploy --compose-file "$dir/docker-compose.yml" "${dir%/}" --detach=true --with-registry-auth
    else
        echo "No docker-compose.yml found in $dir, skipping."
    fi
}

# If CLI arguments are passed, deploy each one
if [ $# -gt 0 ]; then
    for stack in "$@"; do
        deploy_stack "$stack"
    done
else
    # For each directory, deploy the stack
    for dir in $(ls -d */); do
        deploy_stack "$dir"
    done
fi
