export $(cat .env)

deploy_stack() {
    local dir="$1"
    if [ -f "$dir/docker-compose.yml" ]; then
        echo "Deploying stack for $dir"
        docker stack deploy --compose-file "$dir/docker-compose.yml" "${dir%/}" --detach=true
    else
        echo "No docker-compose.yml found in $dir, skipping."
    fi
}

# Check if a CLI argument is passed
if [ "$1" ]; then
    deploy_stack "$1"
else
    # For each directory, deploy the stack
    for dir in $(ls -d */); do
        deploy_stack "$dir"
    done
fi
