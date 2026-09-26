# Flatcar migration: TrueNAS storage and Ignition provisioning

Status: source of truth — sequential migration runbook
Date: 2026-09-15, revised 2026-09-24 (storage-first, one swarm, node-by-node),
revised 2026-09-25 (new dns stack from master, VIP-bound records),
revised 2026-09-26 (rebase onto master: resolver pinning moves to Ignition)

This is the single document for the migration. Read it top to bottom. Section 1
is why, section 2 is the target state, section 3 is optional local testing,
section 4 is the ordered sequence you execute, and sections 5–6 are risks and
open items.

## 1. Context and decisions

The three swarm nodes (cl01/cl02/cl03) are moving to **Flatcar Container Linux**.
Flatcar is immutable: read-only `/usr`, no package manager, no snapd, and no
Python. So MicroCeph (a snap) cannot run, `/mnt/cephfs` disappears, the Docker
boot dependency `Requires=mnt-cephfs.mount` cannot be satisfied, and the
`apt`-based Ansible roles cannot run.

These are the **existing hosts, reformatted in place**. The nodes keep their
names and addresses, so there is no new hardware and no old-node cleanup beyond
the reinstall itself.

Decisions already made:

1. **NAS = the existing TrueNAS SCALE box** (`10.10.10.60`), not a new appliance.
2. **Databases become remote servers on TrueNAS** (one custom app, SSD pool).
   Apps connect over TCP, so no Postgres/MariaDB data dirs on NFS.
3. **App config lives on a TrueNAS NFS export**, host-mounted at `/mnt/nas` on
   every node, replacing `/mnt/cephfs`. Single-replica apps; SQLite-on-network-FS
   risk accepted.
4. **One dataset, one export**, host-mounted via Ignition. Docker NFS volumes and
   iSCSI rejected.
5. **The export `mapall`s all users to `1000:1000`** (matching today's CephFS
   ownership of every app-config dir, so the rsync copies land correctly) and is
   reachable from **any host**, so configs can be edited from a workstation.
6. **Media/Immich blob exports stay as they are** (Docker `type: nfs` volumes).
7. **Ansible is retired entirely** — playbook, inventory, roles, `init.sh`
   deleted; Ignition owns provisioning. Config delivery is netboot.xyz PXE via
   `ignition.config.url` (see Delivery in section 4).
8. **Node login user is Flatcar's `core`** — SSH key, passwordless sudo, docker
   group. No bespoke user.
9. **Swarm membership is set in Ignition**, manager join token baked into the
   follower configs.
10. **DB credentials unchanged** — no churn during migration.
11. **Snapshots and backups are out of scope.**
12. **The `home-assistant` repo's recorder change is in scope** with this work.
13. **One swarm throughout.** There is no swarm rebuild and no second docker
    context. Storage migrates first, on the live swarm, while all three nodes
    still run the old OS; each node is then reformatted to Flatcar one at a
    time and rejoins the **same** swarm. Every node — old OS and Flatcar alike —
    mounts the same export at `/mnt/nas`, so services are indifferent to which
    node they run on. (The earlier 0 → 1 → 3 rebuild-on-cl01 shape is dropped.)
14. **The three Flatcar nodes are the existing nodes, reformatted in place** —
    same names and addresses, no new hardware. There is no old-node cleanup
    step: reinstalling wipes the old OS (and its MicroCeph data) on each node.
15. **Old nodes mount `/mnt/nas` via an idempotent systemd mount unit applied
    over ssh** — the same unit definition the Ignition template renders, with
    the same `nfsvers=4.0` pin. No Ansible resurrection (fstab and a one-off
    playbook were considered and rejected).
16. **keepalived stays.** Flatcar nodes get the official **sysext-bakery
    keepalived extension** (statically compiled, fetched by Ignition, version
    pinned — no `systemd-sysupdate` auto-updates, so an extension update cannot
    request a reboot outside the locksmithd windows). `keepalived.service` is
    enabled before cl01's swap. Old nodes hold the VIP until the final node
    swap; the IP lands on Flatcar then, and DNS never changes.
17. **mcpjungle is a fifth database.** Its Postgres data dir lives on CephFS
    today and cannot live on NFS. It joins the TrueNAS database app as
    `mcpjungle-postgres` (port 5435, dataset `/mnt/SSD/db/mcpjungle`).
18. **renovate stays.** It is stateless (no volumes, no DB), scheduled by
    `swarm-cronjob` — its cutover is a no-op redeploy.
19. **Host and container resolvers stay pinned to the DNS VIP** (`10.10.10.20`).
    Master landed this for the old OS on 2026-09-26 via the Ansible common and
    docker roles (a netplan resolver file with `dhcp4-overrides.use-dns: false`,
    and `/etc/docker/daemon.json` `"dns"`). Those roles are deleted with the
    migration, so Ignition takes the job over on Flatcar — a systemd-networkd
    DNS unit and the same `daemon.json`, both in the common fragment.

## 2. Target state (reference)

- Flatcar nodes hold **no persistent data**.
- TrueNAS is the only persistent layer: runs the five databases, exports the
  config dataset, and continues to export the media/Immich blobs.
- The swarm keeps serving throughout. Each stack freezes only for its own
  cutover window; nothing moves between swarms.

**Storage inventory today** (for reference while migrating):

| Tier | Mechanism | Where | Holds |
|---|---|---|---|
| A | `/mnt/cephfs/<dir>` bind mount | MicroCeph | App config, all DBs, SQLite state — replaced in Phase 2 |
| B | `local` driver + `type: nfs` volume | TrueNAS | Media, downloads, Immich blobs |
| C | none | — | Proxies, redis, ephemeral |

**TrueNAS config dataset:** `/mnt/SSD/cluster`, exported as one NFS export and
mounted at `/mnt/nas`. Subdirectories are today's CephFS names, minus the DB
dirs and `immich-ml-cache`:

`homeassistant`, `predbat`, `mosquitto`, `tasmoadmin-config`, `trmnl`,
`gitea-config`, `uptime-kuma`, `autokuma-data`, `traefik-config`,
`traefik-config-letsencrypt`, `transmission`, `prowlarr-config`,
`radarr-config`, `sonarr-config`, `lidarr-config`, `swiparr`, `apprise-config`,
`apprise-attachments`, `sponsorblock`, `mcpjungle-conf`, `dns`.

**TrueNAS database app** (data on SSD, same image tags as today):

| Container | Image | Data dataset | Host port |
|---|---|---|---|
| `homeassistant-postgres` | `postgres:18-alpine` | `/mnt/SSD/db/homeassistant` | 5432 |
| `immich-postgres` | `ghcr.io/immich-app/postgres:18-vectorchord0.5.3` | `/mnt/SSD/db/immich` | 5433 |
| `gitea-postgres` | `postgres:18.6` | `/mnt/SSD/db/gitea` | 5434 |
| `kuma-mariadb` | `mariadb:12.3` | `/mnt/SSD/db/kuma` | 3306 |
| `mcpjungle-postgres` | `postgres:18.6` | `/mnt/SSD/db/mcpjungle` | 5435 |

**Per-node Ignition contents** (rendered from a common fragment + per-node
values, transpiled with `butane --pretty --strict`):

- Common: `core` user with embedded SSH keys, passwordless sudo, docker group;
  hostname (`/etc/hostname`); SSH hardening; `mnt-nas.mount`; Docker drop-in
  `10-nas.conf`; docker-prune service+timer; `rpc-statd`; a systemd-networkd
  unit pinning host DNS to the VIP; `/etc/docker/daemon.json` with
  `"dns"` = the VIP (see Resolver pinning below).
- All nodes: the **sysext-bakery keepalived extension** (pinned, no sysupdate)
  and the `keepalived.service` unit; all three run `swarm-join.service` with
  the baked manager token — **including cl01**. The swarm is never
  re-initialised (decision 13): a cl01 `swarm init` would fork a second
  cluster with an empty raft while cl02/cl03 still hold the real one, and the
  second swap would either strand the old swarm at quorum 1/3 or leave the new
  one with nothing deployed. `swarm-init.service` is kept in the repo only for
  a genuine from-scratch rebuild. The VRRP parameters must
  match the live cluster so Flatcar nodes join the same election: instance
  `cluster`, interface `enp1s0`, `virtual_router_id 51`, `auth_type PASS` with
  the existing `auth_pass`, VIP `10.10.10.20/32`. Until the final swap Flatcar
  nodes run `state BACKUP` with **priority 50** (below the old nodes' 100
  leader and 90 followers), so they only take the VIP if an old MASTER dies —
  never by preemption.

**Compose path mapping:** every `/mnt/cephfs/<x>` → `/mnt/nas/<x>`. DB services
and their `depends_on` entries are removed; DB hosts become `10.10.10.60`.

**Cluster DNS:** the `dns` stack generates `*.danhughes.dev` records from
Traefik's routers, all pointing at the VIP `10.10.10.20`. Records are bound to
the VIP, not to any node, so when the VIP moves to a Flatcar node at the end of
Phase 3, DNS follows with no change.

**Resolver pinning (from master, implemented in the Ignition common fragment).**
The dns-stack work on master made the VIP the sole resolver, host and container
side: hosts pin `10.10.10.20` with DHCP's DNS ignored, so a lease renewal cannot
reintroduce the router's resolver (it cannot resolve `*.danhughes.dev`), and
containers get `/etc/docker/daemon.json` `"dns": ["10.10.10.20"]` — dockerd
snapshots its upstreams at start and the systemd-resolved stub never changes.
On the old OS this is Ansible, and those roles are deleted here, so the common
fragment carries both halves. Flatcar has no netplan: `/etc/resolv.conf` is a
symlink into systemd-resolved, whose servers come from networkd. The host pin
is `10-cluster.network` — a full `.network` unit for `enp1s0` with `DHCP=yes`,
`DNS=10.10.10.20`, `[DHCPv4] UseDNS=false`, written by Ignition before networkd
starts. networkd applies the first lexically-matching unit and ignores later
matches, so a unit named `10-` wins over anything the platform ships; because
the shipped file is then ignored for this interface, the unit must carry the
DHCP config itself. The container pin is the same `daemon.json` as the old OS,
written with `overwrite`. Both name the keepalived VIP; move them together if
it ever changes.

## 3. Local testing (optional pre-flight)

Most of the Ignition config and the database app can be validated on a laptop
before touching real hardware. Docker supplies the tooling; QEMU boots a real
Flatcar node. Docker cannot run Flatcar itself — it is a full OS with systemd as
PID 1.

**Context safety.** The local daemon and the cluster are different docker
contexts (e.g. `desktop-linux` vs `swarm`). Never rely on the default: pass
`--context <name>` or set `DOCKER_CONTEXT=<name>` on every command. A local
validation run sent to a swarm context executes on a live node, and a
`deploy.sh` run against the swarm with unmigrated paths breaks live services.

**Transpile and validate (Docker, seconds):**

```sh
docker run --rm -i quay.io/coreos/butane:release --pretty --strict < cl01.bu > cl01.ign
docker run --pull=always --rm -i quay.io/coreos/ignition-validate:release - < cl01.ign
```

Butane catches YAML/schema errors; `ignition-validate` catches bad Ignition. Run
this every time the Butane template changes.

**Boot a real node (QEMU):** Flatcar ships QEMU images and a wrapper script. On
Apple Silicon use the `arm64` image.

```sh
./flatcar_production_qemu_uefi.sh -i cl01.ign -f 2049:2049 -- -nographic -snapshot
```

`-snapshot` makes every boot a first boot, so the config can be iterated without
restoring the image. Log in on port 2222 (or the serial console) and check:

- `findmnt /mnt/nas` and `systemctl status mnt-nas.mount`
- `systemctl show docker -p Requires` includes `mnt-nas.mount`
- `resolvectl dns enp1s0` shows only `10.10.10.20` (no DHCP-learned server);
  `networkctl status enp1s0` shows the lease came from `10-cluster.network`
  (the unit that replaced the shipped one); a container's `/etc/resolv.conf`
  names the VIP. The QEMU guest reaches the host at `10.0.2.2`, so this proves
  the pin structurally — end-to-end resolution through the real VIP is a
  Phase 3 per-node check.
- `docker node ls` shows one manager
- `systemctl status docker-prune.timer locksmithd.service`
- `ls /mnt/nas`
- `keepalived --version` resolves once the sysext is added; the unit starts and
  stays `BACKUP`

**NFS export (Docker):** run an NFS server container with a test export, and
point a throwaway copy of the config's `What=` at `10.0.2.2` (QEMU's host
address) instead of `10.10.10.60`. Because the mount pins `nfsvers=4`, only port
2049 is needed, so `-f 2049:2049` works. A generic NFS container only
approximates TrueNAS — fake ownership with `all_squash,anonuid,anongid`; it will
not prove TrueNAS's `mapall` behaves identically.

**Database app (Docker Compose):** run the five-container DB app as an ordinary
compose project locally to validate images, environment, ports and dataset
mounts before deploying to TrueNAS. The Immich Postgres image is multi-arch.

**Limits:** the keepalived sysext and the VIP are best tested on real hardware
or multiple QEMU VMs. Swarm join is exercised for real in Phase 3 — the token
comes from the live swarm, so there is nothing to fake.

**Findings from the first local run (2026-09-15):**

- The wrapper's default HVF acceleration hangs at the UEFI banner on macOS 26
  with QEMU 11.1.1 (100% CPU, no kernel output). Boot under TCG instead
  (`-machine virt,accel=tcg,gic-version=3 -cpu cortex-a57`).
- A containerised NFS server cannot work on Docker Desktop: its kernel refuses
  nfsd in a container netns (`rpc.nfsd: errno 111`, `does not support NFS
  export`). The mount cannot be tested locally this way, so it is verified
  against the real TrueNAS export in Phase 1.
- Everything else is testable. The first run confirmed Flatcar 4757.2.0 applies
  Ignition (core user and SSH keys, passwordless sudo, SSH hardening,
  `update.conf`, the units and timers) and that Docker's
  `Requires=mnt-nas.mount` correctly holds dockerd back when the export is
  missing.
- A second run with the Docker drop-in stripped from the rendered config (test
  only) confirmed the rest: `docker.service` active, `swarm.service` active with
  one manager (`docker node ls`), the staggered locksmithd reboot window, and a
  working container. That run also exposed that Flatcar defaults the hostname to
  `localhost`, so Ignition now writes `/etc/hostname` from `NODE_NAME`.

## 4. The sequence

One docker context is used throughout — the usual `swarm` context. Set it once
and never fall back to the local daemon by accident:

```sh
export DOCKER_CONTEXT=swarm
```

The sequence is **storage first, then one node at a time**: TrueNAS prep,
mount `/mnt/nas` everywhere, cut every stack over in place, then reformat
cl01 → cl02 → cl03 with the swarm never rebuilt.

### Delivery (netboot.xyz)

Used in Phase 3, per node. Boot each node from the netboot.xyz Flatcar menu
entry. It prompts for the Ignition URL and then boots the PXE kernel with
`ignition.config.url=<url> flatcar.first_boot=1` (plus
`flatcar.autologin=tty1/ttyS0`). `flatcar.first_boot=1` is what makes Ignition
run; the menu's `ignition_config` entry sets it.

netboot.xyz itself runs on TrueNAS at `10.10.10.60:31010`, and the custom
Flatcar menu lives there. The `netboot` stack in this repo is only a Caddy proxy
that exposes it at `netboot.danhughes.dev` through Traefik on the swarm. Do not
use that hostname for PXE: firmware has no internal DNS. Read the boot chain
from `10.10.10.60:31010` directly.

**Node prerequisites.** The node must boot UEFI, because UniFi hands out
`netboot.xyz.efi`. Secure Boot must be **off** (that binary is unsigned), and
the NIC needs a driver bundled in iPXE. A legacy-BIOS node would need
`netboot.xyz-undionly.kpxe` as the UniFi bootfile instead.

Serve the rendered configs from the workstation with `ignition/serve.sh`, run in
its own terminal (it is a foreground server and blocks until Ctrl-C). It serves
`ignition/out/` and prints the URL to paste at the prompt (for example
`http://10.10.10.142:8000/cl01.ign`). Keep it running for the whole install; the
booting node fetches its config from it. It listens on all interfaces, which is
fine on the trusted LAN, but the files carry the VRRP password and the manager
join token, so do not leave it running on an untrusted network.

**Per node:**

1. At the Flatcar menu choose **`ignition_config`** and paste
   `http://10.10.10.142:8000/<node>.ign`. That entry is what sets
   `flatcar.first_boot=1`; choosing a channel directly does not, and Ignition
   will not run.
2. Choose **stable** (or beta/alpha). Do **not** choose **edge** — Flatcar has no
   edge channel, so it fails. Either use stable, or add an `lts` entry to
   `flatcar.ipxe` on the NAS.
3. The node boots in RAM and auto-logs-in as `core`. **A PXE boot does not
   install to disk**, so install and reboot:

   ```sh
   curl -o /tmp/<node>.ign http://10.10.10.142:8000/<node>.ign
   sudo flatcar-install -d /dev/<disk> -i /tmp/<node>.ign
   sudo reboot
   ```

   `flatcar-install` is in the PXE image and installs the same channel and
   version that was PXE-booted by default.

4. The node's real Ignition applies on the first **disk** boot. Skipping step 3
   leaves the node diskless: docker and the swarm start in RAM and reset on every
   reboot.

### Phase 0 — TrueNAS prep

1. Create dataset `/mnt/SSD/cluster` on the SSD pool; owner `1000:1000`.
2. Create datasets `/mnt/SSD/db/{immich,homeassistant,gitea,kuma,mcpjungle}`.
3. Create the NFS export for `/mnt/SSD/cluster`: allowed hosts `0.0.0.0/0`, `rw`,
   `sec=sys`, `nfsvers=4`, `mapall` → `1000:1000`.
4. Deploy the database custom app: the five containers, host-path mounts to the
   `db` datasets, ports bound on `10.10.10.60`, firewall restricted to
   `10.10.10.21-23`. The compose app is `truenas-databases/`. Three Postgres
   instances cannot share one host IP and port, so each gets its own: HA 5432,
   immich 5433, gitea 5434, mcpjungle 5435, kuma 3306.

**Verify:**
- `showmount -e 10.10.10.60` lists `/mnt/SSD/cluster`.
- From a node that can reach it, the export mounts read/write and a test file
  created from two different hosts is owned by `1000:1000`.
- `nc -z 10.10.10.60 5432` (and 3306, 5433–5435) succeeds from the swarm subnet
  only.

### Phase 1 — Mount `/mnt/nas` on the old nodes

Per node (cl01, cl02, cl03), over ssh as the node user:

1. Write `/etc/systemd/system/mnt-nas.mount` — the same definition the Ignition
   template renders, options included:

   ```ini
   [Unit]
   Description=TrueNAS config export at /mnt/nas
   After=network-online.target
   Wants=network-online.target

   [Mount]
   What=10.10.10.60:/mnt/SSD/cluster
   Where=/mnt/nas
   Type=nfs
   Options=nfsvers=4.0,_netdev,rw,noatime,hard,rsize=1048576,wsize=1048576

   [Install]
   WantedBy=multi-user.target
   ```

   NFS stays pinned to `4.0` here too — the 4.1/4.2 kernel regression is not
   specific to Flatcar.

2. Write the Docker drop-in `/etc/systemd/system/docker.service.d/10-nas.conf`.
   The existing cephfs requirement ships at
   `/usr/lib/systemd/system/docker.service.d/after-mount.conf` (check the node
   if in doubt); it stays as-is until the node is reformatted, so docker boot
   still needs both mounts during the interim:

   ```ini
   [Unit]
   Requires=mnt-nas.mount
   After=mnt-nas.mount
   ```

3. `sudo systemctl daemon-reload && sudo systemctl enable --now mnt-nas.mount`.

**Verify (each node):**
- `findmnt /mnt/nas` shows the export.
- `systemctl show docker -p Requires` lists both `mnt-cephfs.mount` and
  `mnt-nas.mount`.
- A file created in `/mnt/nas` on one node is visible on the other two and
  owned by `1000:1000`.

### Phase 2 — Storage cutover, stack by stack

The swarm is never rebuilt; each stack is cut over in place. Nothing else on
TrueNAS needs to exist before this phase beyond Phase 0 and Phase 1.

The compose edits in step 4 are already applied in the repo — this branch
carries the post-migration compose state for every stack (`/mnt/nas` paths,
in-stack DB services removed and pointed at the TrueNAS app). At cutover you
only deploy; rollback repoints compose at the cephfs paths (the old files are
in git history).

Repeatable procedure per stack:

1. **Freeze** it: `docker service scale <stack>_<svc>=0` (all services of the
   stack).
2. **Copy config** from CephFS to the NFS dataset (any node — both mounts are
   shared):
   `sudo rsync -aHAX --numeric-ids /mnt/cephfs/<dir>/ /mnt/nas/<dir>/`.
3. **DB** (if it has one): dump from the old container, restore into the TrueNAS
   DB. Postgres: `pg_dump -U <user> <db> | gzip`, then
   `zcat | psql -h 10.10.10.60 -p <port> -U <user> <db>`. Kuma:
   `mariadb-dump`, restore to `10.10.10.60:3306`.
4. **Edit compose** for that stack: `/mnt/cephfs/<x>` → `/mnt/nas/<x>`; remove
   in-stack DB services and their `depends_on`; point DB hosts at
   `10.10.10.60` (ports per the table in section 2).
5. **Deploy in place:** `./deploy.sh <stack>` from `stacks/`.
6. **Verify**, then unfreeze (the deploy already brings replicas back).

**Rollback per stack:** repoint compose at the cephfs paths (and, for DBs, the
old in-stack service) and redeploy. CephFS data stays untouched until every
node has been reformatted, so early rollback is always possible. Data written
after a rollback diverges between the two copies — treat rollback as a decision,
not a reflex, and re-sync before cutting over again.

**Order** (least-coupled first, covering every live stack): **gitea →
uptime-kuma → apprise → sponsorblock → mcpjungle → media → jellyfin, netboot,
pairdrop, truenas, renovate, dns → homeassistant → immich → traefik.** The six
proxy/stateless stacks need only path and DB-host checks; traefik goes last,
with the VIP still on old nodes.

**Per-service notes:**

- **gitea (2.1)** — config `gitea-config`; DB `gitea-postgres`. Remove the `db`
  service; set `GITEA__database__HOST=10.10.10.60:5434`.
- **uptime-kuma (2.2)** — config `uptime-kuma`, `autokuma-data`; DB
  `kuma-mariadb`. Remove `mariadb`; set `UPTIME_KUMA_DB_HOSTNAME=10.10.10.60`.
- **apprise (2.3)** — config `apprise-config`, `apprise-attachments`; no DB.
  Path renames only.
- **sponsorblock (2.4)** — config `sponsorblock`; no DB. Path rename only.
- **mcpjungle (2.5, new)** — config `mcpjungle-conf`; DB `mcpjungle-postgres`
  on `10.10.10.60:5435`. Remove the `db` service and `depends_on`; keep the
  filesystem-server bind, re-pointed to `/mnt/nas/mcpjungle-host:/host:rw`
  (revisit whether it is still wanted — open item).
- **media (2.6)** — configs `transmission`, `prowlarr-config`, `radarr-config`,
  `sonarr-config`, `lidarr-config`, `swiparr`; no DB. Blob NFS volumes
  (`downloads*`, `media*`) unchanged.
- **jellyfin / netboot / pairdrop / truenas (2.7)** — proxy stacks; confirm no
  cephfs paths remain in their compose files, rename anything that slipped in.
- **renovate (2.8)** — stateless (no volumes, no DB); `swarm-cronjob` runs it
  daily and the `0/1` replica count is its designed resting state. Redeploy
  as-is; nothing to migrate.
- **dns (2.9)** — config `dns`, mounted twice (unbound's generated config, the
  generator's output); no DB. Rename `/mnt/cephfs/dns` → `/mnt/nas/dns` for both
  mounts. Records are generated against the VIP (`TRAEFIK_IP=10.10.10.20`), so
  nothing depends on where the VIP currently sits; unbound and adguard run
  `mode: global` on every node — unbound on the `main` overlay, adguard
  publishing port 53 through the ingress routing mesh — so DNS keeps resolving
  on the two nodes that are up while the third is being swapped.
- **homeassistant (2.10)** — configs `homeassistant`, `mosquitto`, `trmnl`,
  `tasmoadmin-config`, `predbat`; DB `homeassistant-postgres`. Remove the
  `postgres` service and the `depends_on`. In the **home-assistant** repo,
  change `static/recorder.yaml` to `@10.10.10.60:5432` **and** point
  `upload.sh`'s rsync target at `/mnt/nas/homeassistant` — after the cutover a
  build uploaded to the cephfs path would silently land on the stale copy —
  then rebuild and upload.
- **immich (2.11)** — DB `immich-postgres`. Set `DB_HOSTNAME=10.10.10.60` and
  `DB_PORT=5433` on the server and microservices; remove the `immich-postgres`
  service and `pgdata` volume; change `ml-cache` to a plain node-local volume.
  `photos`/`uploads_*` NFS volumes unchanged.
- **traefik (2.12, last)** — configs `traefik-config`,
  `traefik-config-letsencrypt` (including `acme.json`). Path rename only; the
  VIP does not move in this phase.

**Verify per service:** container healthy; data present (Gitea repos, Kuma
monitors, HA history, Immich library, *arr configs); the service answers
through Traefik; logs free of DB errors.

### Phase 3 — Node swaps: cl01 → cl02 → cl03

Before the first swap: add the **sysext-bakery keepalived extension** to the
Ignition configs — fetch the pinned `keepalived-v2.3.1-x86-64.raw` into
`/opt/extensions/keepalived/`, symlink it into `/etc/extensions/keepalived.raw`,
no `systemd-sysupdate` timer — and change `keepalived.service` from
`enabled: false` to `enabled: true` with the VRRP parameters from section 2
(`state BACKUP`, priority 50 on Flatcar nodes for now). Re-render and re-run
the QEMU pre-flight for that change: the unit must start as `BACKUP` without
stealing the VIP. This is the least-proven piece of Phase 3; if the sysext will
not come up, stop and resolve it before any swap (the fallback is leaving the
last old node idle as VIP holder).

The resolver pinning is already in `common.bu.tmpl` (section 2) — the same
re-render and QEMU pre-flight pass covers it: the node must lease an address
via `10-cluster.network` and name only the VIP as resolver. Skip the pin and a
swapped node silently falls back to router DNS that cannot resolve
`*.danhughes.dev`, and its containers lose every internal name with it.

Per node:

1. Confirm the swarm is converged: `docker node ls`, `docker service ls` —
   all replicas healthy.
2. Re-render that node's Ignition config with the **current** manager join
   token (`docker swarm join-token manager -q`) — one token serves all three
   swaps because the swarm is never re-initialised. The join target
   (`MANAGER_IP` in `nodes/*.env`) must be a manager that is **alive during
   that node's swap** — never the node being reformatted (cl01 joins via
   cl02, cl02/cl03 via the Flatcar cl01).
3. Drain: `docker node update --availability drain <node>`. Services reschedule
   onto the two surviving nodes, which already read `/mnt/nas` — this is what
   makes the swap safe.
4. Reformat in place with that node's config, using the Delivery flow above
   (serve.sh, PXE menu, `flatcar-install`, reboot).
5. The node rejoins the **same** swarm via the baked token and comes up as a
   manager with `/mnt/nas` mounted and docker gated on it.
6. Demote and remove the replaced node's **stale entry** — the reformatted
   node re-joined under a new identity, and the old entry keeps its raft seat
   until demoted. With it, a three-manager swarm carries four raft members
   with one dead: quorum is 3/4 and the next swap has no margin. Run
   `docker node demote <stale-id>` then `docker node rm <stale-id>` (add
   `--force` if the entry still shows down), so the manager count matches
   reality before the next swap.
7. Activate and rebalance: `docker node update --availability active <node>`,
   then `./rebalance.sh`.
8. Verify the node: `findmnt /mnt/nas`, `systemctl show docker -p Requires`
   includes `mnt-nas.mount`, `docker node ls` shows it `Ready`/`Reachable`,
   keepalived `BACKUP` (cl01) with the VIP still held by an old node, resolvers
   pinned (`resolvectl dns enp1s0` = `10.10.10.20`; a container resolves
   `home.danhughes.dev`), and services healthy on it.

Repeat for cl02, then cl03. When cl03 — the last old node — is reformatted, the
VIP moves to a Flatcar node and `10.10.10.20` resolves to it; DNS is unchanged.

**Verify (whole phase):** three managers `Ready`/`Reachable`; VIP on Flatcar;
every service healthy after rebalance; certificates still valid (`acme.json`
moved in Phase 2); locksmithd reboot windows staggered (02:00/03:00/04:00).

### Phase 4 — Post-migration checks

1. All services on three Flatcar nodes; nothing scaled to 0 that should not be.
2. No `/mnt/cephfs` reference remains anywhere in `stacks/` or on any node
   (the reformats removed MicroCeph with the old OS).
3. Configs editable from the workstation over NFS (`/mnt/nas` with `mapall`).
4. A node reboot in its locksmithd window rejoins cleanly (try one if you can).
5. Restore cl01's keepalived end state — `state MASTER`, `priority 100` in
   `/etc/keepalived/keepalived.conf` (or re-render) — so the VIP lands on cl01
   rather than on whichever node wins the equal-priority election between the
   three backups.
6. Remove the old Ansible-era remnants from the docs if any reference survived.

## 5. Risks and accepted trade-offs

- **SQLite on NFS** — mitigated by single replica and correct locking; already a
  known risk on CephFS (a past HA recorder corruption is noted in
  `predbat/apps.yaml`).
- **TrueNAS is a SPOF** for every database and all config.
- **The config export is open to any host** — anything on the LAN can read/write
  app config as `1000:1000`, including embedded secrets.
- **`mapall` means any container can write any file on the shared dataset.**
- **Longer two-OS interim.** Old and Flatcar nodes coexist until the last swap,
  and CephFS keeps running through Phase 2 as the rollback tier. The trade is
  deliberate: every stack gets an independently reversible cutover, and the
  risky new pieces (Flatcar, NFS mount, swarm join on Flatcar, keepalived
  sysext) are spread across three separate small swaps instead of one big cut.
- **Rollback diverges data.** A stack rolled back after its freeze window writes
  to the backend it landed on; the other copy goes stale. Re-sync before
  re-cutting over.
- **The keepalived sysext is the least-proven piece** of the final swap. The
  bakery ships it and the version is pinned, but prove `BACKUP` behaviour in
  QEMU before cl01; if it will not run on Flatcar, the fallback is to leave the
  last old node idle as VIP holder until it is resolved.
- **Baked join token** — safe here because the swarm is never re-initialised;
  re-render each node's config with the current token before its swap anyway.
- **Drain-before-reformat** moves services before the node goes down, so the
  reformat window is covered; the drain itself is the only disruption, and it
  is brief because only one node drains at a time.
- **Weekly reboot vs Flatcar's update reboots** — resolved by using one
  mechanism. `/etc/flatcar/update.conf` sets `REBOOT_STRATEGY=reboot` with a
  one-hour maintenance window per node (cl01 02:00, cl02 03:00, cl03 04:00), so
  the managers never reboot together and a node reboots only when an update is
  staged. The old weekly reboot timer is dropped so it cannot race locksmithd.
  `etcd-lock` was rejected: it needs an etcd cluster and this stack runs Docker
  Swarm.
- **NFS version** — pinned to 4.0 on both the Flatcar unit and the old-node
  unit (4.1/4.2 kernel regression).
- **DB network exposure** — firewalled to the swarm subnet.
- **Resolver pinning is new on master and unproven on Flatcar.** The old OS
  pins host and container resolvers to the VIP via Ansible (landed 2026-09-26);
  the Ignition replacement (`10-cluster.network` + `daemon.json`) is written
  but not yet booted. Failure modes are quiet: if the NIC is named differently
  on real hardware the unit matches nothing and the node falls back to router
  DNS; a broken unit can also take the NIC's lease with it. The QEMU
  pre-flight checks both the lease and the resolver, and the Phase 3 per-node
  verify repeats them on real hardware.

## 6. Open items

- Revisit whether mcpjungle's filesystem-server bind (`/mnt/nas/mcpjungle-host`
  → `/host:rw`) is still wanted; for now it is kept, re-pointed.
- Confirm each node's disk device name for `flatcar-install -d /dev/<disk>` at
  its Phase 3 swap (`lsblk` on the PXE-booted node).
- In the QEMU pre-flight, confirm `10-cluster.network` wins the match for
  `enp1s0` over whatever the platform ships (`networkctl status enp1s0`,
  `ls /usr/lib/systemd/network/`) and that the DHCP lease survives; the
  `daemon.json` pin uses `overwrite`, so a shipped file is replaced either way.
- The keepalived **`auth_pass` is now public knowledge on the LAN** (it ships in
  every node's config and is baked into the rendered Ignition files). Consider
  rotating it as part of the final swap — all three nodes must change together,
  which the swaps stagger.
