#!/usr/bin/env bash
set -euo pipefail

CFG="${1:-/tmp/gatewayd.rule_based.json}"

if [[ ! -f "$CFG" ]]; then
  echo "Config not found: $CFG"
  exit 1
fi

cd /opt/gatewayd

# Build binary
go build -o ./gatewayd ./cmd/gatewayd

python3 -m json.tool "$CFG" >/dev/null && echo "OK: $CFG" || (echo "BAD: $CFG"; exit 1)

TMPDIR="$(mktemp -d)"
LOGFILE="$TMPDIR/gatewayd.log.jsonl"
TMP_CFG="$TMPDIR/gatewayd.test.json"

trap 'echo; echo "Stopping..."; [[ -n "${GW_PID:-}" ]] && kill "${GW_PID}" >/dev/null 2>&1 || true; echo "Temp dir preserved at: $TMPDIR"' EXIT

readarray -t INFO < <(python3 - "$CFG" "$TMP_CFG" "$TMPDIR" <<'PY'
import json, sys, os

src, dst, tmpdir = sys.argv[1], sys.argv[2], sys.argv[3]
cfg = json.load(open(src, "r"))

routing = cfg.get("routing") or {}
routing["mode"] = "rule_based"

# Force arbitration to deterministic first_wins (prevents preempt during this test)
arb = routing.get("arbitration") or {}
arb["policy"] = "first_wins"
arb["priorities"] = {}
routing["arbitration"] = arb
cfg["routing"] = routing

eps = cfg.get("endpoints", [])
if not isinstance(eps, list) or not eps:
    print("ERROR: config has no endpoints[]", file=sys.stderr)
    raise SystemExit(2)

# Keep only the first enabled mcptt_stub; disable the rest (prevents competition)
mcptt = [e for e in eps if e.get("enabled", True) and e.get("driver") == "mcptt_stub"]
if not mcptt:
    print("ERROR: need at least one enabled mcptt_stub endpoint", file=sys.stderr)
    raise SystemExit(2)

keep_id = mcptt[0].get("id")
for e in mcptt[1:]:
    e["enabled"] = False

# Find enabled gpio_stub destinations
gpio = [e for e in eps if e.get("enabled", True) and e.get("driver") == "gpio_stub"]
if len(gpio) < 2:
    print("ERROR: need at least 2 enabled gpio_stub endpoints", file=sys.stderr)
    raise SystemExit(2)

# Attach demo_health_file to gpio_stub endpoints (runtime health flip)
for e in gpio:
    ecfg = e.get("config") or {}
    ecfg["demo_health_file"] = os.path.join(tmpdir, f"health_{e.get('id','unknown')}.txt")
    e["config"] = ecfg

json.dump(cfg, open(dst, "w"), indent=2, sort_keys=True)

print(keep_id)
print(gpio[0]["id"])
print(gpio[1]["id"])
PY
)

MCPTT_ID="${INFO[0]}"
DEST1_ID="${INFO[1]}"
DEST2_ID="${INFO[2]}"

H1="$TMPDIR/health_${DEST1_ID}.txt"
H2="$TMPDIR/health_${DEST2_ID}.txt"

echo "Using mcptt_stub source: $MCPTT_ID"
echo "Using gpio_stub destinations:"
echo "  DEST1=$DEST1_ID (health file $H1)"
echo "  DEST2=$DEST2_ID (health file $H2)"
echo
echo "Temp config: $TMP_CFG"
echo "Log file:    $LOGFILE"
echo

# Initial health: DEST1 healthy, DEST2 down => selection should pick DEST1
echo "healthy" > "$H1"
echo "down"    > "$H2"

echo "Starting gatewayd..."
(
  GATEWAYD_CONFIG="$TMP_CFG" ./gatewayd
) >"$LOGFILE" 2>&1 &
GW_PID=$!

echo "gatewayd pid=$GW_PID"
echo "Waiting for tx_start..."

python3 - "$LOGFILE" <<'PY'
import sys, time, json
logfile = sys.argv[1]
deadline = time.time() + 10.0

def seen(event_type):
    try:
        with open(logfile, "r") as f:
            for line in f:
                line=line.strip()
                if not line:
                    continue
                try:
                    o=json.loads(line)
                except Exception:
                    continue
                if o.get("msg") == "event_received" and o.get("event_type") == event_type:
                    return True
    except FileNotFoundError:
        return False
    return False

while time.time() < deadline:
    if seen("tx_start"):
        print("tx_start seen")
        sys.exit(0)
    time.sleep(0.1)

print("Timed out waiting for tx_start", file=sys.stderr)
sys.exit(1)
PY

echo
echo "Flipping health DURING TX:"
echo "  DEST1 -> down"
echo "  DEST2 -> healthy"
echo "down"    > "$H1"
echo "healthy" > "$H2"

echo "Waiting for tx_stop..."

python3 - "$LOGFILE" <<'PY'
import sys, time, json
logfile = sys.argv[1]
deadline = time.time() + 15.0

def seen(event_type):
    try:
        with open(logfile, "r") as f:
            for line in f:
                line=line.strip()
                if not line:
                    continue
                try:
                    o=json.loads(line)
                except Exception:
                    continue
                if o.get("msg") == "event_received" and o.get("event_type") == event_type:
                    return True
    except FileNotFoundError:
        return False
    return False

while time.time() < deadline:
    if seen("tx_stop"):
        print("tx_stop seen")
        sys.exit(0)
    time.sleep(0.1)

print("Timed out waiting for tx_stop", file=sys.stderr)
sys.exit(1)
PY

echo
echo "Stopping gatewayd..."
kill "$GW_PID" >/dev/null 2>&1 || true
sleep 0.2

echo
echo "Asserting: command_sent dest for ptt_down == ptt_up (destination locking)"

python3 - "$LOGFILE" <<'PY'
import sys, json
logfile = sys.argv[1]

ptt_down_dest = None
ptt_up_dest = None

with open(logfile, "r") as f:
    for line in f:
        line=line.strip()
        if not line:
            continue
        try:
            o=json.loads(line)
        except Exception:
            continue
        if o.get("msg") != "command_sent":
            continue
        cmd = o.get("command")
        dest = o.get("dest")
        if cmd == "ptt_down" and ptt_down_dest is None and isinstance(dest, str) and dest:
            ptt_down_dest = dest
        if cmd == "ptt_up" and ptt_up_dest is None and isinstance(dest, str) and dest:
            ptt_up_dest = dest

if ptt_down_dest is None or ptt_up_dest is None:
    print("FAILED: Could not find command_sent ptt_down/ptt_up with dest field.")
    print("Log:", logfile)
    sys.exit(2)

print("ptt_down dest:", ptt_down_dest)
print("ptt_up   dest:", ptt_up_dest)

if ptt_down_dest != ptt_up_dest:
    print("FAILED: destination changed mid-session (lock not working).")
    sys.exit(1)

print("OK: destination remained locked for the TX session.")
PY

echo
echo "SUCCESS: rule_based destination locking verified under mid-TX health flip."
echo "Log file saved at: $LOGFILE"
