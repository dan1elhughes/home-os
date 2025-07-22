#!/usr/bin/env bash

set -e

echo "Scaling all services to one..."

# Get all service names, excluding the header row
EXCLUDE_LIST="(NAME)"

for service in $(docker service ls | egrep -v $EXCLUDE_LIST | awk '{print $2}'); do
    echo "Scaling service '$service' to 1 replicas..."
    docker service scale "$service=1" --detach=true
done
