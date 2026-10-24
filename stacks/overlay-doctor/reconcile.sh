#!/bin/sh
# Repairs stale VXLAN FDB entries in the swarm overlay network.
#
# Each cycle it builds the truth table (endpoint MAC -> node that runs it) from
# every node's live docker endpoints, then compares each node's FDB against it
# and repairs rows that disagree. Engine bug: moby#52661, fixed in 29.9.0 --
# see README.md for why this exists and how to remove it.
#
# Safety: repairs are limited to MACs that appear in the truth table, so a
# partial view of the cluster can only cause a skipped cycle, never a wrong
# write. Entries for unknown MACs are counted and left alone.
set -eu

NETWORK=${DOCTOR_NETWORK:-main}
INTERVAL=${DOCTOR_INTERVAL:-120}
DRY_RUN=${DOCTOR_DRY_RUN:-0}
MIN_ENDPOINTS=${DOCTOR_MIN_ENDPOINTS:-10}
NODES=${DOCTOR_NODES:-"cl01,10.10.10.21 cl02,10.10.10.22 cl03,10.10.10.23"}
KEY=${DOCTOR_KEY:-/run/secrets/overlay_doctor_ssh_key}
KNOWN_HOSTS=${DOCTOR_KNOWN_HOSTS:-/doctor/known_hosts}
BEAT_FILE=${DOCTOR_BEAT_FILE:-/tmp/doctor-beat}

# Swarm secrets mount 0444; ssh ignores a key that open. Take a private copy.
if [ -r "$KEY" ]; then
    (umask 077 && cat "$KEY" > /tmp/doctor-key)
    KEY=/tmp/doctor-key
fi

SSH_OPTS="-i $KEY -o UserKnownHostsFile=$KNOWN_HOSTS -o StrictHostKeyChecking=yes -o ConnectTimeout=5 -o BatchMode=yes -o LogLevel=ERROR"

log() { printf '%s %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*"; }

ssh_node() {
    ip=$1
    shift
    ssh $SSH_OPTS "core@$ip" "$@"
}

# One ssh round trip: sandbox name, live endpoints, and the node's FDB.
gather_node() {
    ip=$1
    ssh_node "$ip" sh -s -- "$NETWORK" <<'REMOTE'
set -eu
net=$1
id=$(docker network inspect "$net" --format '{{.Id}}')
id10=$(printf %s "$id" | cut -c1-10)
sandbox=$(sudo ls /var/run/docker/netns | grep -E "^[0-9]+-${id10}$" || true)
found=$(printf '%s\n' "$sandbox" | grep -c . || true)
if [ "$found" -ne 1 ]; then
    echo "expected one sandbox for $id10, found: ${sandbox:-none}" >&2
    exit 3
fi
echo "SANDBOX $sandbox"
echo "ENDPOINTS"
docker network inspect "$net" --format '{{range .Containers}}{{.IPv4Address}} {{.MacAddress}}{{"\n"}}{{end}}'
echo "FDB"
sudo nsenter --net=/var/run/docker/netns/"$sandbox" bridge fdb show
REMOTE
}

sandbox_of() { awk '$1=="SANDBOX"{print $2; exit}' "$1"; }

endpoints_of() {
    awk '/^ENDPOINTS$/{f=1;next} /^FDB$/{f=0} f && $2 ~ /^02:42:/{sub("/.*","",$1); print $1, $2}' "$1"
}

fdb_section() { awk '/^FDB$/{f=1;next} f' "$1"; }

# Normalises `bridge fdb show` rows down to "mac dst" pairs (reads stdin).
fdb_pairs() { awk '$4=="dst"{print $1, $5}'; }

# Emits the repair plan for one node: "del|replace <mac> <dst>" lines.
plan_body() {
    awk -v me="$1" '
        NR==FNR { truth[$1]=$2; next }
        NF==2 { fdb[$1]=$2 }
        END {
            for (m in truth) {
                want=truth[m]; have=fdb[m]
                if (want==me) { if (have!="") print "del", m, have }
                else if (have!=want) print "replace", m, want
            }
        }' "$2" "$3" | LC_ALL=C sort
}

conflicts_of() { cut -d' ' -f1 "$1" | uniq -d | wc -l | tr -d ' '; }

unknown_of() {
    awk 'NR==FNR{truth[$1]=1;next} !($1 in truth){n++} END{print n+0}' "$1" "$2"
}

# --plan <node-ip> <truth-file> <fdb-file>: print a plan without touching anything.
if [ "${1:-}" = "--plan" ]; then
    [ $# -eq 4 ] || { echo "usage: $0 --plan <node-ip> <truth-file> <fdb-file>" >&2; exit 2; }
    fdb_pairs < "$4" > /tmp/doctor-fdb.$$
    plan_body "$2" "$3" /tmp/doctor-fdb.$$ > /tmp/doctor-plan.$$
    cat /tmp/doctor-plan.$$
    echo "plan endpoints=$(wc -l < "$3" | tr -d ' ') fixes=$(wc -l < /tmp/doctor-plan.$$ | tr -d ' ') conflicts=$(conflicts_of "$3") unknown=$(unknown_of "$3" /tmp/doctor-fdb.$$)"
    rm -f /tmp/doctor-plan.$$ /tmp/doctor-fdb.$$
    exit 0
fi

cycle() {
    rm -rf "$TMP"
    mkdir -p "$TMP"
    : > "$TMP/truth.all"
    ok=0
    seen=""

    for pair in $NODES; do
        name=${pair%%,*}
        ip=${pair#*,}
        if gather_node "$ip" > "$TMP/gather.$name" 2> "$TMP/err.$name"; then
            ok=$((ok + 1))
            seen="$seen $name:$ip"
            endpoints_of "$TMP/gather.$name" | while read -r addr mac; do
                echo "$mac $ip"
            done >> "$TMP/truth.all"
        else
            log "gather failed node=$name ip=$ip: $(tr '\n' ' ' < "$TMP/err.$name")"
        fi
    done

    if [ "$ok" -ne "$(echo $NODES | wc -w | tr -d ' ')" ]; then
        log "skip cycle: gathered $ok of $(echo $NODES | wc -w | tr -d ' ') nodes"
        return 1
    fi

    sort -u "$TMP/truth.all" > "$TMP/truth.u"
    conflicts=$(conflicts_of "$TMP/truth.u")
    if [ "$conflicts" -gt 0 ]; then
        cut -d' ' -f1 "$TMP/truth.u" | uniq -d > "$TMP/conflict.macs"
        grep -v -F -f "$TMP/conflict.macs" "$TMP/truth.u" > "$TMP/truth.final" || true
    else
        cp "$TMP/truth.u" "$TMP/truth.final"
    fi

    total=$(wc -l < "$TMP/truth.final" | tr -d ' ')
    if [ "$total" -lt "$MIN_ENDPOINTS" ]; then
        log "skip cycle: $total endpoints is below the $MIN_ENDPOINTS minimum"
        return 1
    fi

    fixes=0
    unknown=0
    for pair in $seen; do
        name=${pair%%:*}
        ip=${pair#*:}
        sandbox=$(sandbox_of "$TMP/gather.$name")
        fdb_section "$TMP/gather.$name" | fdb_pairs > "$TMP/fdb.$name"
        unknown=$((unknown + $(unknown_of "$TMP/truth.final" "$TMP/fdb.$name")))
        plan_body "$ip" "$TMP/truth.final" "$TMP/fdb.$name" > "$TMP/plan.$name"
        [ -s "$TMP/plan.$name" ] || continue
        fixes=$((fixes + $(wc -l < "$TMP/plan.$name")))
        while read -r action mac dst; do
            if [ "$DRY_RUN" = "1" ]; then
                log "dry-run node=$name would $action mac=$mac dst=$dst"
                continue
            fi
            if ssh_node "$ip" sudo nsenter --net=/var/run/docker/netns/"$sandbox" \
                    bridge fdb "$action" "$mac" dev vxlan0 dst "$dst" self permanent </dev/null; then
                log "fixed node=$name action=$action mac=$mac dst=$dst"
            else
                log "apply failed node=$name action=$action mac=$mac dst=$dst"
                return 1
            fi
        done < "$TMP/plan.$name"
    done

    log "cycle nodes=$ok endpoints=$total conflicts=$conflicts unknown=$unknown fixes=$fixes dry_run=$DRY_RUN"
    date +%s > "$BEAT_FILE"
}

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT TERM INT

log "started network=$NETWORK interval=${INTERVAL}s nodes=$NODES dry_run=$DRY_RUN"

if [ "${DOCTOR_ONCE:-0}" = "1" ]; then
    cycle
    exit 0
fi

while :; do
    cycle || log "cycle failed, retrying after ${INTERVAL}s"
    sleep "$INTERVAL"
done
