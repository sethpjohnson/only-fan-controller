# iDRAC Smart Fan Controller

## Overview

An intelligent fan controller for Dell PowerEdge servers that goes beyond simple threshold-based control. Monitors CPU and GPU temperatures, accepts hints from external applications about upcoming workloads, and makes predictive decisions about fan speeds to maintain optimal cooling while minimizing noise.

## Goals

1. **Multi-source temperature monitoring** - CPU (via IPMI) + GPUs (via nvidia-smi)
2. **Intelligent ramping** - Gradual fan speed changes based on temperature trends, not just thresholds
3. **Workload hints API** - External scripts can signal upcoming GPU/CPU load for proactive cooling
4. **Platform agnostic** - Works as standalone Docker, Unraid plugin, or bare metal
5. **Dashboard** - Real-time visualization of temps, fan speeds, and system state
6. **Quiet by default** - Optimize for silence during idle, aggressive cooling only when needed

## Architecture

```
┌─────────────────────────────────────────────────────────────┐
│                    Smart Fan Controller                      │
├─────────────────────────────────────────────────────────────┤
│                                                              │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐       │
│  │  CPU Monitor │  │  GPU Monitor │  │  Workload    │       │
│  │  (ipmitool)  │  │  (nvidia-smi)│  │  Hints API   │       │
│  └──────┬───────┘  └──────┬───────┘  └──────┬───────┘       │
│         │                 │                 │                │
│         └────────────┬────┴─────────────────┘                │
│                      ▼                                       │
│            ┌─────────────────┐                               │
│            │  Decision Engine │                              │
│            │  - Trend analysis│                              │
│            │  - Predictive    │                              │
│            │  - Zone mapping  │                              │
│            └────────┬────────┘                               │
│                     ▼                                        │
│            ┌─────────────────┐                               │
│            │  Fan Controller │                               │
│            │  (ipmitool)     │                               │
│            └─────────────────┘                               │
│                                                              │
│  ┌──────────────────────────────────────────────────────┐   │
│  │                    REST API                           │   │
│  │  GET  /status     - Current temps, fans, state        │   │
│  │  GET  /history    - Temperature/fan history           │   │
│  │  POST /hint       - Workload hint (pre-heat/cooldown) │   │
│  │  POST /override   - Manual fan speed override         │   │
│  │  GET  /config     - Current configuration             │   │
│  │  PUT  /config     - Update configuration              │   │
│  └──────────────────────────────────────────────────────┘   │
│                                                              │
│  ┌──────────────────────────────────────────────────────┐   │
│  │                   Web Dashboard                       │   │
│  │  - Real-time temp graphs (CPU + each GPU)            │   │
│  │  - Fan speed visualization                            │   │
│  │  - Active hints/overrides                             │   │
│  │  - Configuration UI                                   │   │
│  └──────────────────────────────────────────────────────┘   │
│                                                              │
└─────────────────────────────────────────────────────────────┘
```

## Temperature Zones & Fan Curves

Instead of a single threshold, use temperature zones with configurable fan curves:

```yaml
zones:
  idle:
    cpu_max: 45
    gpu_max: 40
    fan_speed: 10      # Near-silent
    
  normal:
    cpu_max: 60
    gpu_max: 70
    fan_speed: 25      # Audible but quiet
    
  warm:
    cpu_max: 70
    gpu_max: 80
    fan_speed: 45      # Moderate
    
  hot:
    cpu_max: 80
    gpu_max: 85
    fan_speed: 70      # Aggressive
    
  critical:
    cpu_max: 999       # Anything above hot
    gpu_max: 999
    fan_speed: 100     # Full blast + Dell auto mode
```

## Workload Hints API

External scripts can inform the controller about upcoming workloads:

```bash
# Signal that heavy GPU work is starting (pre-heat fans)
curl -X POST http://localhost:8086/hint \
  -H "Content-Type: application/json" \
  -d '{
    "type": "gpu_load",
    "action": "start",
    "intensity": "high",       # low, medium, high
    "duration_estimate": 300,  # seconds, optional
    "source": "whisper-transcription"
  }'

# Signal that work is complete (begin cooldown)
curl -X POST http://localhost:8086/hint \
  -H "Content-Type: application/json" \
  -d '{
    "type": "gpu_load",
    "action": "stop",
    "source": "whisper-transcription"
  }'
```

When a hint is received:
- **start + high**: Immediately ramp to at least "warm" zone fan speed
- **start + medium**: Ramp to "normal" zone minimum
- **stop**: Begin gradual cooldown (don't drop immediately, wait for temps to fall)

## Decision Engine Logic

```python
def calculate_target_fan_speed():
    # Get current readings
    cpu_temps = get_cpu_temps()        # List of CPU core temps
    gpu_temps = get_gpu_temps()        # List of GPU temps
    
    max_cpu = max(cpu_temps)
    max_gpu = max(gpu_temps) if gpu_temps else 0
    
    # Determine zone based on highest temp
    zone = determine_zone(max_cpu, max_gpu)
    base_speed = zone.fan_speed
    
    # Check for active workload hints
    if active_hints:
        hint_speed = max(h.minimum_fan_speed for h in active_hints)
        base_speed = max(base_speed, hint_speed)
    
    # Trend analysis - if temps rising fast, be proactive
    cpu_trend = calculate_trend(cpu_history, window=60)  # °C/min
    gpu_trend = calculate_trend(gpu_history, window=60)
    
    if cpu_trend > 2 or gpu_trend > 3:  # Rising fast
        base_speed = min(100, base_speed + 15)
    
    # Smooth transitions (don't jump more than 10% per cycle)
    current_speed = get_current_fan_speed()
    if abs(base_speed - current_speed) > 10:
        if base_speed > current_speed:
            target = current_speed + 10  # Ramp up faster
        else:
            target = current_speed - 5   # Ramp down slower
    else:
        target = base_speed
    
    return target
```

## Configuration

```yaml
# config.yaml
idrac:
  host: "local"              # "local" or IP address
  username: "root"           # Only for remote
  password: "calvin"         # Only for remote

monitoring:
  interval: 10               # Seconds between checks
  history_retention: 3600    # Seconds of history to keep

gpu:
  enabled: true
  nvidia_smi_path: "/usr/bin/nvidia-smi"

zones:
  # ... as above

fan_control:
  min_speed: 5               # Never go below this
  max_speed: 100             # Never exceed this
  ramp_up_step: 10           # Max increase per cycle
  ramp_down_step: 5          # Max decrease per cycle
  cooldown_delay: 30         # Seconds to wait before ramping down

api:
  port: 8086
  host: "0.0.0.0"

dashboard:
  enabled: true
  port: 8087                 # Or same as API with /dashboard route

logging:
  level: "info"
  file: "/var/log/only-fan-controller.log"
```

## API Endpoints

### GET /status
Returns current system state:
```json
{
  "timestamp": "2026-02-02T14:03:00Z",
  "cpu": {
    "temps": [45, 47, 44, 46],
    "max": 47,
    "trend": 0.5
  },
  "gpu": {
    "devices": [
      {"index": 0, "name": "Tesla P40", "temp": 42, "utilization": 0},
      {"index": 1, "name": "Tesla P40", "temp": 40, "utilization": 0}
    ],
    "max": 42,
    "trend": -0.2
  },
  "fans": {
    "current_speed": 15,
    "target_speed": 15,
    "mode": "manual"
  },
  "zone": "idle",
  "active_hints": [],
  "override": null
}
```

### GET /history?duration=3600
Returns temperature and fan history for graphing.

### POST /hint
Register a workload hint (see above).

### POST /override
Set manual fan speed override:
```json
{
  "speed": 50,
  "duration": 300,    // Optional, seconds
  "reason": "testing"
}
```

### DELETE /override
Clear manual override.

### GET /config
Returns current configuration.

### PUT /config
Update configuration (subset of fields).

## Tech Stack

- **Language**: Go (single binary, low resource usage, good for embedded)
- **Web Framework**: Gin or Echo
- **Frontend**: Embedded SPA (Vue.js or Svelte, compiled into binary)
- **Storage**: SQLite for history, YAML for config
- **Packaging**: Docker image + Unraid plugin template

## Unraid Integration

For Unraid, provide:
1. **Docker template** (XML) for Community Applications
2. **Dashboard widget** via Unraid's plugin system (or iframe embed)
3. **WebUI tile** that links to the dashboard

## File Structure

```
idrac-only-fan-controller/
├── SPEC.md
├── README.md
├── Dockerfile
├── docker-compose.yml
├── config.example.yaml
├── cmd/
│   └── controller/
│       └── main.go
├── internal/
│   ├── config/
│   │   └── config.go
│   ├── monitor/
│   │   ├── cpu.go
│   │   └── gpu.go
│   ├── controller/
│   │   ├── fan.go
│   │   └── decision.go
│   ├── api/
│   │   ├── server.go
│   │   ├── handlers.go
│   │   └── hints.go
│   └── storage/
│       └── history.go
├── web/
│   ├── src/
│   │   ├── App.vue
│   │   └── components/
│   ├── package.json
│   └── vite.config.js
├── unraid/
│   ├── only-fan-controller.xml    # Docker template
│   └── plugin/                      # Optional Unraid plugin
└── scripts/
    └── hint-client.sh              # Example hint script
```

## Development Phases

### Phase 1: Core Controller (MVP)
- [ ] CPU temperature monitoring via ipmitool
- [ ] GPU temperature monitoring via nvidia-smi
- [ ] Basic zone-based fan control
- [ ] Configuration file support
- [ ] Logging

### Phase 2: API & Hints
- [ ] REST API server
- [ ] /status endpoint
- [ ] /hint endpoint for workload signals
- [ ] /override endpoint
- [ ] History storage

### Phase 3: Dashboard
- [ ] Web UI with real-time graphs
- [ ] Configuration editor
- [ ] Mobile-responsive design

### Phase 4: Packaging
- [ ] Docker image (multi-arch)
- [ ] Unraid template
- [ ] Documentation

### Phase 5: Polish
- [ ] Unraid dashboard widget
- [ ] Alerting (optional)
- [ ] Home Assistant integration (optional)

## Example Usage

### Transcription Script Integration

```bash
#!/bin/bash
# whisper-transcribe.sh

CONTROLLER_URL="http://localhost:8086"

# Signal GPU work starting
curl -s -X POST "$CONTROLLER_URL/hint" \
  -H "Content-Type: application/json" \
  -d '{"type":"gpu_load","action":"start","intensity":"high","source":"whisper"}'

# Do the actual transcription
whisper --model large-v3 "$1"

# Signal GPU work complete
curl -s -X POST "$CONTROLLER_URL/hint" \
  -H "Content-Type: application/json" \
  -d '{"type":"gpu_load","action":"stop","source":"whisper"}'
```

## References

- [Dell iDRAC IPMI commands](https://www.dell.com/support/kbdoc/en-us/000177566/how-to-set-fan-speed-on-a-poweredge-server)
- [tigerblue77/Dell_iDRAC_fan_controller_Docker](https://github.com/tigerblue77/Dell_iDRAC_fan_controller_Docker)
- [nvidia-smi documentation](https://developer.nvidia.com/nvidia-system-management-interface)
