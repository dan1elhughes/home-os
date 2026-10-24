# Auto-deploy from GitHub via doco-cd — design

2026-10-24

## Intent

Merging a PR should reach the swarm with no laptop in the loop. Today the last
step needs a human: `stacks/deploy.sh` re-executes itself under `op run`, so a
1Password session on the Mac is required for every deploy.

The chosen shape: `doco-cd` runs in the cluster, polls `master`, and applies the
repository to the swarm using the same 1Password values as today. The Renovate
PR flow stays; merging stays manual.

## Goals

- A merge to `master` deploys itself within one poll interval.
- Deploy-time values keep coming from 1Password, resolved in the cluster.
- Dan can still deploy a local, uncommitted change to the real stack before
  pushing (the tested behaviour that makes local iteration work).
- Swarm configs still roll automatically when their content changes, and
  superseded ones are pruned.
- One recovery path that does not depend on doco-cd: if the controller is
  broken, a laptop deploy still works.

## Non-goals

- Renovate automerge. Dan merges. That keeps migration-carrying minors (immich,
  gitea) behind a human without a special-case exclusion list.
- Webhooks or any new public ingress. Polling only.
- Drift correction of hand-made live changes. Verified: doco-cd does not do it.
- Notifications, metrics dashboards and the UI beyond what comes free.
- Moving other stacks' secrets or migrating the database hosts.

## Anti-goals

- A laptop deploy path that produces different naming from doco-cd's, so a local
  experiment then double-rolls on the next poll.
- Losing the ability to see why a deploy failed.
- Anything that makes the repo stop being the source of truth.

## Constraints

- The swarm is LAN/VPN only (`DOCKER_CONTEXT=swarm`, `ssh://10.10.10.20`).
- Deploy-time values live in the 1Password environment
  `q5pfvfpsh44h7xywiqys2ff5ma` (account `BMVXTKJWJNHKDFO36EZROR44HM`).
- The repo is public, so the controller clones anonymously; a GitHub token is
  only needed later if API access is wanted.
- `docker secret inspect` does not return payloads, so anything the swarm must
  hold has to be created there, not read back.

## What the test established

Run against gitea with doco-cd `0.129.1`, polling `master` every 60 seconds:

- **Idle polls do nothing.** Jobs finish in ~250 ms when nothing changed.
- **A laptop-shipped change survives idle polls**, indefinitely.
- **It compares the parsed spec, not the commit.** An empty commit and a
  comment-only commit both deployed nothing.
- **A commit that changes that stack's spec deploys and wins** over the laptop
  change, re-adding doco-cd's tracking labels.
- **A doco-cd restart redeploys everything** (its state is in memory). That
  replaces a laptop change too, and is the one non-obvious clobber path.
- **Its labels are cosmetic to it.** A laptop `docker stack deploy` strips the
  `cd.doco.*` labels; doco-cd carried on correctly and re-added them on its next
  deploy.
- **It prunes unused images after a deploy**, a behaviour `deploy.sh` never had.

Assumed, not tested: a commit that touches one stack does not redeploy the
others. Each stack's own inputs are compared, so this should hold. It is checked
in implementation on a second stack before the old path is deleted.

## How it works

### The controller

One stack, `stacks/doco-cd/`:

- Image `ghcr.io/kimdre/doco-cd:<pinned>`, Renovate-managed like everything else.
- Runs on a manager (`node.role == manager`), mounts `/var/run/docker.sock`, and
  a `data` volume for clones and published artifacts.
- No published ports. No ingress. It only polls outward.
- Its own secrets: the GitHub token if it ever needs one, the 1Password service
  account token, and `GIT_ACCESS_TOKEN` only if a private repo appears later.

Configuration for the stacks lives in the repo as a root `.doco-cd.yml` using
`working_dir: stacks` with `auto_discover: true` (depth 1), so each directory
with a compose file becomes a stack named after the directory. Adding a stack
needs no controller change. Exact shape is a builder call; a single root file
beats sixteen per-stack files if auto-discovery covers them.

### Migration staging

Auto-discovery over all sixteen stacks would re-apply the whole fleet on the
first poll: traefik, the DNS services on port 53, Home Assistant and predbat
included, up to four at a time. The cutover is therefore staged, not all at once:

- The root `.doco-cd.yml` starts as an explicit list of deployments — `name`,
  `working_dir`, and `pause: true` on everything not yet migrated.
- Stacks are un-paused one at a time, lowest blast radius first: pairdrop,
  sponsorblock, apprise, then the rest, with predbat, dns and traefik last.
- Once every stack is migrated and verified, the list collapses to
  `auto_discover: true` with `depth: 1`, so adding a stack needs no config edit.

A controller restart re-applying the whole fleet is accepted: unchanged specs do
nothing, and it is no worse than today's full `deploy.sh` run.

### Deploy-time secrets

`external_secrets` in `.doco-cd.yml` maps each variable the compose files
interpolate to an `op://` reference, resolved by a 1Password service account:

```
external_secrets:
  PREDBAT_TOKEN: op://<vault>/<item>/<field>
  ...
```

Those become the interpolation environment for every stack, so the compose files
keep their `${VAR?error}` guards unchanged. `docker stack deploy`-style
interpolation semantics are preserved.

One shared block for all stacks, not per-stack scoping. The blast radius is one
missing item failing every deploy, which is loud rather than silent, and the
alternative is sixteen files to keep in step.

**Single source of truth:** the deploy values move into a 1Password vault as
items the service account can read. The existing environment keeps only what
node bootstrap needs — `KEEPALIVED_PASSWORD`, `SWARM_JOIN_TOKEN`, UPS
credentials, read by `ignition/render.sh` — so no value lives in two places and
rotation has one home. The laptop helper reads the same vault items.

**Unknown to resolve first:** whether the current values already exist as vault
items, or only inside the environment. 1Password environments and vault items
are different features; a service account resolves the latter. Materialising
them into a vault is the first task in the plan, before any stack changes.
Service account support on Dan's 1Password account is confirmed.

**The service-account token** is a hand-created swarm secret, supplied from the
same vault by a small `stacks/doco-cd/bootstrap.sh` that also deploys the
controller stack. It cannot be read back from the swarm, so that script is also
the cold-start step for a rebuilt cluster.

### Configs

Three stacks deliver files into containers as swarm configs today, with
`deploy.sh` content-hashing the names so immutable configs roll:
`homeassistant` (predbat `apps.yaml`), `dns` (five files as one hash),
`overlay-doctor` (`reconcile.sh`, `known_hosts`).

doco-cd does this natively: it suffixes each config with a short sha256 of its
content, rotates on change, and removes the old object. So those compose files
switch from external hash-named configs to plain `file:` references, and
`PREDBAT_APPS_HASH`, `DNS_CONFIG_HASH`, `DOCTOR_SCRIPT_HASH` disappear along with
the env plumbing and the prune functions.

`overlay_doctor_ssh_key` stays a manually created swarm secret (it must never be
in a public repo), so the stack keeps one external secret.

### The laptop helper

A small script replaces `deploy.sh` for the one job doco-cd cannot do: put an
uncommitted working-tree change on the real stack.

- `op run` for the same values, then `docker stack deploy` for the named stack.
- It suffixes `file:` configs with the same content hash doco-cd uses, so a
  stack iterated locally and then committed unchanged produces identical config
  names and does not double-roll.

Mirroring that naming was agreed as the property worth keeping. It is small,
lives next to the stacks, and is the only deploy code left in the repo.

Behaviour to document in `AGENTS.md`: a laptop deploy is temporary. It survives
idle polls, but is replaced by the next commit that changes that stack, and by a
doco-cd restart. No locking: if a laptop deploy races a poll that changed the
same stack, last writer wins. The window is one poll interval and the failure is
visible in the stack's own state; adding a pause mechanism would add a new way
to be stuck.

### Removal

`deploy.sh` goes. `pull-updates.sh` keeps its PR-merging half and loses the
deploy half, or goes too if the merge step moves to the GitHub UI. `start.sh`
stays. `AGENTS.md`'s deploy instructions get rewritten, including the rotation
story and the "temporary by design" rule.

## Failure handling

- A failed deploy is reported by doco-cd's own logs and its stored state; failed
  stacks are visible in `docker service ps`. Failure notification settings are
  a builder call, not a goal.
- Retries are bounded (`retry.limit` around 3, `retry.delay` around 10 minutes)
  rather than doco-cd's unlimited default, so a bad merge cannot thrash a live
  stack every poll. Exhausting the budget leaves it failed until a human acts.
- Nothing is marked deployed on failure.
- If the controller itself is broken, the laptop helper still deploys any stack,
  including `stacks/doco-cd/`, and `bootstrap.sh` recreates the controller's
  secrets from the vault. That is the recovery path and the reason both stay.
- Auto-discover deletion stays **off** (doco-cd's default is `false`). Removing
  a stack directory does not remove a running stack; retiring one stays an
  explicit `docker stack rm`. This is the safe direction for a destructive
  behaviour, and `AGENTS.md` states it.

## Verification before `deploy.sh` is deleted

1. A stack deployed only by doco-cd, on a clean tree.
2. A config-file change rolls a new config and prunes the old (test with dns or
   overlay-doctor, smallest blast radius first, then predbat).
3. A service rename or removal is handled the way we want (orphan pruning).
4. A second stack confirms an unrelated-commit deploy does not touch it.
5. A laptop deploy, then a poll, still leaves the laptop change live.
6. Pulling the controller's 1Password access fails loudly rather than deploying
   with empty values.

## Left to the builder

- Per-stack `timeout` and `force_image_pull`, if any, and the poll interval
  (5 minutes proposed).
- `SELF_UPDATE_ENABLED`/`SELF_UPDATE_STRATEGY` for the controller deploying its
  own stack.
- Whether the laptop helper is bash in `stacks/` or a `docker run` of the
  controller's image with a deploy-only subcommand.
- What to do with `pull-updates.sh` and the `rebalance.sh`/`start.sh` family.
- The exact shape of the explicit deployment list that gets un-paused stack by
  stack, before it collapses to `auto_discover`.
- The exact `AGENTS.md` wording for the behaviours worth warning about:
  temporary laptop deploys, controller restarts redeploying, and directory
  deletion not destroying stacks.

## Steps

- [x] 0. Vault materialisation: the item `home-os deploy`
      (`op://Swarm Doco-CD/home-os deploy/...`, id `u37d6ragzgqne5647xb3kfjc4e`)
      holds the nine secret values: `CF_API_TOKEN`, `HA_RELOADER_TOKEN`,
      `IMMICH_API_KEY_QUEUE_MANAGEMENT`, `MQTT_PASSWORD`, `POSTCODE`,
      `PREDBAT_TOKEN`, `RENOVATE_GIT_ACCESS_TOKEN`, `SOLCAST_API_TOKEN`,
      `TRMNL_ACCESS_TOKEN`. `PUID`/`PGID` are `1000` and `TZ` is
      `Europe/London`, set as plain values, not vault fields. Every reference
      resolves through the service account itself.
- [x] 0b. The service-account token is stored as
      `Swarm Doco-CD service account token` in the Private vault and created as
      the swarm secret `doco_cd_sa_token`.
- [x] 1. Controller deployed (`stacks/doco-cd/`), 1Password provider
      initialised in-cluster, polling `master`. Swarm self-update needs **no**
      `SELF_UPDATE_*` configuration: the swarm's rolling update replaces the
      controller itself (verified, takeover to the newest revision observed).
- [x] 2. First stack (pairdrop) deployed by the controller from `master`.
- [x] 3. All seventeen stacks migrated and un-paused; the owned `name:` hash
      overrides converted to doco-cd content-hash naming; deploy.sh-era config
      objects pruned. Decision change: the deployment list lives in the repo's
      root `.doco-cd.yml` (not inline `POLL_CONFIG` entries), so un-pausing is
      an ordinary commit.
- [x] 4. Laptop helper: `stacks/deploy-local.sh` → `deploy_local.py`.
      Object naming confirmed reproducible:
      `<stack>_<source file basename>_<sha256(file bytes)[:8]>`.
      Verified end to end on overlay-doctor: local edit rolled a fresh config,
      revert rolled back to the canonical object.
- [x] 5. Verification: idle polls no-op; unrelated or no-op commits deploy
      nothing (per-stack input comparison); laptop deploys survive idle polls;
      all stacks healthy. Prepared but impossible to test without breaking
      something: 1Password unavailability failing loudly — accepted as an
      untested item, monitored through deploy failures.
- [x] 6. `deploy.sh` deleted; `pull-updates.sh` now merges and pulls only;
      `AGENTS.md` rewritten for the doco-cd flow.
