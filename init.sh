#/!usr/bin/env bash

defaulthostname="raspberrypi"

hostname=$1
if [ "$hostname" == "" ]; then
    echo "Usage: $0 <hostname>"
    exit 1
fi

# Forget the key from the last time we init'd a Pi.
ssh-keygen -R "$defaulthostname.local"

# Add any SSH key to the device. We'll override this in Ansible later.
ssh-copy-id "pi@$defaulthostname.local"

# Set the hostname, and reboot.
ssh "pi@$defaulthostname.local" bash <<EOF
sudo hostnamectl set-hostname $hostname
sudo sed -i 's/raspberrypi/$hostname/' /etc/hosts
sudo reboot
EOF
