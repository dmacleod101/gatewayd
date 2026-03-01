# MCPTT ↔ Radio Gateway

Event-driven, deterministic bridge between **MCPTT (Agnet)** and analogue radio systems (e.g. Tait TM9000 series).

Designed for headless Raspberry Pi deployment with GPIO-based PTT/COR control, audio routing, and production-grade routing and arbitration logic.

---

## 🚀 Overview

This project provides a modular gateway that:

- Bridges MCPTT PTT events to analogue radio PTT via GPIO
- Detects radio COR and propagates floor events to MCPTT
- Provides deterministic routing modes
- Supports multi-endpoint configurations
- Runs headless under systemd
- Designed for vehicle-mounted or fixed-site deployment

The system is built around a Go core (`gatewayd`) with pluggable endpoint drivers.

---

## 🧠 Architecture

```
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
   GPIO Endpoint           Audio Endpoint
 (PTT / COR control)       (ALSA/PipeWire)
         ▼
   Analogue Radio
 (Tait TM9000 etc.)
```

---

## 🔧 Core Capabilities

### Deterministic Routing Modes
## 'simple_pair'

Deterministic pairing between one configured **source endpoint** and one or more **destination endpoints.**

Used for standard MCPTT ↔ Radio gateway deployments.

**Forward path (MCPTT → Radio)**
- `TXStart` or `RXStart` on the configured source → `PTTDown` on selected destination
- `TXStop` or `RXStop` on the configured source → `PTTUp` on selected destination

**Reverse path (Radio COR → MCPTT)**

- `RXStart` on a configured destination → `PTTDown` on configured source
- `RXStop` on a configured destination → `PTTUp` on configured source

This enables bidirectional MCPTT ↔ Radio operation in simple deployments.

#### `rule_based`

Rule-driven routing based on interface ID, subscriber ID, or talkgroup ID.

#### `multi_endpoint_bridge` (planned)

Planned symmetrical multi-endpoint bridging mode for complex deployments

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
- Lock and unlock transitions

---

## 📁 Production Directory Layout

```
/opt/gatewayd/              # Application code
    gatewayd                # Compiled binary
    core/
    routing/
    endpoints/
    scripts/

/etc/gateway/
    config.json             # Runtime configuration

/var/log/gateway/           # Optional log output

/etc/systemd/system/
    gatewayd.service
```

---

## ⚙️ Configuration

Primary runtime configuration:

```
/etc/gateway/config.json
```

Example:

```
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
```

---

## 🏗 Build Instructions

On Raspberry Pi (Debian 12/13 recommended):

```
sudo apt update
sudo apt install -y golang libgpiod-dev git

cd /opt
sudo git clone https://github.com/YOUR_ORG/gatewayd.git
cd gatewayd

go build -o gatewayd
```

---

## ▶️ Running Manually

```
sudo -u gateway \
  GATEWAYD_CONFIG=/etc/gateway/config.json \
  /opt/gatewayd/gatewayd
```

---

## 🔁 Running as a Service

Example systemd unit:

```
[Unit]
Description=Gatewayd (MCPTT ↔ Radio Bridge)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=gateway
Group=gateway
WorkingDirectory=/opt/gatewayd

Environment=GATEWAYD_CONFIG=/etc/gateway/config.json
ExecStart=/opt/gatewayd/gatewayd

Restart=on-failure
RestartSec=2
LimitNOFILE=65535

StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
```

Enable:

```
sudo systemctl daemon-reload
sudo systemctl enable gatewayd
sudo systemctl start gatewayd
```

View logs:

```
sudo journalctl -u gatewayd -f
```

---

## 🧪 Testing Strategy

Testing is performed at three levels:

### Core Regression Scripts
- Arbitration logic
- Locking behaviour
- Failover health flip

### GPIO Readback Validation
- Observed asserted vs expected state
- Hardware-level verification

### End-to-End PTT Tests
- MCPTT to Radio
- Radio to MCPTT
- Rapid key transitions

Stable checkpoints are tagged in Git:

```
v0.4-rulebased-lock-stable
v0.6-gpio-gpiod-readback-test-stable
```

---

## 🧩 Hardware Integration

Designed for:

- Raspberry Pi 4 / CM4
- Vehicle-mounted systems
- 12V (24V optional via regulator)
- Analogue audio in and out via rear accessory connector
- Tait TM9000 series compatibility

GPIO:

- PTT control output
- COR detect input
- Configurable active high or low

---

## 🛡 Design Principles

- Deterministic behaviour
- Explicit interface IDs
- No hidden transport IDs
- Event-driven state transitions
- Production-safe defaults
- Headless deployment
- Hardware readback validation

---

## 📌 Roadmap

- Multi-endpoint bridging
- Dynamic rule reload
- Web-based configuration UI
- Metrics export (Prometheus)
- Audio buffering and DSP enhancements
- Redundant MCPTT endpoint support

---

## 🔐 Security Considerations

- Runs as non-root `gateway` user
- GPIO access via group permissions
- No hard-coded credentials
- API tokens injected via config or secure store
- systemd sandboxing can be expanded

---

## 📄 License

Internal commercial project.  
Not for redistribution without permission.


---

## 🏷 Versioning & Release Policy

This project uses **semantic checkpoint tagging** rather than strict SemVer.

Tags are created at known-stable milestones where:

- Routing logic is validated
- Arbitration behaviour is confirmed
- GPIO readback is hardware-verified
- End-to-end PTT testing is successful
- No known regressions exist

### Tag Format

```
v<major>.<minor>-<feature>-stable
```

Examples:

```
v0.4-rulebased-lock-stable
v0.6-gpio-gpiod-readback-test-stable
v0.7-docs-baseline
```

### Tag Meaning

- **Major** – Architectural milestone or behavioural shift  
- **Minor** – Feature completion or capability extension  
- **Feature label** – Describes the stabilised capability  
- **stable** – Indicates hardware-tested, regression-tested state  

---

### When To Create a Tag

Create a tag only after:

1. Core regression scripts pass
2. GPIO readback behaviour is confirmed
3. MCPTT → Radio path verified
4. Radio → MCPTT path verified
5. Rapid keying tested
6. No log errors during sustained run

Tag example:

```
git tag -a v0.8-multi-endpoint-lock-stable -m "Multi-endpoint locking verified"
git push origin --tags
```

---

### Branching Model

- `main` → Production-stable code only  
- Feature work → Short-lived local branches  
- Only tagged commits represent validated production states  

---

### Deployment Policy

Production devices should:

- Run a tagged commit only  
- Never track moving `main`  
- Be updated via controlled pull + rebuild process  

Example upgrade procedure:

```
git fetch --tags
git checkout v0.8-multi-endpoint-lock-stable
go build -o gatewayd
sudo systemctl restart gatewayd
```

---

### Documentation Baselines

Documentation-only changes may be tagged separately:

```
v0.7-docs-baseline
```

This ensures firmware state and documentation state are traceable.

---

### Engineering Principle

If it is not tagged, it is not production-ready.
