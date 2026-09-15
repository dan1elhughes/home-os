# Persistent storage on TrueNAS for the Flatcar swarm

Status: draft
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

## Decisions made

1. **NAS = the existing TrueNAS box** (`10.10.10.60`), not a new appliance.
2. **Databases become remote servers on TrueNAS**, run as custom apps on native
   ZFS storage. Application containers connect over TCP instead of running the
   DB locally. This avoids putting Postgres/MariaDB data dirs on NFS.
3. **App config lives on a TrueNAS NFS export**, host-mounted at one path on
   every node, replacing `/mnt/cephfs`. Single-replica apps, tuned NFS mount
   options; the SQLite-on-network-FS risk is accepted as it is today.
4. **NFS attach method A**: one export, host-mounted via Butane/Ignition at
   `/mnt/nas` on every node. Per-service Docker NFS volumes (approach B) and
   iSCSI (approach C) rejected.
5. **Media/Immich blob exports stay as they are** (Docker `type: nfs` volumes) —
   explicitly not unified onto the new host-mount mechanism.
6. **Flatcar provisioning is in scope and lives in this repo.** The Butane
   configs needed for storage (the NFS mount unit and the Docker dependency
   drop-in) are added here and transpiled to Ignition. Scope is bounded to what
   storage needs — see the boundary below.

## Open questions

- **SSD pool / dataset name.** Assumed `/mnt/SSD/cluster`; to confirm.
- **How Ignition is delivered** to the nodes (PXE/matchbox, `flatcar-install -i`,
  or a hosted config URL). Storage only needs the file to exist and reach the
  node; the delivery mechanism may be a separate concern.
- **PUID/PGID values** (from 1Password env) and how they map to ownership on the
  TrueNAS dataset.
- Whether to also run the TrueNAS DB app with the same credentials currently
  hardcoded in compose (`POSTGRES_PASSWORD=POSTGRES`, etc.) or move them to
  1Password.

## Design

### 1. Target architecture

- Flatcar nodes hold no persistent data.
- TrueNAS SCALE (`10.10.10.60`) is the only persistent layer:
  - Runs the four databases as remote servers (native ZFS + snapshots).
  - Exports one config dataset over NFS, mounted at `/mnt/nas` on every node.
  - Continues to export the media/Immich blob datasets as it does now.
- Application containers become effectively stateless.

### 2. TrueNAS layout

- New dataset on the SSD pool (small-file SQLite over HDD NFS would be slow):
  `/mnt/SSD/cluster`, exported as a single NFS export and mounted at `/mnt/nas`.
  Subdirectories mirror today's cephfs names, so compose paths map 1:1:

  `homeassistant/`, `predbat/`, `mosquitto/`, `tasmoadmin-config/`, `trmnl/`,
  `gitea-config/`, `uptime-kuma/`, `autokuma-data/`, `traefik-config/`,
  `traefik-config-letsencrypt/`, `transmission/`, `prowlarr-config/`,
  `radarr-config/`, `sonarr-config/`, `lidarr-config/`, `swiparr/`,
  `apprise-config/`, `apprise-attachments/`, `sponsorblock/`.

  These are exactly today's CephFS directories, **minus** the ones that move
  with the databases when the DBs become remote: `homeassistant-postgres`,
  `gitea-db`, `uptime-kuma-mariadb`, `immich-postgres`. The `immich-ml-cache`
  directory is also dropped — it becomes a node-local volume (see compose
  changes), since it is a re-downloadable model cache.

- Databases as one custom app (compose) on TrueNAS:

  | Container | Image | Data |
  |---|---|---|
  | `immich-postgres` | `ghcr.io/immich-app/postgres:18-vectorchord0.5.3` | ZFS dataset |
  | `homeassistant-postgres` | `postgres:18-alpine` | ZFS dataset |
  | `gitea-postgres` | `postgres:18.6` | ZFS dataset |
  | `kuma-mariadb` | `mariadb:12.3` | ZFS dataset |

  The Immich DB must use that exact image — it needs VectorChord / pgvecto.rs.

- NFS export restricted to the three node IPs (`10.10.10.21-23`), `rw`,
  `sec=sys`, `nfsvers=4` (matches the existing exports and avoids the Flatcar
  4.1/4.2 kernel regression).

### 3. Flatcar node configuration

Butane → Ignition:

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

- Docker drop-in at `/etc/systemd/system/docker.service.d/10-nas.conf`
  (replaces `roles/docker/files/after-mount.conf`, which targets read-only
  `/usr` on Flatcar):

  ```ini
  [Unit]
  Requires=mnt-nas.mount
  After=mnt-nas.mount
  ```

- Enable `rpc-statd` if NFS locking is needed, or mount config `nolock` where a
  directory is strictly single-writer (Flatcar rpc-statd quirk).
- Remove the MicroCeph tasks from `roles/swarm`, `roles/docker/files/after-mount.conf`,
  and `roles/swarm/templates/snap_microceph_daemon_override.j2.conf` — all
  superseded by the Ignition units above.
- Docker data-root stays on the node's persistent state partition — containers
  are re-pullable, so node-local is fine.

#### Repo layout and scope boundary

The Butane config lives in a new `flatcar/` directory, one file per node role
(all three nodes share the storage config), transpiled to Ignition JSON with
`butane --pretty --strict`. Storage owns only:

- the `mnt-nas.mount` unit,
- the Docker `10-nas.conf` drop-in, so Docker does not start before `/mnt/nas`,
- any `/etc` files those two need.

**Explicitly out of scope** for this spec (separate work): base OS provisioning
(users, SSH keys, sudo, the weekly-reboot timer), Docker data-root config, swarm
init/join, the `keepalived` VIP role, and a `cockpit` replacement. All of these
currently live in `apt`-based Ansible roles with no Flatcar equivalent; deciding
what replaces each is a follow-on spec. This spec assumes an Ignition delivery
mechanism exists and can carry the storage units — it does not define that
mechanism.

### 4. Compose changes

Global: every `/mnt/cephfs/...` bind mount becomes `/mnt/nas/...`. Media/Immich
NFS blob volumes are untouched.

**immich**
- Delete the `immich-postgres` service and the `pgdata` volume.
- `immich-server` and `immich-microservices`: `DB_HOSTNAME: immich-postgres` →
  `10.10.10.60`.
- `ml-cache`: replace the CephFS bind volume with a plain node-local volume
  (`driver: local`). It is a re-downloadable model cache and the microservices
  run `mode: global`, so each node should keep its own copy.
- `photos` and `uploads_*` NFS volumes unchanged.

**homeassistant**
- Delete the `postgres` service and remove `postgres` from `depends_on`.
- The HA recorder URL lives in the **home-assistant** repo
  (`static/recorder.yaml`): `@postgres:5432` → `@10.10.10.60:5432`. That repo's
  build/upload must change in lockstep with this one.
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

`deploy.sh` needs no change — it still wraps `op run`, and the remote-DB
credentials can be added to the same 1Password environment.

### 5. Data migration and cutover

1. **TrueNAS prep.** Create the config dataset on the SSD pool, set ownership to
   the apps' `PUID`/`PGID`, create the node-restricted NFS export, and deploy the
   DB app (four containers, ZFS datasets, firewalled to the swarm subnet).
2. **Config copy (bridge).** While one node is still on the old OS, mount the new
   export at `/mnt/nas` and `rsync -aHAX --numeric-ids` each surviving
   `/mnt/cephfs/<dir>` across. Verify counts and ownership. CephFS is shared, so
   one node is enough.
3. **Database migration.** Dump from the running containers (`pg_dump`/
   `pg_dumpall` for the three Postgres DBs, `mariadb-dump` for Kuma) and restore
   into the TrueNAS DBs. The Immich DB must be migrated exactly; the HA recorder
   DB carries history and long-term statistics.
4. **Cutover.** Scale the affected stacks to 0, do a final `rsync` and a final DB
   dump/restore, provision the nodes as Flatcar (Ignition with `mnt-nas.mount`),
   and redeploy the stacks against `/mnt/nas` and the remote DBs.
5. **Verify.** Check each service's data (Immich library, HA history, Gitea
   repos, Kuma monitors, *arr configs). Keep the Ceph data until this passes;
   decommission MicroCeph separately.

**Rollback:** the old setup is untouched until step 4. After cutover, rollback
means reverting the compose changes and booting the previous OS image.

### 6. Risks

- **SQLite on NFS.** Mitigated by single replica and correct locking; known
  risk, already present on CephFS (a past HA recorder corruption is noted in
  `predbat/apps.yaml`).
- **TrueNAS is a SPOF** for every database and all config.
- **NFS version on Flatcar** — 4.1/4.2 kernel regression; pin 4.0.
- **uid/gid mapping** between container `PUID`/`PGID` and TrueNAS dataset
  ownership.
- **DB network exposure** — must be firewalled to the swarm subnet.
