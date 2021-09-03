# Home OS

Ansible roles for spinning up new Raspberry Pi's around the house.

A systemd+python service for emitting metrics for a DHT11 chip and system metrics to an InfluxDB server.

It's _very_ specific to me, so I haven't exposed any configuration. 😅

## Adding a new device

```sh
$ ./init.sh # if hostname is raspberrypi
$ ./init.sh <hostname> # if custom hostname
```

## Connecting to K3S

```sh
$ . ./k3s-env.sh
```
