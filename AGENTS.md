# Agent notes — home-os

Docker Swarm / Ansible infra. Stacks live under `stacks/<name>/`; deploy with
`stacks/deploy.sh <stack>` (wraps `op run` for 1Password secrets, content-hashes
any `predbat/apps.yaml` into an immutable swarm config, prunes superseded ones).

## Swarm access

- Nodes: **cl01 = 10.10.10.21, cl02 = 10.10.10.22, cl03 = 10.10.10.23**.
- Manager: `ssh 10.10.10.20`, or the local `swarm` docker context
  (`docker context use swarm`).
- Services move between nodes on redeploy. To find a service's container:
  `ssh 10.10.10.20 'docker service ps <svc> --filter desired-state=running'`,
  then `docker ps` / `docker exec` on that node. Don't assume a fixed node/id.

## Controlling Home Assistant

HA runs in the `homeassistant` stack; config is in the **`../home-assistant`**
repo (built with `./build.sh`, deployed with `./upload.sh` — rsync to
`/mnt/cephfs/homeassistant` on the host). Operate it via its REST API:

- **Base URL:** `https://home.danhughes.dev/api/` (Traefik). A long-lived token
  is required (`Authorization: Bearer <token>`); it lives in the 1Password env
  used by `deploy.sh` as `PREDBAT_TOKEN`, e.g.
  `op run --environment <env> --account <acct> -- bash -c 'curl -s -H "Authorization: Bearer $PREDBAT_TOKEN" …'`.
- **Read entity state:** `GET /api/states/<entity_id>` (or `/api/states` for all).
  The `attributes` object holds the useful structured data (e.g. flow breakdowns,
  `results` time-series, schedule lists).
- **Read history:** `GET /api/history/period/<start_iso>?filter_entity_id=<id>&end_time=<end_iso>&minimal_response`.
- **Call a service:** `POST /api/services/<domain>/<service>` with a JSON body.
  Commonly used here:
  - `template/reload` — reload `template.yaml` sensors (no restart needed).
  - `homeassistant/check_config` (`POST /api/config/core/check_config`) — ALWAYS
    validate before a restart on this live system.
  - `homeassistant/restart` — needed for platform sensors / `utility_meter` /
    new `!include`d top-level keys (template reload does NOT pick those up).
- **Evaluate a template** (handy for the device/entity registry):
  `POST /api/template` with `{"template": "{{ device_entities('<device_id>') | join('\n') }}"}`
  or `device_attr('<device_id>', 'name')`. Device IDs are HA registry ids, not
  containers.

Recorder runs on Postgres (internal `homeassistant_postgres` service, overlay-net
only — not manually attachable). Reach it on whichever node it's on:
`ssh <node> 'docker exec <pg_container> psql -U homeassistant -d homeassistant -c "…"'`.
Sanity-check schema and back up before any writes — this is a live home system;
confirm before anything destructive.

## Reading Predbat's plan

Predbat (`homeassistant_predbat`) runs the optimiser in
`Control charge & discharge` + `set_read_only: on` — it **plans only, commands
nothing**. `apps.yaml` is in `stacks/homeassistant/predbat/`; mode/read-only are
also persisted in `predbat_config.json` on the cephfs volume and override
`apps.yaml` on restart (re-assert via `select.predbat_mode` +
`switch.predbat_set_read_only` if it ever drifts).

Read the plan via the HA API:

- **Full half-hourly plan:** `predbat.plan_html`, attribute **`raw`** — the
  richest source. `raw["rows"]` is one dict per 30-min slot with `time`, `state`
  (`Demand`/`Chrg`/`Exp`/`FrzChrg`/`HoldChrg`/…), `state_target` (target SoC% or
  export floor%), `soc_percent`, `load_forecast`, `pv_forecast`, `import_rate`,
  `export_rate`, `total_cost`. `raw` also has top-level `soc`, `soc_max`,
  `reserve`, `end_record`, `totals`.
- **Next charge/export window (coarse):** `predbat.best_charge_start/end/limit`,
  `predbat.best_export_start/end/limit`. Often empty (`[]`) when the optimiser
  decides not to act — check `predbat.status.attributes.debug`
  (`best_charge_window=[]` etc.).
- **Time-series:** the `results` attribute on `predbat.soc_kw_best`,
  `predbat.charge_limit_kw`, `predbat.load_energy`, `predbat.pv_energy`, … — a
  change-point-filtered dict keyed by ISO timestamp (forward-fill between keys).
- **Decision trace:** the live log at `/config/predbat.log` on the node running
  predbat (large; grep it). Useful lines: `Raw/Unclipped/Filtered charge
  windows`, `Best charging limit SoC's`, `Import rates: min … max …`, `Today's
  load divergence …`. An empty `Filtered charge windows [ ]` with `@ Xp 0%` raw
  windows means the optimiser set every charge target to 0 (no economic benefit),
  not that it failed to see the cheap window.

Predbat itself never actuates the Enphase battery (unsupported inverter); a
separate HA automation translates the plan into Enphase CFG/DTG schedules.
