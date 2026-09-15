# Ignition provisioning (Flatcar)

Replaces Ansible for the three swarm nodes. Each node is a Flatcar Container
Linux host with no persistent local data: its Ignition config provisions the
`core` user, mounts the TrueNAS config export at `/mnt/nas`, and joins the
swarm. Config delivery after first boot is out of scope (runbook decision 7).

## Layout

```
common.bu.tmpl              common Butane config, ${PLACEHOLDER} substituted per node
nodes/cl01.env              per-node values (no secrets)
nodes/cl02.env
nodes/cl03.env
files/authorized_keys       embedded public SSH keys
files/keepalived.conf.tmpl  VRRP config template (auth_pass injected at render time)
files/swarm-init.service.tmpl   cl01: docker swarm init
files/swarm-join.service.tmpl   cl02/cl03: docker swarm join with baked token
render.sh                   render -> butane -> ignition-validate (all in Docker)
out/                        generated *.bu / *.ign (git-ignored; hold secrets)
```

## Render

Butane and `ignition-validate` run in containers, so nothing needs installing.
Secrets come from the environment and are never committed:

```sh
cd ignition
KEEPALIVED_PASSWORD='<vrrp pass>' \
SWARM_JOIN_TOKEN='<manager join token>' \
  ./render.sh cl01          # or: all
```

- `KEEPALIVED_PASSWORD` is required for every node.
- `SWARM_JOIN_TOKEN` is required for join nodes (cl02/cl03).
- SSH keys are fetched from `https://danhughes.dev/keys` unless `SSH_KEYS_FILE`
  points at a local file.

`render.sh` writes `out/<node>.bu` and `out/<node>.ign`, then runs
`ignition-validate`. `out/` is git-ignored because the rendered configs contain
the VRRP password and the swarm join token.

To pass a rendered config to Flatcar, use the `.ign` file (for example with
`coreos-installer` or the `flatcar_production_qemu.sh -i` wrapper). The delivery
mechanism itself is out of scope.

## What this replaces

| Old Ansible role | Now in Ignition |
|---|---|
| `common` | `core` SSH keys, passwordless sudo, SSH hardening, weekly reboot timer |
| `docker` | `docker-prune` service+timer (Docker itself ships with Flatcar) |
| `swarm` | `swarm.service` (init or join), Docker `Requires=mnt-nas.mount` drop-in |
| `keepalived` | `/etc/keepalived/keepalived.conf` + a disabled `keepalived.service` |

Dropped with the move to Flatcar: `cockpit`, `sysstat`, `lm-sensors`, apt
upgrades, `loginctl enable-linger`, and the Raspberry Pi `init.sh`.

## Notes and known limits

- **keepalived is disabled and needs a sysext.** Flatcar has no keepalived
  binary. It comes from a community `keepalived` systemd-sysext that is not
  wired up yet; the service is included but `enabled: false`. Add the sysext
  and enable the unit at Phase 3 (cl01) and Phase 4 (followers).
- **`KEEPALIVED_INTERFACE`** defaults to `eth0` in the node env files. Confirm
  the real LAN interface before Phase 3 (the old role used
  `ansible_default_ipv4.interface`).
- **NFS is pinned to 4.0** (`nfsvers=4.0` in `mnt-nas.mount`) because 4.1/4.2
  regress on the Flatcar kernel.
- **The join token is baked in**, so it goes stale if the swarm is
  re-initialised. Re-render and reinstall the follower nodes if that happens.
- **`core` is the login user** and Flatcar's own update reboots may interact
  with the weekly reboot timer; Locksmith may be needed later.

## Local pre-flight (optional)

Boot a real node without touching hardware, using the Flatcar QEMU image and
`--files-dir` for the rendered config:

```sh
./flatcar_production_qemu_uefi.sh -i out/cl01.ign -f 2049:2049 -- -nographic -snapshot
```

`-snapshot` makes every boot a first boot. The mount will not come up unless a
reachable NFS server answers; to test locally, point `NAS_SERVER` at a throwaway
NFS container and use `10.0.2.2` (QEMU's host address). Swarm join, keepalived
and the VIP are best tested on real hardware or multiple VMs.
