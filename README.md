# Home OS

## Adding a new device

(Assuming device is raspberrypi.local)

```sh
$ ./init.sh <newhostname>
```

## Reboot cluster

```sh
ansible all -m reboot --limit cluster --become
```
