# TrueNAS databases

The database servers that back the swarm apps, as one TrueNAS SCALE custom app
(runbook Phase 0). Apps connect over TCP, so no database data directory lives on
NFS. `ombi-postgres` was added later (2026-10-09) for Ombi; the other five moved
off the swarm in Phase 0.

**Deployed on TrueNAS 2026-09-26 (runbook Phase 0 executed).**

This directory sits outside `stacks/` on purpose. `stacks/deploy.sh` iterates
every directory under `stacks/`, so a compose file here is never deployed to
the swarm.

## Images and data

| Container | Image | Data path on NAS | Host port |
|---|---|---|---|
| `homeassistant-postgres` | `postgres:18-alpine` | `/mnt/SSD/local/cluster-db/homeassistant` | 5432 |
| `immich-postgres` | `ghcr.io/immich-app/postgres:18-vectorchord0.5.3` | `/mnt/SSD/local/cluster-db/immich` | 5433 |
| `gitea-postgres` | `postgres:18.6` | `/mnt/SSD/local/cluster-db/gitea` | 5434 |
| `mcpjungle-postgres` | `postgres:18.6` | `/mnt/SSD/local/cluster-db/mcpjungle` | 5435 |
| `ombi-postgres` | `postgres:18.6` | `/mnt/SSD/local/cluster-db/ombi` | 5436 |
| `kuma-mariadb` | `mariadb:12.3` | `/mnt/SSD/local/cluster-db/kuma` | 3306 |

## Deviation from the runbook

The runbook says "ports `5432`/`3306` bound on `10.10.10.60`" and gives per-app
hosts of `10.10.10.60:5432`. Several Postgres instances cannot share one port on
one host IP, so each gets its own port. `homeassistant-postgres` keeps 5432 so
the recorder change in the `home-assistant` repo (`@10.10.10.60:5432`) stays
correct. The `immich`, `gitea`, `mcpjungle` and `ombi` compose files in this
repo use 5433, 5434, 5435 and 5436. Confirm this scheme at Phase 0 or replace it
with per-container IPs.

## Deploy on TrueNAS (done 2026-09-26)

1. Datasets `/mnt/SSD/local/cluster-db/{immich,homeassistant,gitea,kuma,mcpjungle,ombi}`.
   Grouped with the cluster dataset under `SSD/local` so the NAS backup regime
   covers it (the runbook originally said `/mnt/SSD/db/*`). Parent dataset
   stays root-owned; only the leaves are mounted.
2. Owner `999:999` per leaf dataset. On this NAS that is user `netdata` +
   group `docker` (UID 999 already belonged to netdata, GID 999 to the docker
   group, and TrueNAS refuses duplicate IDs — the names are cosmetic).
3. Custom app from this compose file (Apps > Custom App, Install via YAML).
   TrueNAS's UI prefixes the project `ix-cluster-databases-*`.
4. Port restriction to the swarm subnet (10.10.10.21-23): **skipped** — the
   SCALE UI offers no supported firewall/ACL mechanism for custom-app port
   bindings. The ports answer from any LAN host. Decide at Phase 1.

## Verify

```sh
nc -z 10.10.10.60 5432 && nc -z 10.10.10.60 5433 \
  && nc -z 10.10.10.60 5434 && nc -z 10.10.10.60 5435 \
  && nc -z 10.10.10.60 5436 && nc -z 10.10.10.60 3306
```

Then dump each database from the old swarm and restore here before starting the
migrated app (runbook Phase 2). Credentials are unchanged from the old compose
files on purpose (runbook decision 10).

As of Phase 0 the ports answer from any LAN host (restriction skipped — step 4
above), so this check succeeds from the workstation too.
