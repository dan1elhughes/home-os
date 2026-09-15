# Persistent storage on TrueNAS for the Flatcar swarm

Status: draft (pending final approval)
Date: 2026-09-15

## Context

The three swarm nodes (cl01/cl02/cl03) are moving to **Flatcar Container Linux**.
Flatcar is immutable: read-only `/usr`, no package manager, no snapd. That has
two consequences for the current setup:

- MicroCeph (a snap) cannot run on the nodes, so `/mnt/cephfs` disappears.
- The Docker boot dependency `Requires=mnt-cephfs.mount` cannot be satisfied.

Today the stacks use three storage paths:

| Tier | Mechanism | Where | Holds |
|---|---|---|---|
| A | Bind mount `/mnt/cephfs/<dir>` | MicroCeph on cl01/02/03 | App config, **all databases**, SQLite state |
| B | Named volume, `local` driver + `type: nfs` | TrueNAS `10.10.10.60` | Media libraries, downloads, Immich photo blobs |
| C | No persistent volume | — | Proxies, redis, ephemeral jobs |

The goal is to make the Flatcar nodes stateless and put all persistent storage
on the **existing TrueNAS SCALE box** (`10.10.10.60`), which already serves the
Tier B exports.

## Decisions

1. **NAS = the existing TrueNAS box** (`10.10.10.60`), not a new appliance.
2. **Databases become remote servers on TrueNAS**, in a single custom app with
   all data on the SSD pool. Application containers connect over TCP instead of
   running the DB locally. This avoids putting Postgres/MariaDB data dirs on NFS.
3. **App config lives on a TrueNAS NFS export**, host-mounted at one path on
   every node, replacing `/mnt/cephfs`. Single-replica apps; the
   SQLite-on-network-FS risk is accepted as it is today.
4. **NFS attach = one dataset, one export**, mounted at `/mnt/nas` on every node
   via Butane/Ignition. Per-service Docker NFS volumes and iSCSI rejected.
5. **The export maps all users (including root) to `PUID:PGID`** (`mapall`), so
   `linuxserver` apps, Immich's `3000`, and root-running apps all write as one
   identity. The export is reachable from **any host**, so the configs can be
   mounted and edited directly from a workstation.
6. **Media/Immich blob exports stay as they are** (Docker `type: nfs` volumes) —
   explicitly not unified onto the new host-mount mechanism.
7. **Flatcar Ignition configs live in this repo**, scoped to what storage needs
   (the NFS mount unit and the Docker dependency drop-in). How the configs are
   delivered to nodes is out of scope.
8. **Database credentials are unchanged.** The existing fixed creds
   (`POSTGRES_PASSWORD=POSTGRES`, `gitea/gitea`, `kuma/mariadb`) move with the
   databases; no credential churn during the migration.
9. **Snapshots and backups are out of scope** — this work relocates storage.
10. **The `home-assistant` repo's recorder change is in scope with this work**, so
    HA and the stacks move together.

## Design

### 1. Target architecture

- Flatcar nodes hold no persistent data.
- TrueNAS SCALE (`10.10.10.60`) is the only persistent layer:
  - Runs the four databases as remote servers on native ZFS.
  - Exports one config dataset over NFS, mounted at `/mnt/nas` on every node.
  - Continues to export the media/Immich blob datasets as it does now.
- Application containers become effectively stateless.

### 2. TrueNAS layout

- New dataset on the SSD pool (small-file SQLite over HDD NFS would be slow):
  `/mnt/SSD/cluster`, exported as a single NFS export. Subdirectories mirror
  today's CephFS names, so compose paths map 1:1:

  `homeassistant/`, `predbat/`, `mosquitto/`, `tasmoadmin-config/`, `trmnl/`,
  `gitea-config/`, `uptime-kuma/`, `autokuma-data/`, `traefik-config/`,
  `traefik-config-letsencrypt/`, `transmission/`, `prowlarr-config/`,
  `radarr-config/`, `sonarr-config/`, `lidarr-config/`, `swiparr/`,
  `apprise-config/`, `apprise-attachments/`, `sponsorblock/`.

  These are exactly today's CephFS directories, **minus** the ones that move
  with the databases: `homeassistant-postgres`, `gitea-db`,
  `uptime-kuma-mariadb`, `immich-postgres`. `immich-ml-cache` is also dropped —
  it becomes a node-local volume, since it is a re-downloadable model cache.

- NFS export settings:
  - Allowed hosts: **any** (`0.0.0.0/0`), so the configs can be mounted and
    edited from a workstation, not only from the nodes.
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
  Immich DB must use that exact image — it needs VectorChord / pgvecto.rs.
  Ports (`5432`, `3306`) are published on `10.10.10.60` and firewalled to the
  swarm subnet.

### 3. Flatcar node configuration

Butane → Ignition. The config lives in a new `flatcar/` directory (all three
nodes share it), transpiled with `butane --pretty --strict`.

- `mnt-nas.mount`:

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

- Docker drop-in at `/etc/systemd/system/docker.service.d/10-nas.conf`, so Docker
  never starts with an empty `/mnt/nas` (this replaces
  `roles/docker/files/after-mount.conf`, which targets read-only `/usr`):

  ```ini
  [Unit]
  Requires=mnt-nas.mount
  After=mnt-nas.mount
  ```

- Enable `rpc-statd` so NFS locking works (Flatcar does not start it by
  default). If it proves troublesome for a strictly single-writer directory, a
  per-mount `nolock` is the fallback.
- Remove the now-dead Ceph bits from the Ansible roles:
  `roles/swarm` MicroCeph tasks,
  `roles/swarm/templates/snap_microceph_daemon_override.j2.conf`, and
  `roles/docker/files/after-mount.conf`.
- Docker data-root stays on the node's persistent state partition — containers
  are re-pullable, so node-local is fine.

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
  `@postgres:5432` → `@10.10.10.60:5432`; rebuild and upload that repo in
  lockstep.
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

`deploy.sh` needs no change.

### 5. Data migration and cutover

1. **TrueNAS prep.** Create `/mnt/SSD/cluster` and the four `/mnt/SSD/db/*`
   datasets, set the config dataset to `PUID:PGID`, create the open (`0.0.0.0/0`)
   `mapall` export, and deploy the DB app (firewalled to the swarm subnet).
2. **Config copy (bridge).** While one node is still on the old OS, mount the new
   export at `/mnt/nas` and `rsync -aHAX --numeric-ids` each surviving
   `/mnt/cephfs/<dir>` across. Verify counts. CephFS is shared, so one node is
   enough.
3. **Database migration.** Dump from the running containers (`pg_dump`/
   `pg_dumpall` for the three Postgres DBs, `mariadb-dump` for Kuma) and restore
   into the TrueNAS DBs. The Immich DB must be migrated exactly; the HA recorder
   DB carries history and long-term statistics.
4. **Cutover.** Scale the affected stacks to 0, do a final `rsync` and a final DB
   dump/restore, provision the nodes as Flatcar (Ignition with `mnt-nas.mount`),
   and redeploy the stacks (and the rebuilt home-assistant config) against
   `/mnt/nas` and the remote DBs.
5. **Verify.** Check each service's data (Immich library, HA history, Gitea
   repos, Kuma monitors, *arr configs). Keep the Ceph data until this passes;
   decommission MicroCeph separately.

**Rollback:** the old setup is untouched until step 4. After cutover, rollback
means reverting the compose changes and booting the previous OS image.

### 6. Risks and accepted trade-offs

- **SQLite on NFS** — mitigated by single replica and correct locking; known
  risk, already present on CephFS (a past HA recorder corruption is noted in
  `predbat/apps.yaml`).
- **TrueNAS is a SPOF** for every database and all config. Accepted.
- **NFS version on Flatcar** — 4.1/4.2 kernel regression; pin 4.0 and validate.
- **`mapall` means any container can write any file on the shared dataset.**
  Accepted for a LAN-only home cluster.
- **The config export is open to any host.** Anything on the LAN can mount it and
  read/write app config as `PUID:PGID`, including secrets embedded in configs
  (HA, Gitea, Traefik, etc.). Accepted for a private internal network, and
  required so configs can be edited from a workstation.
- **DB network exposure** — must be firewalled to the swarm subnet.

### 7. Out of scope

Separate work, deliberately excluded:

- Base OS provisioning (users, SSH keys, sudo, weekly-reboot timer), Docker
  data-root config, swarm init/join, the `keepalived` VIP role, and a `cockpit`
  replacement. These currently live in `apt`-based Ansible roles with no Flatcar
  equivalent.
- The Ignition **delivery** mechanism (PXE/matchbox, `flatcar-install -i`, or a
  hosted URL). This design assumes one exists and can carry the storage units.
- Snapshots, backups, and replication.
- Decommissioning MicroCeph / reclaiming the node disks.

### To confirm at implementation

- The literal `PUID:PGID` values (from the 1Password env) and the TrueNAS user
  they map to.
- Final dataset naming under the SSD pool.
