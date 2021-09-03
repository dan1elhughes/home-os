#!/usr/bin/env bash
# {{ ansible_managed }}

set -f

response=`curl https://healthchecks.io/api/v1/checks/ \
    -sSL \
    --header "X-Api-Key: {{ healthcheck_api_key }}" \
    --data '{"name": "{{ ansible_hostname }}", "grace": 120, "channels": "*", "schedule": "* * * * *", "unique": ["name"]}'`

ping_url=`echo $response | jq -r ".ping_url"`

curl -sSL "$ping_url"
