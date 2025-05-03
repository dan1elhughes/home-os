export $(cat .env)

# For each directory, deploy the stack

for dir in $(ls -d */); do
    # Check if the directory contains a docker-compose.yml file
    if [ -f "$dir/docker-compose.yml" ]; then
        echo "Deploying stack for $dir"
        # Deploy the stack using docker stack deploy
        docker stack deploy --compose-file "$dir/docker-compose.yml" "${dir%/}" --detach=true
    else
        echo "No docker-compose.yml found in $dir, skipping."
    fi
done
