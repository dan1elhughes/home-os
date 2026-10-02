# Agent notes — home-os

## Deployment and live-system safety

- Run `DOCKER_CONTEXT=swarm ./deploy.sh <stack>` from inside `stacks/`.
  Running it from the repository root breaks path resolution.
- For a single service-label change, `docker service update --label-add …`
  avoids a full stack deployment and its 1Password requirements. Keep the
  repository configuration consistent with any live change.
- Discover the current node and container with `docker service ps` and
  `docker ps` before using `docker exec`. Do not assume fixed placement or IDs.
- Create shared bind-mount directories on one node only.
- Check the database schema and take a backup before writes. Discover the
  actual database container on TrueNAS rather than assuming its name.
  Ask for confirmation before destructive operations on this live home system.
- Do not run `docker swarm init` on an existing cluster node.
- `ignition/out/` contains secrets. Do not commit it or print its contents.
- Check `journalctl -u ups-watcher -u ups-undrain` before changing a node's
  drain state; do not override a power-protection drain without checking NUT.
- Changes to power-control source do not update the binaries embedded by
  Ignition. Rebuild the binaries and update their checksums when changing them.

## Traefik (ingress)

For Bad Gateway errors, check the service's backend port label against the
container's actual listening port with `ss -tlnp` or `netstat`.

**Cluster service names under `*.danhughes.dev` are internal, accessible
from the LAN or VPN.**
Treat the domain as private network space. Services exposed on it do not need
their own auth, TLS termination beyond Traefik's existing LetsEncrypt cert, or
egress hardening, and there is no public-internet attack surface to worry
about. Don't add password/2FA layers on top unless the service itself demands it
(e.g. an AI agent that can run shell — then add `OPENCODE_SERVER_PASSWORD`
even on the internal domain, because anything on the VPN can reach it).

## Controlling Home Assistant

HA configuration lives in the sibling **`../home-assistant`** repository.
Build with `./build.sh` and deploy with `./upload.sh` there.

- Use the 1Password `PREDBAT_TOKEN` as the bearer token for the HA REST API.
- Always validate with `POST /api/config/core/check_config` before a restart.
- Prefer `template/reload` for template changes. Platform sensors,
  `utility_meter`, and new top-level `!include` keys require a restart.
- Do not edit generated `/config/secrets.yaml` files manually; container
  startup overwrites them.

## Reading Predbat's plan

**Values changed via the Predbat HA UI persist in `predbat_config.json` and
override `apps.yaml` on restart.** If the configuration and plan disagree,
check the persisted JSON before changing `apps.yaml`.

Read the plan via the HA API:

- **Full half-hourly plan:** `predbat.plan_html`, attribute **`raw`**.
  `raw["rows"]` contains each slot's state, target SoC/export floor, forecasts,
  rates, and cost. Prefer it to the coarse next-window entities.
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
