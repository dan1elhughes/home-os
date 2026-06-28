# Predbat → Enphase battery control: status & next steps

_Working document. Last updated: 2026-06-27 (daytime session)._

> **2026-06-27 daytime update — read this first.** Several earlier assumptions
> were wrong (the DB was rebuilt and entity ids shifted; the topology was
> mis-described). Corrections + new fixes are in the
> **"2026-06-27 daytime session"** log near the bottom. Headline:
> - **No backfill** was done (user decision). Load + PV forecasts are left to
>   **self-heal as real Postgres history accrues** (`days_previous: 7`).
> - **Topology corrected:** there are **no Enphase microinverters**. PV =
>   **one small 1.2 kWp array → dumb string inverter → Enphase IQ 5P (battery
>   only)**. PV actuals/forecast now both track that single array.
> - **Solcast** wired (cloud-direct). **Grid/PV/car** sensors corrected.
>   **Power-flow signs** fixed. All deployed; Predbat still **read-only**.
> - The "weird plan" (SoC cliff, empty charge windows) was diagnosed as a
>   **downstream symptom of the empty-history bogus load/PV** — not a config
>   bug. It self-heals with history.

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

- **Battery:** Enphase Envoy + Encharge / **IQ 5P (battery only — NO
  microinverters)**, ~15 kWh usable, ~9.6 kW. Controlled today via
  `enphase_envoy_cloud_control` (CFG = charge-from-grid, DTG = discharge-to-grid
  schedules).
- **PV:** **one small ~1.2 kWp array → a dumb (string) inverter → into the
  Enphase IQ 5P.** This is the *only* PV. (Earlier notes claimed Envoy
  microinverter PV producing ~12 kWh/day — **wrong**; the Envoy "production"
  sensor reflects battery/AC flows, not a real array. The myenergi generation CT
  is the correct PV source: `sensor.myenergi_myenergi_hub_generated_today` /
  `_power_generation`.)
- **Tariff:** Octopus Energy (BottlecapDave integration), on Intelligent
  (cheap overnight ~3.99p, car charging via Intelligent dispatch slots).
- **Car:** EV on a Zappi (myenergi), ~1.9 kW when charging. Energy via
  `sensor.myenergi_zappi_charge_added_session` (incrementing, per-session reset).
- **Existing optimiser:** `unified_battery_schedule` (in `home-assistant`) — a
  two-pass DP solver that controls the battery via `sensor.energy_intents_py` →
  `Enphase control`. **Decision: Predbat v2 will REPLACE it** (they must not both
  drive the battery). v2 is a NEW, separate, initially-disabled automation so it
  can be compared/swapped (see Remaining work #4).

### CT / sensor topology (the subtle bit)

The Envoy **cannot see the car** (separate circuit) and its "consumption" CT
**already nets battery charge/discharge**. The **myenergi hub** sits meter-side
and sees the whole site (incl. car). Energy balance verified live 2026-06-27:

```
grid_import + PV(myenergi)        = house + car + battery_charge − battery_discharge
Pure house load (load_power)      = envoy_consumption − battery_charge + battery_discharge
True grid (grid_power)            = myenergi hub import − export   (signed: +import / −export)
PV (pv_power / pv_today)          = myenergi generation CT (the 1.2 kWp array)
```

Predbat models the **car** (via Octopus Intelligent + `car_charging_energy`) and
the **battery** separately, so it needs **pure house load** for `load_power`,
**true signed grid** for `grid_power`, and the **myenergi PV** (not Envoy).

**Sign conventions (verified from addon source):**
- `grid_power`: **+import / −export** (`predbat_metrics.py:101`).
- `battery_power`: **+discharge / −charge** (`execute.py:1014-1015`). Our
  `sensor.battery_total_power` already matches (−ve while charging) — **no
  `battery_power_invert` needed**.
- These two feed only Predbat's **live display + battery-size inference**, NOT
  the forecast/plan.

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
- `sensor.predbat_grid_power` = myenergi hub **import − export** (signed,
  +import/−export). _(2026-06-27: was the raw Zappi grid CT; changed to the
  signed hub value to match Predbat's convention.)_

`apps.yaml` points `load_power`/`grid_power` at these. `pv_power`/`pv_today`
point at the **myenergi generation** sensors (the real 1.2 kWp array), NOT the
Envoy production sensor. _(2026-06-27 corrections — see daytime session log.)_

### 6. Predbat is producing real schedules ✅ (caveat)
In `Control charge & discharge` + `set_read_only: True`, Predbat computes the
plan and commands nothing. The plan is exposed in `predbat.plan_html` (attribute
**`raw`** = full per-30-min table; the richest source) and the `results`
attributes of `predbat.charge_limit_kw` / `predbat.soc_kw_best`, plus the
single-next-window dummies `sensor.predbat_enphase_0_*`.
**Caveat:** while the load/PV forecasts are bogus (no history), the *plan is
empty* (no charge/export windows) — see blocker below.

---

## The current blocker → now DEFERRED (self-heals with history)

**Root cause: the Postgres DB was rebuilt and has almost no history**, so the
**load forecast is bogus** (`days_previous: 7` extrapolates garbage →
*"divergence 100%"*, `load_energy ~68–88 kWh/48h`) and PV calibration can't run.

**Decision (user, 2026-06-27): do NOT backfill.** Let real history accrue;
`days_previous: 7` becomes valid after ~7 days. Predbat stays **read-only**, so
the bogus forecast is harmless. This replaces the earlier synthetic-backfill
plan (which was **never executed**). The backfill spec below is kept only as a
record of the rejected approach.

> ⚠️ **Stale ids in the old backfill spec.** When the DB was rebuilt, entity ids
> shifted: `load_today` source `sensor.envoy_122322027694_energy_consumption_today`
> is now **`metadata_id = 362`** (NOT 361 — 361 is now `current_power_consumption`).
> `attributes_id = 367` happens to still be a valid kWh-energy attrs row used by
> the real 362 rows. `state_id` PK max is ~130k (not ~7k), and it's
> `generated by default as identity` (auto-assigned). Real history for 362 starts
> ~`2026-06-26 22:51 UTC`. Do not trust the numbers in the spec below without
> re-checking.

### Backfill spec (REJECTED — kept for reference only)
- **Target:** `sensor.envoy_122322027694_energy_consumption_today` (`load_today`).
- **Shape:** daily-resetting rising kWh counter (`total_increasing`): 0 at each
  local midnight (Europe/London), +0.25 kWh / 30-min step, up to ~12 kWh.
- **Range:** last 8 days. Do not overwrite real rows. Mind `state_id` PK & NOT
  NULL columns.

### Access to Postgres (still accurate)
- Postgres runs on **cl02 = `10.10.10.22`**, container `4d024fcaa5f1`.
- The `main` overlay net is not manually attachable; reach it via:
  `ssh 10.10.10.22 'docker exec 4d024fcaa5f1 psql -U homeassistant -d homeassistant -c "…"'`
- Swarm nodes: **cl01 = .21, cl02 = .22, cl03 = .23**; manager via the `swarm`
  docker context / `ssh 10.10.10.20`.
- **Schema notes (verified 2026-06-27):** `states` rows use only the `*_ts`
  doubles (legacy `entity_id`/`attributes`/`last_*` columns are NULL).
  `old_state_id` chains the recorder's linked list (irrelevant to history reads).
  HA history queries by `metadata_id` + time range, reading `state` +
  `last_updated_ts`.

---

## Remaining work (in order)

1. **Load + PV forecasts: warming up.** Now driven by CORRECT inputs (see
   2026-06-27/28 log); they tighten as the new pure-house `load_today` counter
   accrues. `days_previous: [1,2,3]` (3-day average) becomes useful after ~1 day.
   The recurring SolarAPI calibration traceback **self-healed** once a few days of
   PV history accrued (PV forecast now ~13 kWh/day). Re-check the plan over the
   next few days; no action needed.
2. **PV forecast — DONE.** Solcast cloud-direct wired; ~13 kWh/day landing.
3. **`load_today` topology bug — DONE.** Replaced the Envoy
   `energy_consumption_today` (which counts battery charging as house load) with a
   **pure-house daily counter**: `sensor.predbat_house_load_today` (utility_meter,
   daily cycle) over `sensor.predbat_house_load_energy` (Riemann integration,
   `method: left`) of `sensor.predbat_house_load_power`. Result: the phantom
   midnight ~12 kW load spike is gone; `load_forecast` fell to ~24 kWh/48h
   (~0.5 kW baseline). **No backfill** — accrues forward only.
4. **Build `Enphase control v2`** (`home-assistant` repo) — NEXT, once the
   forecasts have warmed up and the plan is trustworthy. Read Predbat's
   **half-hourly plan** — the richest source is `predbat.plan_html` attribute
   **`raw`** (`raw["rows"]` = full per-30-min table with `state`/`state_target`/
   soc/cost), or parse the single-next-window dummies `sensor.predbat_enphase_0_*`
   / `predbat.best_charge_*` / `best_export_*`. Convert contiguous charge/export
   runs into the existing `events: [{intent: Charge|Discharge, start, end}]`
   schema and feed the existing reconciler
   (`static/automations/energy_intents_schedule.yaml` → CFG/DTG diff).
   **Decisions made:** v2 **REPLACES** the DP-driven `unified_battery_schedule`
   (not coexist — they must not both drive the battery); build v2 as a **NEW,
   separate, initially-DISABLED** automation + feeder sensor so it can be
   compared and swapped in deliberately.

---

## Safety / current runtime state

- All 6 stack services healthy (1/1). Recorder on Postgres, **0 errors**.
- Predbat: **`Control charge & discharge` + `set_read_only: on`** — planning,
  **commanding nothing**. Safe to leave running; it warms the load model as the
  new counter accrues.
- Predbat moves between swarm nodes on redeploy (has run on cl01/cl02/cl03).
  Find it: `ssh 10.10.10.20 'docker service ps homeassistant_predbat ...'` then
  `docker ps` on that node. Live log: `/config/predbat.log` on the cephfs volume.
- `apps.yaml` mode/read-only are also persisted in `predbat_config.json` on the
  cephfs volume, which **overrides `apps.yaml` on restart**. In practice mode has
  **persisted across every redeploy** this session (no re-assert needed), but if
  it ever resets, re-assert via `select.predbat_mode` +
  `switch.predbat_set_read_only`. (Header comment in `apps.yaml` updated.)

## Committed / uncommitted changes

The **recorder→Postgres + initial Predbat power sensors** are already committed
(`home-assistant` commit `db0b10c`). The 2026-06-27/28 fixes below are
**uncommitted** at time of writing (commit them):

**`home-os`:**
- `M stacks/homeassistant/docker-compose.yml` — `SOLCAST_API_TOKEN` env +
  entrypoint writes `solcast_api_key` to secrets.yaml.
- `M stacks/homeassistant/predbat/apps.yaml` — header fix; Solcast; PV sources
  (myenergi); `inverter_hybrid: False`; car config; pure-house `load_today`;
  `days_previous: [1,2,3]`.
- `M stacks/homeassistant/predbat/STATUS.md` — this doc.
- (`renovate.json`, `stacks/deploy.sh` — predbat tag rule + content-hash/prune;
  may already be committed.)

**`home-assistant`:**
- `M static/template.yaml` — `predbat_grid_power` = myenergi import−export.
- `M static/sensor.yaml` — `predbat_house_load_energy` Riemann integration.
- `M static/configuration.yaml` — `utility_meter: !include utility_meter.yaml`.
- `?? static/utility_meter.yaml` — `predbat_house_load_today` daily meter.

## 2026-06-27/28 daytime session — corrections & fixes

Chronological summary (everything verified live, not assumed):

1. **Backfill cancelled** (user) — load left to accrue; avoided Postgres writes.
   (Old backfill spec above is stale — ids shifted post-rebuild: `load_today`
   source = `metadata_id 362`, not 361.)
2. **Topology corrected** (user): no Enphase microinverters — PV is one **1.2 kWp
   array → dumb inverter → Enphase IQ 5P (battery only)**. Envoy "production"
   ~12 kWh was NOT a real array; the **myenergi generation CT** is the PV source.
3. **Solcast wired** (cloud-direct): `solcast_host`/`solcast_api_key` (1Password
   `SOLCAST_API_TOKEN` → entrypoint → secrets.yaml)/`solcast_poll_hours: 8`. Site
   `1901-9db0-3cec-4015`, 1.2 kWp. **Known transient:** PV Calibration crashes on
   sparse `pv_today` history (upstream bug `solcast.py:943`, `None - None`;
   `metric_pv_calibration_enable` does NOT prevent it) — **self-healed** once a
   few days of PV history accrued (PV now ~13 kWh/day).
4. **PV sources → myenergi:** `pv_today = sensor.myenergi_myenergi_hub_generated_today`,
   `pv_power = sensor.myenergi_myenergi_hub_power_generation`.
5. **Grid sign fixed:** `predbat_grid_power` = myenergi hub **import − export**
   (signed +import/−export, matching Predbat). Battery sign already correct
   (`battery_total_power` −ve=charge = Predbat convention; no invert needed).
6. **Car modelled correctly:** `num_cars: 1`, `car_charging_hold: True`,
   `car_charging_energy = sensor.myenergi_zappi_charge_added_session` (precise
   subtraction; the default 6 kWh threshold never caught the 1.9 kW car).
   `switch.predbat_car_charging_from_battery = off` — car does NOT drain battery.
7. **`inverter_hybrid: False`** — AC-coupled (separate PV inverter), correct.
8. **"Weird plan" diagnosed:** the SoC cliff + empty charge windows were
   **downstream symptoms** of the empty-history bogus load/PV (not a config bug);
   the SoC cliff self-healed ~11:45. **BUT** the persistent root of the
   battery-drain was found: **`load_today` (Envoy consumption) counts battery
   CHARGING as house load.** Proven twice live (`Envoy consumption = pure house +
   battery_charge`) and in the plan (a 9.5 kW overnight battery charge appeared as
   a phantom ~12 kW house-load spike at 23:30–00:00).
9. **`load_today` fixed (the big one):** built a **pure-house daily kWh counter**
   (Riemann integration `method: left` of `predbat_house_load_power` →
   utility_meter daily). No native Envoy/Zappi sensor exists for pure house load
   (would need a hardware total-consumption CT). After deploy: midnight spike
   GONE, `load_forecast` ~24 kWh/48h, mean ~0.5–0.7 kW. **Verified.**
10. **`days_previous: [1,2,3]`** (3-day average, not the single-day `[7]`).

## Key references
- Predbat container app source: `/addon/*.py` (e.g. `fetch.py` mode→calc
  mapping ~L2423; `inverter.py` soc/rate handling; `execute.py` `set_read_only`
  short-circuit ~L62).
- Mode strings: `Monitor`, `Control SOC only`, `Control charge`,
  `Control charge & discharge`. Only the latter calculates both charge & export.
- HA token validated against `https://home.danhughes.dev/api/`.
