#!/usr/bin/env bash

export $(cat .env)

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
