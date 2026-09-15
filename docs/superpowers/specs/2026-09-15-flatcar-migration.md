# Flatcar migration: TrueNAS storage and Ignition provisioning

Status: source of truth — sequential migration runbook
Date: 2026-09-15

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

Decisions already made:

1. **NAS = the existing TrueNAS SCALE box** (`10.10.10.60`), not a new appliance.
2. **Databases become remote servers on TrueNAS** (one custom app, SSD pool).
   Apps connect over TCP, so no Postgres/MariaDB data dirs on NFS.
3. **App config lives on a TrueNAS NFS export**, host-mounted at `/mnt/nas` on
   every node, replacing `/mnt/cephfs`. Single-replica apps; SQLite-on-network-FS
   risk accepted.
4. **One dataset, one export**, host-mounted via Ignition. Docker NFS volumes and
   iSCSI rejected.
5. **The export `mapall`s all users to `PUID:PGID`** and is reachable from **any
   host**, so configs can be edited from a workstation.
6. **Media/Immich blob exports stay as they are** (Docker `type: nfs` volumes).
7. **Ansible is retired entirely** — playbook, inventory, roles, `init.sh`
   deleted; Ignition owns provisioning. Config delivery to nodes is out of scope.
8. **Node login user is Flatcar's `core`** — SSH key, passwordless sudo, docker
   group. No bespoke user.
9. **Swarm membership is set in Ignition**, manager join token baked into the
   follower configs.
10. **DB credentials unchanged** — no churn during migration.
11. **Snapshots and backups are out of scope.**
12. **The `home-assistant` repo's recorder change is in scope** with this work.
13. **Migration is progressive**: single-node swarm on cl01 first, services moved
    one at a time, ingress cut over last, then expand to three nodes.

## 2. Target state (reference)

- Flatcar nodes hold **no persistent data**.
- TrueNAS is the only persistent layer: runs the four databases, exports the
  config dataset, and continues to export the media/Immich blobs.
- The old swarm keeps serving until each service has been moved.

**Storage inventory today** (for reference while migrating):

| Tier | Mechanism | Where | Holds |
|---|---|---|---|
| A | `/mnt/cephfs/<dir>` bind mount | MicroCeph | App config, all DBs, SQLite state |
| B | `local` driver + `type: nfs` volume | TrueNAS | Media, downloads, Immich blobs |
| C | none | — | Proxies, redis, ephemeral |

**TrueNAS config dataset:** `/mnt/SSD/cluster`, exported as one NFS export and
mounted at `/mnt/nas`. Subdirectories are today's CephFS names, minus the DB
dirs and `immich-ml-cache`:

`homeassistant`, `predbat`, `mosquitto`, `tasmoadmin-config`, `trmnl`,
`gitea-config`, `uptime-kuma`, `autokuma-data`, `traefik-config`,
`traefik-config-letsencrypt`, `transmission`, `prowlarr-config`,
`radarr-config`, `sonarr-config`, `lidarr-config`, `swiparr`, `apprise-config`,
`apprise-attachments`, `sponsorblock`.

**TrueNAS database app** (data on SSD, same image tags as today):

| Container | Image | Data dataset |
|---|---|---|
| `immich-postgres` | `ghcr.io/immich-app/postgres:18-vectorchord0.5.3` | `/mnt/SSD/db/immich` |
| `homeassistant-postgres` | `postgres:18-alpine` | `/mnt/SSD/db/homeassistant` |
| `gitea-postgres` | `postgres:18.6` | `/mnt/SSD/db/gitea` |
| `kuma-mariadb` | `mariadb:12.3` | `/mnt/SSD/db/kuma` |

**Per-node Ignition contents** (rendered from a common fragment + per-node
values, transpiled with `butane --pretty --strict`):

- Common: `core` user with embedded SSH keys, passwordless sudo, docker group;
  SSH hardening; `mnt-nas.mount`; Docker drop-in `10-nas.conf`; docker-prune
  service+timer; reboot timer; `rpc-statd`.
- cl01: `swarm-init.service`; keepalived included but **disabled** until Phase 3.
- cl02/cl03 (Phase 4): `swarm-join.service` with the baked manager token;
  keepalived `BACKUP`.

**Compose path mapping:** every `/mnt/cephfs/<x>` → `/mnt/nas/<x>`. DB services
and their `depends_on` entries are removed; DB hosts become `10.10.10.60`.

## 3. Local testing (optional pre-flight)

Most of the Ignition config and the database app can be validated on a laptop
before touching real hardware. Docker supplies the tooling; QEMU boots a real
Flatcar node. Docker cannot run Flatcar itself — it is a full OS with systemd as
PID 1.

**Context safety.** The local daemon and the cluster are different docker
contexts (e.g. `desktop-linux` vs `swarm`). Never rely on the default: pass
`--context <name>` or set `DOCKER_CONTEXT=<name>` on every command. A local
validation run sent to a swarm context executes on a live node, and a
`deploy.sh` run in a swarm context with unmigrated paths breaks live services.

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
- `docker node ls` shows one manager
- `systemctl status docker-prune.timer reboot.timer`
- `ls /mnt/nas`

**NFS export (Docker):** run an NFS server container with a test export, and
point a throwaway copy of the config's `What=` at `10.0.2.2` (QEMU's host
address) instead of `10.10.10.60`. Because the mount pins `nfsvers=4`, only port
2049 is needed, so `-f 2049:2049` works. A generic NFS container only
approximates TrueNAS — fake ownership with `all_squash,anonuid,anongid`; it will
not prove TrueNAS's `mapall` behaves identically.

**Database app (Docker Compose):** run the four-container DB app as an ordinary
compose project locally to validate images, environment, ports and dataset
mounts before deploying to TrueNAS. The Immich Postgres image is multi-arch.

**Limits:** swarm join tokens, the keepalived sysext, and the VIP are best tested
on real hardware or multiple QEMU VMs.

## 4. The sequence

Two docker contexts are used throughout. Define them once:

```sh
# existing old swarm (adjust to however you reach it today)
export OLD_CONTEXT=swarm
# new single-node swarm on cl01
docker context create cl01 --docker "host=ssh://core@10.10.10.21"
export NEW_CONTEXT=cl01
```

### Phase 0 — TrueNAS prep

1. Create dataset `/mnt/SSD/cluster` on the SSD pool; owner `PUID:PGID`.
2. Create datasets `/mnt/SSD/db/{immich,homeassistant,gitea,kuma}`.
3. Create the NFS export for `/mnt/SSD/cluster`: allowed hosts `0.0.0.0/0`, `rw`,
   `sec=sys`, `nfsvers=4`, `mapall` → `PUID:PGID`.
4. Deploy the database custom app: the four containers, host-path mounts to the
   `db` datasets, ports bound on `10.10.10.60`, firewall restricted to
   `10.10.10.21-23`. The compose app is `truenas-databases/`. Three Postgres
   instances cannot share one host IP and port, so each gets its own: HA 5432,
   immich 5433, gitea 5434, kuma 3306.

**Verify:**
- `showmount -e 10.10.10.60` lists `/mnt/SSD/cluster`.
- From a node that can reach it, the export mounts read/write and a test file
  created from two different hosts is owned by `PUID:PGID`.
- `nc -z 10.10.10.60 5432` (and `3306`) succeeds from the swarm subnet only.

### Phase 1 — Build the single-node swarm on cl01

1. Confirm the old swarm is healthy before touching anything:
   `DOCKER_CONTEXT=$OLD_CONTEXT docker node ls` and `docker service ls` — all
   services converged.
2. Remove cl01 from the old swarm, leaving cl02/cl03 as its two managers:
   `DOCKER_CONTEXT=$OLD_CONTEXT docker node demote cl01` then `docker node rm cl01`.
   (Or `docker swarm leave --force` on cl01 after everything is frozen there.)
3. Render cl01's Butane config (common + cl01 values) and transpile:
   `butane --pretty --strict cl01.bu -o cl01.ign`.
4. Install Flatcar on cl01 with `cl01.ign` (delivery mechanism out of scope).
5. Create the overlay network the stacks expect:
   `DOCKER_CONTEXT=$NEW_CONTEXT docker network create --driver overlay --attachable main`.

**Verify:**
- `/mnt/nas` is mounted and listed by `findmnt`.
- Docker's unit requires the mount: `systemctl show docker -p Requires` includes
  `mnt-nas.mount`, and `/mnt/nas` is non-empty inside a container.
- `docker node ls` shows one manager; `main` network exists.

### Phase 2 — Move services one at a time

Order (least-coupled first): **gitea → uptime-kuma → apprise → sponsorblock →
media → homeassistant → immich.**

Repeatable procedure for each service/stack:

1. **Freeze** it on the old swarm:
   `DOCKER_CONTEXT=$OLD_CONTEXT docker service scale <stack>_<svc>=0`.
2. **Copy config** from CephFS to the NFS dataset, from an old node that has
   both (CephFS is shared, so one node is enough):
   `ssh cl02 'sudo mount -t nfs -o nfsvers=4 10.10.10.60:/mnt/SSD/cluster /mnt/nas && sudo rsync -aHAX --numeric-ids /mnt/cephfs/<dir>/ /mnt/nas/<dir>/'`.
3. **DB** (if it has one): dump from the old container, restore into the TrueNAS
   DB. Postgres: `pg_dump -U <user> <db> | gzip`, then
   `zcat | psql -h 10.10.10.60 -U <user> <db>`. Kuma:
   `mariadb-dump`, restore to `10.10.10.60`.
4. **Edit compose** for that stack (see per-service notes below).
5. **Deploy** to the new swarm, with a temporary host port for testing:
   `DOCKER_CONTEXT=$NEW_CONTEXT ./deploy.sh <stack>`.
6. **Verify**, then remove the temporary host port and redeploy.

Rules: only **one instance** of an app may run at a time (its config is now the
shared NFS copy); the old swarm is **never redeployed** (it keeps its
last-applied specs); if a service misbehaves, scale it back up on the old swarm
after restoring its config.

**Per-service notes:**

- **gitea (2.1)** — config `gitea-config`; DB `gitea-postgres`. Remove the `db`
  service; set `GITEA__database__HOST=10.10.10.60:5434`.
- **uptime-kuma (2.2)** — config `uptime-kuma`, `autokuma-data`; DB
  `kuma-mariadb`. Remove `mariadb`; set `UPTIME_KUMA_DB_HOSTNAME=10.10.10.60`.
- **apprise (2.3)** — config `apprise-config`, `apprise-attachments`; no DB.
  Path renames only.
- **sponsorblock (2.4)** — config `sponsorblock`; no DB. Path rename only.
- **media (2.5)** — configs `transmission`, `prowlarr-config`, `radarr-config`,
  `sonarr-config`, `lidarr-config`, `swiparr`; no DB. Blob NFS volumes
  (`downloads*`, `media*`) unchanged.
- **homeassistant (2.6)** — configs `homeassistant`, `mosquitto`, `trmnl`,
  `tasmoadmin-config`, `predbat`; DB `homeassistant-postgres`. Remove the
  `postgres` service and the `depends_on`. In the **home-assistant** repo,
  change `static/recorder.yaml` to `@10.10.10.60:5432`, then rebuild and upload.
- **immich (2.7)** — DB `immich-postgres`. Set `DB_HOSTNAME=10.10.10.60` and
  `DB_PORT=5433` on the server and microservices; remove the `immich-postgres`
  service and `pgdata`
  volume; change `ml-cache` to a plain node-local volume. `photos`/`uploads_*`
  NFS volumes unchanged.

**Verify per service:** container healthy; data present (Gitea repos, Kuma
monitors, HA history, Immich library, *arr configs); the service answers on the
temporary host port; logs free of DB errors.

### Phase 3 — Migrate Traefik and move the VIP

1. Ensure `traefik-config` and `traefik-config-letsencrypt` (including
   `acme.json`) are copied to `/mnt/nas`.
2. Deploy traefik to the new swarm: `DOCKER_CONTEXT=$NEW_CONTEXT ./deploy.sh traefik`.
3. Verify against cl01's IP before moving traffic:
   `curl -H 'Host: gitea.danhughes.dev' http://10.10.10.21/` returns the app.
4. Stop keepalived on the old cluster (find how it is actually running first —
   its Ansible role is commented out).
5. Enable and start keepalived on cl01. The `10.10.10.20` VIP moves; DNS already
   points at it, so no DNS change.
6. Scale the old swarm's services to 0. The old cluster is now idle.

**Verify:** `10.10.10.20` is on cl01; every `*.danhughes.dev` hostname resolves
and serves from the new swarm; certs are valid (copied `acme.json`).

### Phase 4 — Expand to three nodes

1. Get the manager join token on cl01: `docker swarm join-token manager -q`.
2. Render cl02/cl03 Butane configs with that token and keepalived `BACKUP`.
3. Reinstall cl02/cl03 as Flatcar with their configs. They leave the old swarm
   (which then dies) and join the new one.
4. Rebalance service placement: `./rebalance.sh` (with `DOCKER_CONTEXT=$NEW_CONTEXT`).

**Verify:** `docker node ls` shows three managers, all `Ready`/`Active`;
keepalived runs on all three with cl01 `MASTER`; VIP present; services healthy
after rebalance.

### Phase 5 — Retire the old

1. Confirm nothing still runs on the old cluster.
2. Decommission MicroCeph and reclaim the node disks — **keep the Ceph data
   until everything has been verified.**
3. Remove the old docker context.

**Verify:** all services on the new swarm, all data intact, configs editable from
a workstation over NFS.

## 5. Risks and accepted trade-offs

- **SQLite on NFS** — mitigated by single replica and correct locking; already a
  known risk on CephFS (a past HA recorder corruption is noted in
  `predbat/apps.yaml`).
- **TrueNAS is a SPOF** for every database and all config.
- **The config export is open to any host** — anything on the LAN can read/write
  app config as `PUID:PGID`, including embedded secrets.
- **`mapall` means any container can write any file on the shared dataset.**
- **Baked join token** goes stale if the swarm is re-initialised.
- **keepalived** depends on a community sysext and on the `auth_pass` being
  injected at render time; it must stay stopped on cl01 until Phase 3 or both
  clusters claim the VIP.
- **Old-swarm quorum** — after cl01 leaves, the old swarm runs on two managers,
  so a single failure freezes its services.
- **Two swarms, two contexts** — always set `DOCKER_CONTEXT` explicitly.
- **Ignition runs at first boot only** — anything added later (keepalived on
  cl01) has to be include-but-disabled, or applied out of band.
- **Weekly reboot timer vs Flatcar's update reboots** — resolved by staggering.
  `/etc/flatcar/update.conf` sets `REBOOT_STRATEGY=reboot` with a one-hour
  maintenance window per node (cl01 02:00, cl02 03:00, cl03 04:00), matching the
  weekly reboot timer, so the managers never reboot together. `etcd-lock` was
  rejected: it needs an etcd cluster and this stack runs Docker Swarm.
- **NFS version on Flatcar** — 4.1/4.2 kernel regression; pin 4.0.
- **DB network exposure** — firewalled to the swarm subnet.

## 6. Open items

- The literal `PUID:PGID` values and the TrueNAS user they map to.
- Final dataset naming under the SSD pool.
- The LAN interface name (the old role used `ansible_default_ipv4.interface`).
- The manager join token for the baked follower configs.
- How keepalived is currently running on the old cluster, so it can be stopped
  cleanly at Phase 3.
- The Ignition **delivery** mechanism (out of scope; assumed to exist).
