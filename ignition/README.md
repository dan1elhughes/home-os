# Ignition provisioning (Flatcar)

Replaces Ansible for the three swarm nodes. Each node is a Flatcar Container
Linux host with no persistent local data: its Ignition config provisions the
`core` user, mounts the TrueNAS config export at `/mnt/nas`, and joins the
swarm. Config delivery is netboot.xyz PXE (see Serve for PXE).

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
serve.sh                    serve out/ over HTTP for netboot.xyz (foreground)
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

## Serve for PXE (netboot.xyz)

Delivery is the netboot.xyz Flatcar menu entry, which boots the PXE kernel with
`ignition.config.url=<url> flatcar.first_boot=1`. Serve the rendered files from
this host, **in its own terminal** — it is a foreground server, so it blocks
until you Ctrl-C it:

```sh
./serve.sh            # or: ./serve.sh <port>   (default 8000)
```

It listens on `0.0.0.0` by default and prints this host's LAN address to use,
e.g. `http://10.10.10.142:8000/cl01.ign`; paste that as the
`ignition.config.url` at the menu prompt. Only `out/` is served, and it holds
the VRRP password and the manager join token, so run it on the trusted LAN and
keep this host awake while a node installs. Override the bind address with
`BIND_IP=<addr>`.

A PXE boot runs Flatcar in RAM. The PXE image includes `flatcar-install`, which
must write the node to disk before it is a real node; see Delivery in the
runbook.

## What this replaces

| Old Ansible role | Now in Ignition |
|---|---|
| `common` | `core` SSH keys, passwordless sudo, SSH hardening, hostname, update/reboot policy |
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
- **Reboots come only from locksmithd, staggered across the managers.**
  `/etc/flatcar/update.conf` sets `REBOOT_STRATEGY=reboot` with a one-hour
  window: cl01 Sunday 02:00, cl02 03:00, cl03 04:00. A node reboots only when
  an update is staged, and only in its slot, so the managers never reboot
  together and the swarm keeps quorum. There is no unconditional weekly reboot
  (the old Ansible cron is gone). Set the slot per node with
  `REBOOT_WINDOW_START` in `nodes/*.env`.
- **`core` is the login user**.
- **Hostname comes from `/etc/hostname`.** Flatcar defaults to `localhost`, so
  Ignition writes `${NODE_NAME}` per node. Without it every swarm node would be
  named `localhost`.

## Local pre-flight (optional)

Boot a real node without touching hardware. `-i` takes the rendered Ignition
file; `-snapshot` makes every boot a first boot and never modifies the image.

```sh
./flatcar_production_qemu_uefi.sh -i out/cl01.ign -p 2222 -- -snapshot
```

**HVF hangs on some Apple Silicon hosts.** The wrapper defaults to
`-machine virt,accel=kvm:hvf:tcg -cpu host`, which hangs at the UEFI banner on
macOS 26 with QEMU 11.1.1 (100% CPU, no kernel output). TCG boots reliably but
is slow:

```sh
qemu-system-aarch64 -M virt,accel=tcg,gic-version=3 -cpu cortex-a57 \
  -m 3072 -display none -serial file:console.log \
  -drive if=pflash,unit=0,file=flatcar_production_qemu_uefi_efi_code.qcow2,format=qcow2,readonly=on \
  -drive if=pflash,unit=1,file=flatcar_production_qemu_uefi_efi_vars.qcow2,format=qcow2 \
  -drive if=none,id=blk,file=flatcar_production_qemu_uefi_image.img \
  -device virtio-blk-pci,drive=blk,bootindex=1 \
  -netdev user,id=eth0,hostfwd=tcp::2222-:22 -device virtio-net-pci,netdev=eth0 \
  -fw_cfg name=opt/org.flatcar-linux/config,file=out/cl01.ign -snapshot
```

The guest reaches the host at `10.0.2.2` (QEMU's host address), so a local NFS
server must listen there. The wrapper's `-f` forwards host->guest, not
guest->host.

**The mount cannot be tested on Docker Desktop.** Its kernel refuses nfsd in a
container network namespace (`rpc.nfsd: writing fd to kernel failed: errno 111`,
`does not support NFS export`), so no containerised NFS server works. Prove the
mount at Phase 0/1 against the real TrueNAS export, or with a host-level nfsd.
Everything else is testable: user, SSH keys and hardening, `update.conf`, the
units/timers, and Docker's `Requires=mnt-nas.mount` holding dockerd back while
the export is missing.

Swarm join, keepalived and the VIP are best tested on real hardware or multiple
VMs.
