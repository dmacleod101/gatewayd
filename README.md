# MCPTT ↔ Radio Gateway

Event-driven, deterministic bridge between **MCPTT (Agnet)** and analogue radio systems (e.g. Tait TM9000 series).

Designed for headless Raspberry Pi deployment with GPIO-based PTT/COR control, audio routing, and production-grade routing/arbitration logic.

---

## 🚀 Overview

This project provides a modular gateway that:

- Bridges MCPTT PTT events to analogue radio PTT via GPIO
- Detects radio COR and propagates floor events to MCPTT
- Provides deterministic routing modes
- Supports multi-endpoint configurations
- Runs headless under systemd
- Designed for vehicle-mounted or fixed-site deployment

The system is built around a **Go core (`gatewayd`)** with pluggable endpoint drivers.

---

## 🧠 Architecture

       ┌────────────────────┐
       │     MCPTT Side     │
       │   (Agnet / API)    │
       └─────────┬──────────┘
                 │ events
                 ▼
          ┌──────────────┐
          │   gatewayd   │
          │   (Go Core)  │
          │              │
          │ - Routing    │
          │ - Arbitration│
          │ - Failover   │
          │ - Locking    │
          └──────┬───────┘
                 │
     ┌───────────┴───────────┐
     ▼                       ▼

GPIO Endpoint Audio Endpoint
(PTT / COR control) (ALSA/PipeWire)
▼
Analogue Radio
(Tait TM9000 etc.)


---

## 🔧 Core Capabilities

### Deterministic Routing Modes

- `simple_pair`
- `rule_based`
- `multi_endpoint_bridge` (planned)

### Arbitration Modes

- `first_wins`
- `priority_preempt`
- Destination locking
- Health-aware failover

### Endpoint Types

- `gpio_gpiod` (libgpiod-based GPIO control)
- MCPTT endpoint (API-driven)
- Audio endpoint (ALSA / PipeWire integration)

### Event-Driven State Machine

- PTT assert / release
- COR detection
- Observed readback validation
- Health state tracking
- Lock/unlock transitions

---

## 📁 Production Directory Layout


/opt/gatewayd/ # Application code
gatewayd # Compiled binary
core/
routing/
endpoints/
scripts/

/etc/gateway/
config.json # Runtime configuration

/var/log/gateway/ # Optional log output

/etc/systemd/system/
gatewayd.service


---

## ⚙️ Configuration

Primary runtime configuration:


/etc/gateway/config.json


Example:

```json
{
  "routing": {
    "mode": "rule_based",
    "arbitration": "first_wins"
  },
  "endpoints": [
    {
      "interface_id": "radio_1",
      "driver": "gpio_gpiod",
      "chip": "gpiochip0",
      "ptt_line": 17,
      "cor_line": 27,
      "active_high": false
    },
    {
      "interface_id": "mcptt_a",
      "driver": "mcptt"
    }
  ]
}


🏗 Build Instructions

On Raspberry Pi (Debian 12/13 recommended):

