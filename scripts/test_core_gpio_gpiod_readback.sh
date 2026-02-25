#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/.."

LOG="$(mktemp /tmp/gatewayd_core_gpio_readback.XXXXXX.log)"
echo "[*] Log: ${LOG}"

echo "[*] Building gatewayd..."
go build -o ./gatewayd ./cmd/gatewayd

echo "[*] Running gatewayd for a short window (capturing stdout+stderr, line-buffered)..."
( stdbuf -oL -eL ./gatewayd >"${LOG}" 2>&1 ) &
PID=$!
sleep 6
kill "${PID}" >/dev/null 2>&1 || true
wait "${PID}" >/dev/null 2>&1 || true

echo "[*] Validating expected gpio events (radio_2)..."
python3 - "${LOG}" <<'PY'
import json, sys, pathlib

log_path = pathlib.Path(sys.argv[1])
lines = log_path.read_text(errors="ignore").splitlines()

def extract_json(s: str):
    i = s.find("{")
    if i < 0:
        return None
    candidate = s[i:].strip()
    if not candidate.endswith("}"):
        return None
    try:
        return json.loads(candidate)
    except Exception:
        return None

gpio_lines = []
for ln in lines:
    obj = extract_json(ln.strip())
    if not obj:
        continue
    if obj.get("interface_id") == "radio_2" and obj.get("command") in ("connect_ok","ptt_down_ok","ptt_up_ok"):
        gpio_lines.append(obj)

if not gpio_lines:
    print("FAIL: no gpio driver JSON lines found for interface_id=radio_2 in captured log.")
    print("---- LOG HEAD (first 80 lines) ----")
    for l in lines[:80]:
        print(l)
    sys.exit(1)

want_down = False
want_up = False
for obj in gpio_lines:
    cmd = obj.get("command")
    if cmd == "ptt_down_ok" and obj.get("active_high") is False and obj.get("observed_asserted") is True:
        want_down = True
    if cmd == "ptt_up_ok" and obj.get("active_high") is False and obj.get("observed_asserted") is False:
        want_up = True

if not want_down:
    print("FAIL: missing ptt_down_ok with observed_asserted=true and active_high=false")
    for o in gpio_lines:
        print(json.dumps(o, sort_keys=True))
    sys.exit(1)

if not want_up:
    print("FAIL: missing ptt_up_ok with observed_asserted=false and active_high=false")
    for o in gpio_lines:
        print(json.dumps(o, sort_keys=True))
    sys.exit(1)

print("OK: core-level gpio_gpiod readback assertions passed (radio_2)")
PY

echo "[*] OK"
