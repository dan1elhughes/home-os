# Predbat → Enphase battery control: status & next steps

_Working document. Last updated: 2026-06-27 (overnight session)._

This captures the Predbat integration work in progress so it can be resumed
cleanly. It spans two repos:

- **`home-os`** — the Docker Swarm / Ansible infra repo (this repo).
- **`home-assistant`** (`../home-assistant`) — HA config, built via `build.sh`
  (nunjucks + fragment concat) and deployed via `upload.sh` (rsync to host).

---

## Goal

Run [Predbat](https://github.com/springfall2008/batpred) so it **computes an
optimal battery charge/discharge schedule**, then have a **Home Assistant
automation (`Enphase control v2`) read that schedule and apply it to the Enphase
battery** — declaratively, the same way the existing `Enphase control`
automation applies `sensor.energy_intents_py`.

Predbat itself never commands the battery (Enphase isn't a natively supported
inverter). It is the *brain*; our automation is the *actuator*.

---

## The home energy setup (important context)

- **Inverter/battery:** Enphase Envoy + 3× Encharge (~15 kWh usable, ~9.6 kW).
  Controlled today via `enphase_envoy_cloud_control` (CFG = charge-from-grid,
  DTG = discharge-to-grid schedules).
- **Tariff:** Octopus Energy (BottlecapDave integration), on Intelligent
  (cheap overnight ~3.99p, car charging via Intelligent dispatch slots).
- **Car:** EV on a Zappi (myenergi), ~1.9 kW when charging.
- **Existing optimiser:** `unified_battery_schedule` (in the `home-assistant`
  repo) — a two-pass DP solver that already optimises and controls the battery
  via `sensor.energy_intents_py` → `Enphase control` automation. It is well
  built (tests, repro harness, its own skill) and tuned to this exact hardware.

### CT / sensor topology (the subtle bit)

The Envoy **cannot see the car** (separate circuit) and its "consumption" CT
**already nets battery charge/discharge**. Verified by energy balance:

```
Envoy consumption (≈6.6 kW)  = house + battery_charge − battery_discharge   (NO car)
True grid (Zappi grid CT)    = Envoy consumption + car ≈ 8.6 kW
Pure house load              = Envoy consumption − battery_charge + battery_discharge ≈ 1.9 kW
```

Predbat models the **car** (via Octopus Intelligent) and the **battery**
separately, so it needs **pure house load** for `load_power` and **true grid**
for `grid_power`. Feeding the raw Envoy sensors double-counts the battery and
under-counts the grid by the car.

---

## What has been done

### 1. Predbat added to the `homeassistant` stack ✅
`stacks/homeassistant/docker-compose.yml`:
- Service `predbat` on `nipar44/predbat_addon:slim-v8.42.1`.
- `/config` bind-mounted to `/mnt/cephfs/predbat` (logs, runtime).
- `apps.yaml` delivered as a **Docker Swarm config** (repo isn't on the swarm
  host, so we can't bind-mount it). Config name is content-hashed
  (`predbat_apps_<sha>`); `deploy.sh` computes the hash (`PREDBAT_APPS_HASH`),
  rolls a new config on edit, and prunes superseded configs.
- HA long-lived token injected from **1Password** (`PREDBAT_TOKEN`) via the
  deploy `op run`, written to `/config/secrets.yaml` by an entrypoint shim,
  referenced as `ha_key: !secret ha_key`.
- Traefik route `predbat.danhughes.dev` → 5052.
- `WAIT_FOR_HA_HOST/PORT` watchdog (slim image restarts predbat if HA drops).
- `renovate.json` rule tracks the `slim-v*` tag line.

### 2. Custom `ENPHASE` inverter type ✅
Predbat defaults to a GivEnergy inverter and would crash with *"unable to read
charge window time"*. We declare `inverter_type: ENPHASE` with **all control
capabilities off** (`has_service_api/REST/MQTT: False`,
`output_charge_control: none`, etc.). Predbat then auto-creates dummy entities
and never tries to reach hardware. (`fetch.py`/`inverter.py`/`execute.py`
confirm this is the supported "unsupported inverter" path.)

### 3. Battery model fixed ✅
- `soc_max: 15.0` as a **literal kWh** (the Envoy capacity sensor reports **Wh**
  and `soc_max` has no unit conversion → 15000 was read as 15000 kWh).
- `soc_percent: sensor.envoy_122322027694_battery` (Predbat derives kWh).
- `battery_rate_max: 9600` W (the key Predbat reads on the non-REST path;
  `battery_rate_max_charge/discharge` are NOT read there → it was stuck at the
  GivEnergy 2600 W default).

### 4. Recorder migrated SQLite → Postgres ✅ (the big one)
**Why:** Predbat in control mode fires many concurrent HA history queries.
Against HA's default **SQLite-on-cephfs** recorder this caused
`StaleDataError` then `database disk image is malformed` — the recorder died
(~23:28). HA core kept running; only history was lost.

**What we did:**
- Added an internal `postgres:17-alpine` service to the stack (data on
  `/mnt/cephfs/homeassistant-postgres`, fixed non-secret creds
  `homeassistant`/`POSTGRES`/`homeassistant`, healthcheck, **no ports / no
  Traefik** — overlay-network only). HA `depends_on: postgres`.
- `home-assistant` repo `static/recorder.yaml`:
  `db_url: postgresql://homeassistant:POSTGRES@postgres:5432/homeassistant`.
- Moved the corrupt + leftover SQLite files to
  `/mnt/cephfs/homeassistant/corrupt-db-backup/` (kept for forensics).
- **Validated:** Postgres survives sustained control-mode Predbat load with
  **zero** recorder errors. History API returns 200 (was 500).

> Swarm gotcha: the cephfs bind-mount dir must pre-exist on the node or the task
> is *Rejected*. Created `/mnt/cephfs/homeassistant-postgres` by hand.

### 5. Corrected power-input sensors ✅
`home-assistant` repo `static/template.yaml` — two new template sensors:
- `sensor.predbat_house_load_power` =
  `envoy_consumption − battery_charge + battery_discharge` (pure house).
- `sensor.predbat_grid_power` = `sensor.myenergi_zappi_power_ct_grid` (true grid).

`apps.yaml` now points `load_power`/`grid_power` at these, and `pv_power` at
`sensor.solar_power_generation` (existing repo sensor with staleness guard).
Verified live: Predbat reads `load_power ≈ 1.9 kW`, `grid_power ≈ 8.5 kW` —
physically correct.

### 6. Predbat is producing real schedules ✅
In `Control charge & discharge` + `set_read_only: True`, Predbat computes charge
windows, export windows, SoC trajectory, cost metrics — and commands nothing.
The plan is exposed in `predbat.best_charge_*`, `predbat.best_export_*`,
`predbat.plan_html`, and the `results` attributes of `predbat.charge_limit_kw` /
`predbat.soc_kw_best`.

---

## The current blocker

**The load forecast is bogus** (e.g. `load 108 kWh`, `best_import 108 kWh`)
because the load model is history-driven (`days_previous: 7`) and the **Postgres
DB is only ~1–2 h old** (we nuked the corrupt SQLite). Predbat reports
*"Today's load divergence 100.0%"* and extrapolates garbage. The sensors are
correct; there's simply no history yet.

**Plan: backfill history in Postgres** with a synthetic **flat 0.5 kW** pure
house load (≈12 kWh/day) so `days_previous` returns sane data tonight.

Decision made: **do NOT create a separate sensor** — backfill the existing
`load_today` source directly.

### Backfill spec (next action)
- **Target:** `sensor.envoy_122322027694_energy_consumption_today`
  (this is what `apps.yaml` `load_today` points at), `states_meta.metadata_id =
  361`.
- **Shape:** a **daily-resetting rising kWh counter** (it's
  `total_increasing`): 0 at each **local midnight (Europe/London)**, +0.25 kWh
  per 30-min step, up to ~12 kWh, then resets next local midnight.
- **Range:** last **8 days** (`days_previous` 7 + 1).
- **Do not overwrite** the real recent rows (current real history starts
  ~22:51 UTC today; backfill only *before* that).
- **Insert into `states`:** columns of interest — `metadata_id=361`, `state`
  (the kWh string), `last_updated_ts`/`last_changed_ts`/`last_reported_ts`
  (epoch float), `attributes_id` (reuse an existing kWh-energy attrs row —
  `attributes_id=367` had
  `{"state_class":"total_increasing","unit_of_measurement":"kWh",...}`).
  Leave `event_id` null; set `origin_idx` as other rows do. Watch the
  `state_id` PK (max was ~7005) and any NOT NULL columns.

### Access to Postgres
- Postgres runs on **cl02 = `10.10.10.22`**, container `4d024fcaa5f1`.
- The `main` overlay network is **not manually attachable**, so a local
  `docker run --network main` fails. Reach it via:
  `ssh 10.10.10.22 'docker exec 4d024fcaa5f1 psql -U homeassistant -d homeassistant -c "…"'`
- Swarm nodes: **cl01 = 10.10.10.21, cl02 = 10.10.10.22, cl03 = 10.10.10.23**.
  Swarm manager reachable from this machine via the `swarm` docker context and
  via `ssh 10.10.10.20`.

---

## Remaining work (in order)

1. **Backfill `load_today` history in Postgres** (spec above) so the load
   forecast is sane. Then redeploy / let Predbat re-run, re-assert control mode,
   and confirm `load` forecast ≈ 12 kWh/day and the plan looks reasonable.
2. **Fix PV forecast.** `pv_forecast_today/tomorrow` are unset; Forecast.Solar's
   daily-total sensors (`sensor.energy_production_today/tomorrow`) lack the
   half-hourly `forecast`/`detailedForecast` attribute Predbat needs. Either
   wire a Solcast-style attribute source or synthesize a half-hourly profile.
   (Low urgency at night; matters for daytime plans.)
3. **Consider `load_today` going forward.** The Envoy energy counter has the
   same topology bug (includes battery, excludes car) for *real* future data.
   May want a corrected daily house-load counter eventually (user declined a
   separate sensor, so TBD).
4. **Build `Enphase control v2`** (`home-assistant` repo). Read Predbat's
   **half-hourly plan** (from `predbat.plan_html` or the `results` attributes —
   NOT just `best_charge_start/end`, which are coarse and often empty), convert
   contiguous charge/export runs into the existing
   `events: [{intent: Charge|Discharge, start, end}]` schema, and feed the
   existing Enphase reconciler (`static/automations/energy_intents_schedule.yaml`
   already does the declarative diff → CFG/DTG add/update/delete). Likely write
   to a sensor the v2 automation consumes. **Decide** whether v2 replaces or
   runs alongside the existing DP-driven `Enphase control` (they must not both
   drive the battery).

---

## Safety / current runtime state

- All 6 stack services healthy (1/1). Recorder on Postgres, **0 errors**.
- Predbat: **`Control charge & discharge` + `set_read_only: on`** — planning,
  **commanding nothing**. Safe to leave running; it also warms the load model as
  Postgres accumulates real history.
- `apps.yaml` mode/read-only are also persisted in `predbat_config.json` on the
  cephfs volume, which **overrides `apps.yaml` on restart**. After each redeploy
  we re-assert via the HA entities `select.predbat_mode` and
  `switch.predbat_set_read_only`. (Header comment in `apps.yaml` still says
  "MONITOR / FORECAST ONLY" — **stale, update it**.)

## Uncommitted changes (nothing committed yet)

**`home-os`:**
- `M renovate.json` — predbat tag rule
- `M stacks/deploy.sh` — content-hash + prune for the swarm config
- `M stacks/homeassistant/docker-compose.yml` — predbat + postgres services
- `?? stacks/homeassistant/predbat/` — `apps.yaml` (+ this doc)

**`home-assistant`:**
- `M static/recorder.yaml` — Postgres `db_url`
- `M static/template.yaml` — `predbat_house_load_power`, `predbat_grid_power`

## Key references
- Predbat container app source: `/addon/*.py` (e.g. `fetch.py` mode→calc
  mapping ~L2423; `inverter.py` soc/rate handling; `execute.py` `set_read_only`
  short-circuit ~L62).
- Mode strings: `Monitor`, `Control SOC only`, `Control charge`,
  `Control charge & discharge`. Only the latter calculates both charge & export.
- HA token validated against `https://home.danhughes.dev/api/`.
