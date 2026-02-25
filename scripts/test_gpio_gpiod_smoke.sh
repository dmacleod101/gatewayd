#!/usr/bin/env bash
set -euo pipefail

# Usage:
#   sudo ./scripts/test_gpio_gpiod_smoke.sh gpiochip0 17 true
#   sudo ./scripts/test_gpio_gpiod_smoke.sh /dev/gpiochip0 17 false

CHIP="${1:-gpiochip0}"
LINE_OFFSET="${2:-}"
ACTIVE_HIGH="${3:-true}"

if [[ -z "${LINE_OFFSET}" ]]; then
  echo "Usage: sudo $0 <chip> <line_offset> <active_high(true|false)>"
  exit 2
fi

cd "$(dirname "$0")/.."

echo "[*] Running gpio_gpiod smoketest..."
echo "    chip=${CHIP} line_offset=${LINE_OFFSET} active_high=${ACTIVE_HIGH}"

go run ./scripts/gpio_gpiod_smoketest.go \
  -id "gpio_gpiod_test" \
  -chip "${CHIP}" \
  -line_offset "${LINE_OFFSET}" \
  -active_high "${ACTIVE_HIGH}" \
  -hold_ms 250

echo "[*] Done."
