# Agent notes — home-os

## Deployment: doco-cd

Deploys are GitOps: `doco-cd` runs in the cluster (`stacks/doco-cd/`), polls
`master` every 5 minutes, and applies each stack directory from the repository
(see `stacks/doco-cd/` and its `.doco-cd.yml` at the root). Its secrets resolve
from the 1Password vault `Swarm Doco-CD` through a service account; the token
is the swarm secret `doco_cd_sa_token` (also stored in the Private vault item
`Swarm Doco-CD service account token`).

- **Merge instead of deploy.** After merging, wait for the next poll; report
  progress with `docker service logs doco-cd_doco-cd` and `docker service ps`.
- **Local iteration before a push:** `cd stacks && ./deploy-local.sh <stack>`
  deploys the working tree to the real stack, with 1Password values. Config
  objects are named exactly as doco-cd names them, so committing the same
  content afterwards does not double-roll.
- **A laptop deploy is temporary.** It survives idle polls; the next commit
  that changes that stack, or a doco-cd restart (its state is in memory, so a
  restart reapplies everything), replaces it. Commit to make it stick.
- **New deploy-time variables:** add the `${VAR?error}` reference to
  `.doco-cd.yml` as `op://Swarm Doco-CD/home-os deploy/<VAR>` and add a field
  of the same name to that vault item.
- Removing or renaming a stack directory does **not** remove the running stack
  (auto-discover deletion stays off). Retire stacks with `docker stack rm`.
- The controller is the only thing with 1Password cluster access. It is the
  only writer of configs except the laptop helper, so stale objects are rare.

## Live-system safety

- For a single service-label change, `docker service update --label-add …`
  avoids a full stack deployment. Keep the
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

## Swarm overlay network

**Symptom:** a service is healthy and answers from some nodes, but containers
on one node get connection timeouts to it by service name. The overlay VXLAN
forwarding database (FDB) on the *source* node can keep a stale entry after a
task moves to another node; packets then go to the wrong node and are dropped.
Seen with engine 28.2.2 on 2026-10-24: four stale entries blackholed
cl02→lidarr, cl02→radarr and cl03→swarm-cronjob for hours to weeks. A task
restart does NOT fix it, and nothing rewrites the kernel entry by itself.

Nodes: cl01=10.10.10.21, cl02=10.10.10.22, cl03=10.10.10.23; 10.10.10.20 is a
VIP held by cl01 (the `swarm` docker context points at it). The `main`
overlay sandbox on each node is `/var/run/docker/netns/1-nns5xsdm8h`.

- Check the FDB on a source node:
  `sudo nsenter --net=/var/run/docker/netns/1-nns5xsdm8h bridge fdb show | grep ' dst '`
  Each remote endpoint MAC must point at the node that runs the task. Get the
  endpoint MAC and node from `docker network inspect main` on each node and
  `docker service ps`. Locally-hosted MACs must have no `dst` entry.
- Fix a wrong remote entry:
  `sudo nsenter --net=/var/run/docker/netns/1-nns5xsdm8h bridge fdb replace <mac> dev vxlan0 dst <node-ip> self permanent`
- Delete a stale entry for an endpoint that now runs on the same node:
  `sudo nsenter --net=/var/run/docker/netns/1-nns5xsdm8h bridge fdb del <mac> dev vxlan0 dst <old-node-ip> self permanent`

`stacks/overlay-doctor` does exactly this automatically every two minutes and
logs each repair; see its README for the checks and how to remove it once
Flatcar ships docker 29.9.0.

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
