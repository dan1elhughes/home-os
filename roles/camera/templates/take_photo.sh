#!/usr/bin/env bash
# {{ ansible_managed }}

server="dan@100.77.199.70"

now=$(date +"%Y-%m-%d.%H-%M-%S")
output="/home/dan/Timelapse/input/$now.jpg"

# Define photo options.
addTimestamp="-a 12" # Embed timestamp at the top
exposure="-ex verylong" # Set a fixed exposure to prevent AWB flicker
whitebalance="-awb sun" # Same but for white balance
prewait="-t 1000" # Wait 1 second before capture

options="$addTimestamp $exposure $whitebalance $prewait"

# Pipe immediately to server without touching local disk.
raspistill $options -o - | ssh $server "cat > $output"
