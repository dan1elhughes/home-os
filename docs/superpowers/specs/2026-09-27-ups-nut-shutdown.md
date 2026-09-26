# UPS-triggered shutdown and power-on for the swarm

Status: approved design, ready for planning
Date: 2026-09-27

The three swarm nodes sit on a CyberPower UPS. Today nothing watches it: if an
outage outlasts the battery, the UPS cuts output and every node takes a hard
power cut. This spec adds shutdown (immediately on battery for cl02/cl03, at
battery low for cl01) and power-on (Wake-on-LAN from cl01, BIOS AC-restore as
the deep fallback). The Pi keeps its current role: serve UPS status.

## 1. Context (current state, verified)

- **Nodes**: cl01/cl02/cl03, Flatcar Container Linux, Ignition-provisioned
  (`ignition/`), NUC-like mini PCs. cl01 is the swarm manager; the LAN is
  entirely UPS-protected (switch included).
- **NUT server**: Raspberry Pi 2 Model B, `pi-nut` (10.10.10.12), Debian,
  NUT packaged install. UPS name `ups`. Driver `usbhid-ups`
  (`vendorid 0764`, `productid 0501`), UPS connected by USB to the Pi.
  `upsd` listens on `0.0.0.0:3493`. One user, `[upsmon]` (password `upsmon`),
  running `upsmon primary` on the Pi with `SHUTDOWNCMD "/sbin/shutdown -h +0"`
  and `POWERDOWNFLAG /etc/killpower` — the Pi already shuts itself down and
  killpowers the UPS at battery low.
- **UPS**: CyberPower CP1300EPFCLCD (1300 VA). Observed: ~37 min runtime at
  13 % load; `battery.charge.low 10`, `battery.runtime.low 300`,
  `ups.delay.shutdown 20`, `ups.delay.start 30`.
- **Nodes today**: no NUT client, no BIOS AC-restore confirmed, no WOL config.

## 2. Goals and non-goals

Goals:

1. cl02/cl03 shut down cleanly **immediately** when the UPS leaves mains.
2. cl01 rides the outage and shuts down cleanly when the Pi signals battery
   low (`FSD`).
3. Nodes power back on when mains returns, without manual steps, in both the
   "battery held" and "battery died" cases.
4. The Pi gains nothing except one new `upsd` user.
5. Ignition remains the source of truth for node config.

Non-goals: UPS handling for other devices (TrueNAS and desktops are out of
scope); Apprise/notification hooks (optional follow-up); per-node USB access;
running any of this as a swarm service; a Pi-side WOL sender.

## 3. Design decisions

| Decision | Choice | Rejected alternatives (why) |
|---|---|---|
| Where the client runs | `upsmon` (secondary) in a container on each node | Pi-orchestrated SSH shutdown (too much reliance on the Pi); pure-bash NUT poller (less battle-tested; user chose upsmon); NUT sysext (no bakery extension exists, would need a self-built artifact) |
| How the container is launched | host systemd unit `nut-upsmon.service` running plain `docker run` | swarm service (an agent that shuts down its own host gets rescheduled; depends on swarm control plane exactly when it is degraded) |
| Power-on, battery held | cl01 sends WOL on its own `ONLINE` event | Pi as WOL sender (adds Pi reliance) |
| Power-on, battery died | BIOS "restore after AC loss" on every node | nothing else can fire when everything is dark |
| Applying to live nodes | apply by hand now, commit identical files to Ignition, verify parity | immediate PXE reinstall of all three (heavier; chosen to avoid now) |

Why `--net=host` is required: the container sends a UDP broadcast to
`10.10.10.255` for WOL. Forwarded directed broadcasts from bridge/overlay
networks are dropped by the kernel, so only host networking makes WOL
reliable. `--pid=host --privileged` is required so `SHUTDOWNCMD` can
`nsenter` into PID 1 and shut the host down.

## 4. Architecture

### 4.1 Pi (only change)

Add one user to `/etc/nut/upsd.users` (secondary may read status and receive
`FSD`, nothing more):

```
[upsmon-slave]
    password = <NUT slave password>
    upsmon secondary
```

The Pi's existing primary `upsmon` config stays as is. At battery low it sets
`FSD` (received by all secondaries), shuts the Pi down, and its driver sends
killpower to the UPS after halt (`/etc/killpower`).

### 4.2 Node container and unit

`nut-upsmon.service`, a host systemd unit on all three nodes:

```
[Service]
Environment=UPS_MODE=survivor
ExecStart=/usr/bin/docker run --rm --name nut-upsmon \
  --net=host --pid=host --privileged \
  -v /etc/nut:/etc/nut:ro \
  <nut-client image, pinned by digest> \
  upsmon -D
Restart=always
[Install]
WantedBy=multi-user.target
```

Not a swarm service. It must never be rescheduled, and it must run when the
swarm control plane is degraded.

**Image gate (decided during planning):** the image must contain `upsmon`,
`nsenter` (util-linux) — `SHUTDOWNCMD` executes inside the container — and a
WOL sender (busybox `nc -u` or `wakeonlan`). Verify and pin a public image by
digest; if none qualifies, build a small one from Alpine
(`nut-client`, `util-linux`, `wakeonlan`) and push to the Gitea registry.

### 4.3 Node config files

All nodes, `/etc/nut/nut.conf`: `MODE=netclient`.

All nodes, `/etc/nut/upsmon.conf` (common core):

```
MONITOR ups@10.10.10.12 1 upsmon-slave <NUT slave password> secondary
SHUTDOWNCMD "/usr/bin/nsenter -t 1 -m -u -i -n -p -- /usr/sbin/shutdown -h now"
POLLFREQ 5
POLLFREQALERT 5
NOTIFYCMD /etc/nut/ups-actions
```

Per-node difference is only the notify flags and the script's mode:

- **cl02/cl03** (`upsmon.conf` evacuee variant): `NOTIFYFLAG ONBATT EXEC`.
  On `ONBATT` the actions script shuts the host down immediately. The default
  `FSD` behavior (shutdown) also applies for battery low.
- **cl01** (`upsmon.conf` survivor variant): `NOTIFYFLAG ONLINE EXEC`. On
  `ONLINE` the actions script sends WOL. No `ONBATT` action. Shutdown on
  `FSD` is the default secondary behavior.

### 4.4 Actions script (`/etc/nut/ups-actions`)

One script, mode from a unit env var (`UPS_MODE=survivor|evacuee`):

- `evacuee` (cl02/cl03) + `ONBATT`: run the shutdown once (guard flag under
  `/run` so a status flap cannot re-trigger a shutdown that is already in
  flight), log to the journal.
- `survivor` (cl01) + `ONLINE`: send the magic packet to each recorded MAC — 3
  packets, 1 s apart, UDP broadcast to `10.10.10.255:9`. A packet to an
  already-running node is ignored.
- All modes log every notify event. `UPS_DRY_RUN=1` turns every action into a
  journal log line only (used for the first live test).

### 4.5 cl01 boot oneshot

`nut-wol-boot.service` on cl01: `oneshot`, `After=network-online.target`,
runs the WOL burst once at boot. Covers two states: cl01 rebooting while
cl02/cl03 sit in S5 with UPS power present, and `upsmon` not raising
`ONLINE` on a fresh start when the UPS is already `OL`.

### 4.6 BIOS settings (manual, one-time per node)

- cl02/cl03: enable Wake-on-LAN from S5, enable "restore power after AC loss".
- cl01: enable "restore power after AC loss" (it never needs WOL itself).
- Record cl02/cl03 `enp1s0` MACs into cl01's `nodes/cl01.env` (Ignition).

### 4.7 Timing sequences

**Mains drops:** upsd reports `OB` within ~2 s of the USB poll; cl02/cl03
receive `ONBATT` within `POLLFREQ` (5 s) and begin shutdown within seconds;
UPS load drops, runtime left for cl01 + Pi extends. A mains blip longer than
~5 s costs cl02/cl03 a reboot cycle (`ONBATT` fired, then `ONLINE` + WOL wake
them back).

**Mains returns:** upsd reports `OL`; cl01's `upsmon` raises `ONLINE`; WOL
burst; cl02/cl03 boot, `swarm.service` rejoins them, services reschedule.

**Outage outlasts the battery:** Pi `upsmon` hits battery low → `FSD`
propagates to cl01 within 5 s → cl01 shuts down; the Pi shuts down; after the
Pi halts, its driver sends killpower and the UPS waits `ups.delay.shutdown`
(currently 20 s) before cutting output.

**Battery-death risk and mitigation:** cl01's graceful stop (docker stopping
containers) may exceed the 20 s output delay, turning the clean shutdown into
a dirty cut. Mitigation: raise `ups.delay.shutdown` to ~120 s on the UPS so
the whole shutdown fits inside it (verify the variable is writable via `upsc`
with the primary credentials during planning; if not, set it in the UPS LCD
menu). cl02/cl03 are already down by this point and are unaffected.

**After output death:** mains returns → the CP1300 re-applies output (its
`delay.start` window) → BIOS AC-restore boots the Pi and all nodes → cl01's
boot oneshot fires WOL for any partial state → cluster re-forms.

## 5. Ignition changes

- `ignition/files/ups/`: `nut.conf`, `upsmon-survivor.conf`,
  `upsmon-evacuee.conf`, `ups-actions` (executable script).
- `common.bu.tmpl`: file entries for the four files plus `nut-upsmon.service`
  (all nodes) and `nut-wol-boot.service` (cl01 only). Each node's
  `nodes/cl0X.env` gains `UPS_MODE` (`evacuee` for cl02/cl03, `survivor`
  for cl01) and `UPSMON_CONF` pointing at the right source file
  (`files/ups/upsmon-evacuee.conf` vs `files/ups/upsmon-survivor.conf`); the
  unit's `Environment=UPS_MODE=…` is filled from `UPS_MODE` at render time.
- `nodes/cl01.env`: `UPS_WOL_MACS="<cl02 mac>,<cl03 mac>"`; new render
  variable `NUT_SLAVE_PASSWORD` alongside `KEEPALIVED_PASSWORD` (render-time
  secret; `out/` stays git-ignored).
- The script's shutdown path calls `/usr/sbin/shutdown` **on the host** (via
  the `nsenter` above); it needs no sudo or extra keys anywhere.

## 6. Applying to the live nodes

1. BIOS settings per node; capture MACs (`ssh core@<node> ip -br link`).
2. Pi: add the `[upsmon-slave]` user; generate the password; keep it in the
   1Password entry used by `render.sh`.
3. Deploy in dry-run first: render, copy files to each node
   (`scp` as `core`), `systemctl daemon-reload && systemctl enable --now
   nut-upsmon.service` with `UPS_DRY_RUN=1`.
4. Live test (section 8), then flip dry-run off per node (cl03 first, then
   cl02, then cl01).
5. Parity check: render `out/cl0*.ign`, extract the embedded files, diff
   against the copies on the nodes. The next natural PXE reinstall proves the
   baked copy.

## 7. Failure modes (accepted)

- Pi dead or `upsd` unreachable → clients see stale data and do nothing;
  nodes ride the battery (today's behavior; no regression). `upsmon` never
  treats stale data as a shutdown trigger.
- cl01 dead during the outage → no WOL on `ONLINE`; nodes wait for a manual
  wake. Narrow compound failure; accepted (BIOS AC-restore still covers the
  battery-death path).
- cl02/cl03 booting during an outage (blip + wake while still on battery) →
  they come up and keep running until the next battery-low; acceptable.
- `ONBATT` flap re-fires the notify → guard flag prevents double-shutdown.

## 8. Testing

1. **Dry run**: `UPS_DRY_RUN=1` on all three; unplug the UPS for 10–30 s;
   expect `ONBATT` logged on cl02/cl03, `ONLINE` + WOL-burst logged on cl01,
   no real shutdowns; replug.
2. **Live shutdown/wake**: mains off → cl02/cl03 power off (~1 min);
   `docker node ls` shows them down; mains on → WOL → nodes boot, rejoin,
   services reschedule. Acceptance: no dirty shutdowns in either journal.
3. **Full loop, mains present**: `upsmon -c fsd` on the Pi → cl01, cl02/cl03
   and the Pi all shut down cleanly → UPS cuts output after the delay → UPS
   restores output → everything auto-boots → quorum restored. Proves the
   battery-death path without waiting hours.
4. **Long-outage** (optional): leave mains off until cl01 reaches `FSD`.

## 9. Security notes

- The NUT protocol is plaintext TCP; the slave password rides the LAN only
  (LAN/VPN-only network, switch on UPS). Acceptable.
- The privileged container's scope is minimal: it runs `upsmon` and the
  actions script only. Image pinned by digest.
- Slave password: `0640 root:nut`-style file on nodes, render-time env on the
  workstation. Never committed.
- The Pi's existing `[upsmon]` password is weak (`upsmon`). Optional
  hardening: strengthen it, and verify no other host consumes `upsd` before
  considering a `LISTEN` restriction (open item).

## 10. Open items (decided during planning/implementation)

- Pick and verify the container image against the section 4.2 gate; pin by
  digest (or write the fallback Dockerfile).
- Verify `ups.delay.shutdown` is settable on this UPS; set ~120 s.
- Exact BIOS menu names per mini-PC model; confirm WOL actually wakes from S5
  (one node, one packet).
- Confirm nothing else polls `upsd@10.10.10.12` (e.g. uptime-kuma) before any
  credential/LISTEN hardening.
- Confirm `upsmon` ONLINE-on-start semantics (covered either way by the cl01
  boot oneshot).
