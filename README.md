# Home OS

## Adding a new device

(Assuming device is raspberrypi.local)

```sh
$ ./init.sh <newhostname>
```

## How to

```sh
# Run playbook
ansible-playbook -i inventory.ini playbook.yml

# Reboot cluster
ansible all -m reboot --limit cluster --become

# Poweroff cluster
ansible all -m shell -a 'poweroff' --limit cluster --become
```
