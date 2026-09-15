# Flatcar migration: TrueNAS storage and Ignition provisioning

Status: draft (pending final approval)
Date: 2026-09-15

## Context

The three swarm nodes (cl01/cl02/cl03) are moving to **Flatcar Container Linux**.
Flatcar is immutable: read-only `/usr`, no package manager, no snapd, and no
Python in the base image. That has three consequences:

- MicroCeph (a snap) cannot run on the nodes, so `/mnt/cephfs` disappears, and
  the Docker boot dependency `Requires=mnt-cephfs.mount` cannot be satisfied.
- The `apt`-based Ansible roles cannot run at all, and Ansible cannot even
  connect without a Python sysext.
- Provisioning has to move to **Ignition** (via Butane), which is per-node and
  runs at first boot.

Today the stacks use three storage paths:

| Tier | Mechanism | Where | Holds |
|---|---|---|---|
| A | Bind mount `/mnt/cephfs/<dir>` | MicroCeph on cl01/02/03 | App config, **all databases**, SQLite state |
| B | Named volume, `local` driver + `type: nfs` | TrueNAS `10.10.10.60` | Media libraries, downloads, Immich photo blobs |
| C | No persistent volume | — | Proxies, redis, ephemeral jobs |

The goal is to make the Flatcar nodes stateless, put all persistent storage on
the **existing TrueNAS SCALE box** (`10.10.10.60`), and provision the nodes with
Ignition instead of Ansible.

## Decisions

1. **NAS = the existing TrueNAS box** (`10.10.10.60`), not a new appliance.
2. **Databases become remote servers on TrueNAS**, in a single custom app with
   all data on the SSD pool. Application containers connect over TCP. This
   avoids putting Postgres/MariaDB data dirs on NFS.
3. **App config lives on a TrueNAS NFS export**, host-mounted at one path on
   every node, replacing `/mnt/cephfs`. Single-replica apps; the
   SQLite-on-network-FS risk is accepted as it is today.
4. **NFS attach = one dataset, one export**, mounted at `/mnt/nas` on every node
   via Ignition. Per-service Docker NFS volumes and iSCSI rejected.
5. **The export maps all users (including root) to `PUID:PGID`** (`mapall`), and
   is reachable from **any host**, so configs can be edited from a workstation.
6. **Media/Immich blob exports stay as they are** (Docker `type: nfs` volumes).
7. **Ansible is retired entirely.** Playbook, inventory, roles and `init.sh` are
   deleted; provisioning moves to Ignition (Butane). How the Ignition configs
   reach the nodes is out of scope.
8. **The node login user is Flatcar's `core`** — SSH key, passwordless sudo,
   docker group. No bespoke user.
9. **Swarm membership is established in Ignition**, with the manager join token
   baked into the follower configs.
10. **Database credentials are unchanged** — no churn during migration.
11. **Snapshots and backups are out of scope.**
12. **The `home-assistant` repo's recorder change is in scope with this work.**
13. **The migration is progressive.** Build a single-node swarm on cl01 first,
    move services one at a time, cut ingress over last, then expand to three
    nodes. The old swarm keeps serving until each service is moved.

## Design

### 1. Target architecture

- Flatcar nodes hold no persistent data.
- TrueNAS SCALE (`10.10.10.60`) is the only persistent layer:
  - Runs the four databases as remote servers on native ZFS.
  - Exports one config dataset over NFS, mounted at `/mnt/nas` on every node.
  - Continues to export the media/Immich blob datasets as it does now.
- Provisioning is Ignition; Ansible is gone.

### 2. TrueNAS layout

- New dataset on the SSD pool: `/mnt/SSD/cluster`, exported as a single NFS
  export. Subdirectories mirror today's CephFS names, so compose paths map 1:1:

  `homeassistant/`, `predbat/`, `mosquitto/`, `tasmoadmin-config/`, `trmnl/`,
  `gitea-config/`, `uptime-kuma/`, `autokuma-data/`, `traefik-config/`,
  `traefik-config-letsencrypt/`, `transmission/`, `prowlarr-config/`,
  `radarr-config/`, `sonarr-config/`, `lidarr-config/`, `swiparr/`,
  `apprise-config/`, `apprise-attachments/`, `sponsorblock/`.

  These are exactly today's CephFS directories, **minus** the DB dirs that move
  with the databases: `homeassistant-postgres`, `gitea-db`,
  `uptime-kuma-mariadb`, `immich-postgres`. `immich-ml-cache` is also dropped —
  it becomes a node-local volume, since it is a re-downloadable model cache.

- NFS export settings:
  - Allowed hosts: **any** (`0.0.0.0/0`), so configs can be mounted and edited
    from a workstation, not only from the nodes.
  - `rw`, `sec=sys`, `nfsvers=4` (matches the existing exports and avoids the
    Flatcar 4.1/4.2 kernel regression).
  - `mapall` → `PUID:PGID`.

- Databases as one custom app (compose) on TrueNAS, data on the SSD pool:

  | Container | Image | Data dataset |
  |---|---|---|
  | `immich-postgres` | `ghcr.io/immich-app/postgres:18-vectorchord0.5.3` | `/mnt/SSD/db/immich` |
  | `homeassistant-postgres` | `postgres:18-alpine` | `/mnt/SSD/db/homeassistant` |
  | `gitea-postgres` | `postgres:18.6` | `/mnt/SSD/db/gitea` |
  | `kuma-mariadb` | `mariadb:12.3` | `/mnt/SSD/db/kuma` |

  Same image tags as today, so the migration is a straight dump/restore. The
  Immich DB must use that exact image (VectorChord / pgvecto.rs). Ports `5432`
  and `3306` are published on `10.10.10.60`, firewalled to the swarm subnet.

### 3. Node provisioning (Ignition)

A new `flatcar/` directory holds Butane configs. Because Butane has no includes,
a small build script renders a common fragment plus per-node values into three
`cl0*.bu` files and transpiles them with `butane --pretty --strict`.

**Common to all nodes:**

- **User/SSH:** `core` with embedded public SSH keys, passwordless sudo, and
  `docker` group membership. `PasswordAuthentication no`, `PermitRootLogin no`.
- **`mnt-nas.mount`:**

  ```ini
  [Unit]
  Before=remote-fs.target

  [Mount]
  What=10.10.10.60:/mnt/SSD/cluster
  Where=/mnt/nas
  Type=nfs
  Options=rw,hard,noatime,_netdev,nfsvers=4

  [Install]
  WantedBy=remote-fs.target
  ```

- **Docker drop-in** at `/etc/systemd/system/docker.service.d/10-nas.conf`, so
  Docker never starts with an empty `/mnt/nas`:

  ```ini
  [Unit]
  Requires=mnt-nas.mount
  After=mnt-nas.mount
  ```

- **Docker prune** (`docker-prune.service` + `.timer`): weekly
  `docker system prune --volumes -f`, Sunday 03:xx, per-node randomised minute.
- **Weekly reboot** (`reboot.timer`): Sunday 02:xx, per-node randomised minute.
- **`rpc-statd`** enabled for NFS locking (`nolock` per-mount as fallback).

**Per node:**

- **cl01** (Phase 1): `swarm-init.service` (oneshot) runs
  `docker swarm init --advertise-addr 10.10.10.21`, guarded by
  `ConditionPathExists` on the swarm state file. keepalived is included but
  **disabled** until Phase 3.
- **cl02/cl03** (Phase 4, rendered after cl01's swarm exists):
  `swarm-join.service` (oneshot) runs
  `docker swarm join --token <manager-token> 10.10.10.21:2377`. The token is
  baked in; if the swarm is re-initialised the config must be re-baked. The join
  target is cl01's node IP (not the VIP), so it does not depend on keepalived.
- **keepalived** via the Flatcar bakery `keepalived` sysext (community):
  cl01 `state MASTER` priority 100, cl02/cl03 `state BACKUP`, VIP `10.10.10.20`
  on the LAN interface. The VRRP `auth_pass` is injected from 1Password at
  config-render time and is not committed.

**Removed:** `playbook.yml`, `ansible.cfg`, `inventory.ini`, `roles/`, and
`init.sh`. `README.md` updated to describe the Ignition flow. The workstation
scripts (`deploy.sh`, `start.sh`, `stop.sh`, `rebalance.sh`, `pull-updates.sh`)
stay — they talk to the swarm context, not the nodes.

### 4. Compose changes

Global: every `/mnt/cephfs/...` bind mount becomes `/mnt/nas/...`. Media/Immich
NFS blob volumes are untouched.

**immich**
- Delete the `immich-postgres` service and the `pgdata` volume.
- `immich-server` and `immich-microservices`: `DB_HOSTNAME: immich-postgres` →
  `10.10.10.60`.
- `ml-cache`: replace the CephFS bind volume with a plain node-local volume
  (`driver: local`). The microservices run `mode: global`, so each node keeps its
  own copy.
- `photos` and `uploads_*` NFS volumes unchanged.

**homeassistant** (this repo + `home-assistant` repo)
- Delete the `postgres` service and remove `postgres` from `depends_on`.
- In the **home-assistant** repo, `static/recorder.yaml`:
  `@postgres:5432` → `@10.10.10.60:5432`; rebuild and upload in lockstep.
- Path renames for `tasmoadmin-config`, `mosquitto`, `trmnl`, `homeassistant`,
  `predbat`.

**gitea**
- Delete the `db` service.
- `GITEA__database__HOST: db:5432` → `10.10.10.60:5432`.
- `/mnt/cephfs/gitea-config:/data` → `/mnt/nas/gitea-config:/data`.

**uptime-kuma**
- Delete the `mariadb` service and remove `depends_on`.
- `UPTIME_KUMA_DB_HOSTNAME: mariadb` → `10.10.10.60`.
- Path renames for `uptime-kuma`, `autokuma-data`.

**media / traefik / apprise / sponsorblock**
- Path renames only.

### 5. Progressive migration

The old swarm (three managers) keeps serving while a **new single-manager swarm**
is built on cl01 and services are moved across one at a time. The `10.10.10.20`
VIP is live and floating, and DNS points at it, which is what makes the ingress
cutover in Phase 3 a VIP move rather than a DNS change.

**Phase 0 — TrueNAS prep.** Create `/mnt/SSD/cluster` and the four
`/mnt/SSD/db/*` datasets, set the config dataset to `PUID:PGID`, create the open
(`0.0.0.0/0`) `mapall` export, and deploy the DB app (firewalled to the swarm
subnet).

**Phase 1 — Stand up the new swarm on cl01.**
- Remove cl01 from the old swarm, leaving cl02/cl03 as its two managers. Verify
  the old swarm is healthy before touching cl01.
- Install Flatcar on cl01 with its Ignition config: `core` user/SSH,
  `mnt-nas.mount`, the Docker drop-in, `docker swarm init --advertise-addr
  10.10.10.21`, docker-prune and reboot timers, `rpc-statd`. **No join.**
  keepalived files are present but the service stays **disabled** to avoid
  fighting the old cluster for the VIP.
- Create the external overlay network the stacks expect:
  `docker network create --driver overlay --attachable main`.
- Verify: `/mnt/nas` is mounted before Docker starts, and `docker node ls` shows
  a single manager.

**Phase 2 — Move services one at a time.**
- Order, least-coupled first: gitea, uptime-kuma, apprise, sponsorblock, media
  (*arr + transmission), then homeassistant (+ mosquitto, trmnl, tasmoadmin,
  predbat), then immich.
- Per service:
  1. Scale it to 0 on the old swarm (freeze).
  2. `rsync -aHAX --numeric-ids /mnt/cephfs/<dir>` → `/mnt/SSD/cluster/<dir>`.
  3. If it has a DB, dump from the old container and restore into the TrueNAS DB.
  4. Deploy to the new swarm with `/mnt/nas` paths and the remote DB host, with a
     temporary `ports:` publish for verification.
  5. Verify, remove the temporary publish, and leave the old copy frozen.
- Only **one instance of an app may run at a time**, because its config now lives
  on the shared NFS export.
- `deploy.sh` must target the new swarm context. The old swarm keeps its
  last-applied specs untouched; it is never redeployed.

**Phase 3 — Migrate Traefik and move the VIP.**
- Deploy Traefik on cl01 against the migrated `/mnt/nas/traefik-config*`
  (copy `acme.json` so the certs carry over). Verify with a `Host` header against
  cl01's IP before moving anything.
- Stop keepalived on the old cluster, then enable it on cl01. The VIP moves to
  cl01; DNS already points at it. The old swarm is now idle.

**Phase 4 — Expand the swarm.**
- Render cl02/cl03 Ignition configs (manager join token from cl01's swarm,
  keepalived `BACKUP`).
- Reinstall cl02/cl03 as Flatcar; they leave the old swarm (which then dies) and
  join the new one, forming a three-manager swarm with keepalived running on all
  three.
- Rebalance service placement.

**Phase 5 — Retire the old.** Decommission MicroCeph; keep the Ceph data until
everything is verified.

**Rollback:** until each service is moved it still runs untouched on the old
swarm. After a service is moved, rollback means redeploying its previous spec
(and DB dump) to the old swarm. Ceph holds the original copies until the end.

### 6. Risks and accepted trade-offs

- **SQLite on NFS** — mitigated by single replica and correct locking; known
  risk, already present on CephFS (a past HA recorder corruption is noted in
  `predbat/apps.yaml`).
- **TrueNAS is a SPOF** for every database and all config. Accepted.
- **The config export is open to any host** — anything on the LAN can read/write
  app config as `PUID:PGID`, including embedded secrets. Accepted for a private
  network; required for workstation editing.
- **`mapall` means any container can write any file on the shared dataset.**
- **Baked join token** goes stale if the swarm is re-initialised.
- **keepalived** depends on a community sysext and on the VRRP `auth_pass` being
  injected at render time.
- **Old-swarm quorum during migration** — after cl01 leaves, the old swarm runs
  on two managers, so a single failure freezes its services. Move steadily and
  keep both up.
- **VIP conflict** — keepalived must stay stopped on cl01 until the old
  cluster's is stopped (Phase 3), or both clusters claim `10.10.10.20`.
- **Two swarms, two contexts** — deploying to the wrong cluster is easy; always
  be explicit about `DOCKER_CONTEXT`.
- **Ignition runs at first boot only** — anything added later (keepalived on
  cl01) has to be written include-but-disabled, or applied out of band.
- **Weekly reboot timer vs Flatcar's own update reboots** — may need
  coordination (Locksmith).
- **NFS version on Flatcar** — 4.1/4.2 kernel regression; pin 4.0 and validate.
- **DB network exposure** — firewalled to the swarm subnet.

### 7. Out of scope

- The Ignition **delivery** mechanism (PXE/matchbox, `flatcar-install -i`, or a
  hosted URL). This design assumes one exists and can carry the configs.
- Snapshots, backups, and replication.
- Decommissioning MicroCeph / reclaiming the node disks.

### To confirm at implementation

- The literal `PUID:PGID` values and the TrueNAS user they map to.
- Final dataset naming under the SSD pool.
- The LAN interface name for `mnt-nas.mount`/keepalived (was
  `ansible_default_ipv4.interface`).
- The manager join token for the baked follower configs.
- How keepalived is currently running on the old cluster (its Ansible role is
  commented out, yet the VIP is live), so it can be stopped cleanly at Phase 3.
