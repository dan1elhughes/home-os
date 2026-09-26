# TrueNAS databases

The five database servers that used to run on the swarm, as one TrueNAS SCALE
custom app (runbook Phase 0). Apps connect over TCP, so no database data
directory lives on NFS.

This directory sits outside `stacks/` on purpose. `stacks/deploy.sh` iterates
every directory under `stacks/`, so a compose file here is never deployed to
the swarm.

## Images and data

| Container | Image | Data path on NAS | Host port |
|---|---|---|---|
| `homeassistant-postgres` | `postgres:18-alpine` | `/mnt/SSD/db/homeassistant` | 5432 |
| `immich-postgres` | `ghcr.io/immich-app/postgres:18-vectorchord0.5.3` | `/mnt/SSD/db/immich` | 5433 |
| `gitea-postgres` | `postgres:18.6` | `/mnt/SSD/db/gitea` | 5434 |
| `mcpjungle-postgres` | `postgres:18.6` | `/mnt/SSD/db/mcpjungle` | 5435 |
| `kuma-mariadb` | `mariadb:12.3` | `/mnt/SSD/db/kuma` | 3306 |

## Deviation from the runbook

The runbook says "ports `5432`/`3306` bound on `10.10.10.60`" and gives per-app
hosts of `10.10.10.60:5432`. Four Postgres instances cannot share one port on
one host IP, so each gets its own port. `homeassistant-postgres` keeps 5432 so
the recorder change in the `home-assistant` repo (`@10.10.10.60:5432`) stays
correct. The `immich`, `gitea` and `mcpjungle` compose files in this repo use
5433, 5434 and 5435. Confirm this scheme at Phase 0 or replace it with
per-container IPs.

## Deploy on TrueNAS

1. Create `/mnt/SSD/db/{immich,homeassistant,gitea,kuma,mcpjungle}`.
2. Give each dataset an owner the container runtime can write to. The official
   Postgres and MariaDB images run as UID/GID 999; either chown the datasets to
   999:999 or run the app under the same `PUID:PGID` used elsewhere.
3. Create a custom app from this compose file (TrueNAS: Apps > Custom App, or
   `docker compose up -d` over SSH).
4. Restrict inbound 5432-5435 and 3306 to the swarm subnet (10.10.10.21-23).

## Verify

```sh
nc -z 10.10.10.60 5432 && nc -z 10.10.10.60 5433 \
  && nc -z 10.10.10.60 5434 && nc -z 10.10.10.60 5435 \
  && nc -z 10.10.10.60 3306
```

Then dump each database from the old swarm and restore here before starting the
migrated app (runbook Phase 2). Credentials are unchanged from the old compose
files on purpose (runbook decision 10).
