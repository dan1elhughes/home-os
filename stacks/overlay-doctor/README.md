# overlay-doctor

Self-repair for stale overlay VXLAN entries (Uptime Kuma reporting a service
down while the service is actually healthy).

## Why this exists

Docker Engine can leave a node's overlay forwarding database (FDB) pointing at
an old node after a task moves. Traffic from that node is encapsulated to the
wrong host and dropped, so every container on it times out against a service
that works everywhere else. Upstream: [moby#52661](https://github.com/moby/moby/issues/52661).
The fixes ([#53663](https://github.com/moby/moby/pull/53663),
[#53420](https://github.com/moby/moby/pull/53420)) shipped in Engine 29.9.0.

The cluster runs Flatcar 4757, which bundles docker 28.2.2 — before those
fixes — so this service repairs the entries itself until the engine ships.

## How it works

Every 120s, from one container on whichever node the scheduler picks:

1. One ssh round trip per node gathers that node's live endpoints on `main`
   (`docker network inspect`) and its overlay FDB (`bridge fdb show` inside the
   network sandbox).
2. It builds the truth table `endpoint MAC -> node that runs it`. Endpoint MACs
   derive from the endpoint IPv4, and `main-endpoint` (the per-node LB endpoint)
   is included, so every legitimate remote entry is covered.
3. For each node it repairs rows that disagree: a wrong or missing `dst` gets
   `bridge fdb replace`, and a leftover `dst` for an endpoint that now runs on
   that node gets `bridge fdb del`.

Guards, so a partial view can never cause a wrong write:

- A cycle is skipped unless all three nodes answer.
- A cycle is skipped below `DOCTOR_MIN_ENDPOINTS` (default 10) endpoints.
- Only MACs present in the truth table are touched. Entries for MACs that no
  live endpoint claims are counted (`unknown=`) and left alone — they are
  leftovers from long-dead containers and harmless.
- An endpoint seen on two nodes at once (mid-reschedule) is counted
  (`conflicts=`) and skipped for that cycle.

Repairs are logged as `fixed node=… action=… mac=… dst=…`; each clean cycle logs
one summary line. A healthcheck watches a heartbeat file the loop writes, so a
broken ssh path or a revoked key shows up as an unhealthy service rather than
silent silence.

## Deploy

```sh
cd stacks && DOCKER_CONTEXT=swarm ./deploy.sh overlay-doctor
```

`deploy.sh` content-hashes `reconcile.sh` and `known_hosts` into the swarm
config names, so editing either rolls the service and prunes the old pair.

The ssh key is a swarm secret (`overlay_doctor_ssh_key`). It is not in git. One
time only, this is how it was made:

```sh
ssh-keygen -t ed25519 -N '' -C overlay-doctor -f ./id_ed25519
docker secret create overlay_doctor_ssh_key ./id_ed25519 && rm ./id_ed25519
```

Then append the `.pub` to `core`'s `~/.ssh/authorized_keys` on all three nodes
(and to `ignition/files/authorized_keys` so a reinstall keeps it — note
`render.sh` normally fetches keys from `danhughes.dev/keys`, so a reinstall
built from that URL also needs the key added there).

`known_hosts` pins the three node host keys; it was made with
`ssh-keyscan -t ed25519 10.10.10.21 10.10.10.22 10.10.10.23`. If a node is
rebuilt, ssh fails against its new key and cycles are skipped (fail-safe).
Regenerate the file and redeploy.

## Check it

```sh
docker service logs overlay-doctor_overlay-doctor --tail 20
```

Healthy output is one line per cycle, `fixes=0` when nothing is wrong:

```
cycle nodes=3 endpoints=43 conflicts=0 unknown=360 fixes=0 dry_run=0
```

Dry run (logs the plan, writes nothing) without a redeploy:

```sh
docker service update --env-add DOCTOR_DRY_RUN=1 --detach=false overlay-doctor_overlay-doctor
```

The plan logic can also be run against a saved `bridge fdb show` dump, with a
truth file of `mac node-ip` lines:

```sh
sh reconcile.sh --plan 10.10.10.22 truth.txt fdb-dump.txt
```

## Remove it

Delete it once the engine carries the fix — i.e. when Flatcar ships docker
29.9.0 or later (`docker version` on a node). Confirm first that the upstream
signature is gone from the daemons for a few weeks:

```sh
sudo journalctl -u docker --since "14 days ago" | grep -c 'Neighbor entry already present'
```

Then:

```sh
cd stacks && docker stack rm overlay-doctor        # also removes its configs
docker secret rm overlay_doctor_ssh_key
```

and remove the `overlay-doctor` line from `~/.ssh/authorized_keys` on all three
nodes plus `ignition/files/authorized_keys` in this repo (and from
`danhughes.dev/keys` if that is where the render gets them). Delete this
directory. Nothing else references it.
