# 🌀 Only Fan Controller

*The hottest fan controller for your server*

An intelligent fan controller for Dell PowerEdge servers with simple threshold-based control, hysteresis to prevent oscillation, and a workload hints API for proactive cooling.

## Features

- **Multi-source Monitoring** — CPU temps via IPMI, GPU temps via nvidia-smi
- **Simple Threshold Control** — Set CPU and GPU temperature thresholds, fans increase when exceeded
- **Hysteresis** — Configurable cooldown delay prevents fan oscillation at threshold boundaries
- **Workload Hints API** — External scripts can signal upcoming load for proactive cooling
- **Web Dashboard** — Real-time temps, fan speeds, and threshold visualization
- **Constant Idle Speed** — Quiet operation when temps are below thresholds

## Quick Start

### Docker (Recommended)

```bash
# Clone the repo
git clone https://github.com/sethpjohnson/only-fan-controller
cd only-fan-controller

# Copy and edit config
cp config.example.yaml config.yaml
nano config.yaml

# Run with docker-compose
docker-compose up -d
```

### Docker with NVIDIA GPU Support

```bash
docker run -d \
  --name only-fan-controller \
  --restart unless-stopped \
  --gpus all \
  -e NVIDIA_VISIBLE_DEVICES=all \
  -p 8086:8086 \
  -v ./config.yaml:/etc/only-fan-controller/config.yaml:ro \
  -v ./data:/var/lib/only-fan-controller \
  only-fan-controller:latest
```

## How It Works

1. **Idle State**: Fans run at `idle_speed` (default 20%) when both CPU and GPU are below their thresholds
2. **Threshold Exceeded**: When CPU > `cpu_threshold` OR GPU > `gpu_threshold`, fans increase by `step_size` per 5°C over
3. **Cooldown**: Fans only ramp down after staying below thresholds for `cooldown_delay` seconds (prevents oscillation)
4. **Workload Hints**: External scripts can set a minimum fan speed floor via the API

## Web Dashboard

Access the dashboard at `http://your-server:8086/dashboard/`

Shows:
- CPU temperatures (all cores)
- GPU temperatures (all GPUs)
- Current fan speed and mode
- Temperature history graph with threshold lines
- Active workload hints

## API Security

The API binds `0.0.0.0` by default (required for container/bridge networking), so
the dashboard and API are reachable from every host on your LAN. Fan control is
protected by a **bearer token**, not by the bind address.

- **Mutating endpoints** — `POST`/`DELETE /api/override` and `POST /api/hint`,
  `DELETE /api/hint/:source` — require the token.
- **Read-only endpoints** — `/api/status`, `/api/history`, `/api/config`, and the
  dashboard — stay open.

Set the token via `api.token` in the config (or the `API_TOKEN` env var), then
send it as an `Authorization: Bearer <token>` header:

```bash
curl -X POST http://localhost:8086/api/override \
  -H "Authorization: Bearer $API_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"speed": 50, "duration": 300, "reason": "testing"}'
```

`scripts/hint-client.sh` reads `API_TOKEN` from the environment and adds the
header automatically.

**If no token is configured**, mutating endpoints are accepted **only from the
local host (loopback)** and a warning is logged at startup. This keeps a
single-host setup convenient without silently exposing fan control to the LAN.
Loopback is determined from the real connection peer — a spoofed
`X-Forwarded-For` cannot bypass it.

There is no built-in TLS. To expose the controller beyond a trusted LAN, run it
behind a reverse proxy (nginx / Caddy / Traefik) that terminates TLS.

> **Upgrading from an earlier version?** No breaking changes: add `api.token` to
> your config (or set `API_TOKEN`) to enable off-host control. Existing
> docker-compose / Unraid deployments keep working unchanged — without a token
> they simply restrict control to loopback.

Other safety behavior worth knowing:

- Manual overrides are **clamped** to the configured `min_speed`/`max_speed`
  band and **capped at 24 hours** (an override with `duration: 0` is treated as
  24h, never truly indefinite). A critical temperature still ramps fans to max,
  overriding any manual override.
- Hint `source`/`type` are restricted to `[A-Za-z0-9_.-]` (max 64 chars),
  `intensity` to `low`/`medium`/`high`, and `action` to `start`/`stop`;
  everything else is rejected with `400`.

## API Endpoints

### GET /api/status

Returns current system state including thresholds:

```json
{
  "cpu": { "Temps": [33, 34], "Max": 34 },
  "gpu": { "Devices": [{"name": "Tesla P40", "temp": 42}], "Max": 42 },
  "current_speed": 20,
  "zone": "idle",
  "cpu_threshold": 65,
  "gpu_threshold": 60,
  "idle_speed": 20
}
```

### POST /api/hint

Register a workload hint for proactive cooling:

```bash
# Signal high GPU load starting (sets minimum fan speed to 45%)
curl -X POST http://localhost:8086/api/hint \
  -H "Content-Type: application/json" \
  -d '{
    "type": "gpu_load",
    "action": "start",
    "intensity": "high",
    "source": "whisper-transcription"
  }'

# Signal work complete (removes the floor)
curl -X POST http://localhost:8086/api/hint \
  -H "Content-Type: application/json" \
  -d '{
    "type": "gpu_load",
    "action": "stop",
    "source": "whisper-transcription"
  }'
```

Intensity levels:
- `low` — 25% minimum fan speed
- `medium` — 35% minimum fan speed  
- `high` — 45% minimum fan speed

### POST /api/override

Set a manual fan speed override:

```bash
curl -X POST http://localhost:8086/api/override \
  -H "Content-Type: application/json" \
  -d '{"speed": 50, "duration": 300, "reason": "testing"}'
```

### DELETE /api/override

Clear manual override and return to automatic control.

### GET /api/history?duration=3600

Get temperature/fan history for graphing.

## Configuration

See [config.example.yaml](config.example.yaml) for all options.

### Key Settings

```yaml
fan_control:
  idle_speed: 20             # Base fan speed when cool (%)
  cpu_threshold: 65          # Increase fans when CPU exceeds this (°C)
  gpu_threshold: 60          # Increase fans when GPU exceeds this (°C)
  step_size: 10              # Fan increase per 5°C over threshold (%)
  cooldown_delay: 60         # Seconds below threshold before ramping down
```

### Example: Quiet Home Server

```yaml
fan_control:
  idle_speed: 15             # Very quiet
  cpu_threshold: 70          # Higher threshold for quieter operation
  gpu_threshold: 65
  cooldown_delay: 120        # Wait 2 minutes before ramping down
```

### Example: Workstation with GPUs

```yaml
fan_control:
  idle_speed: 25             # Slightly higher baseline
  cpu_threshold: 60          # More aggressive cooling
  gpu_threshold: 55
  cooldown_delay: 30         # Faster response
```

## Integration Example

Wrap your GPU-intensive scripts to automatically manage cooling:

```bash
#!/bin/bash
# transcribe-with-cooling.sh

CONTROLLER_URL="http://localhost:8086"

# Signal GPU work starting
curl -s -X POST "$CONTROLLER_URL/api/hint" \
  -H "Content-Type: application/json" \
  -d '{"type":"gpu_load","action":"start","intensity":"high","source":"whisper"}'

# Wait for fans to ramp up
sleep 3

# Do the actual work
whisper --model large-v3 "$1"

# Signal complete
curl -s -X POST "$CONTROLLER_URL/api/hint" \
  -H "Content-Type: application/json" \
  -d '{"type":"gpu_load","action":"stop","source":"whisper"}'
```

## Environment Variables

All config options can be overridden via environment variables:

| Variable | Description | Default |
|----------|-------------|---------|
| `IDRAC_HOST` | iDRAC IP or "local" | from config |
| `IDRAC_USERNAME` | iDRAC username | root |
| `IDRAC_PASSWORD` | iDRAC password | - |
| `GPU_ENABLED` | Enable GPU monitoring | true |
| `FAN_IDLE_SPEED` | Base fan speed (%) | 20 |
| `FAN_CPU_THRESHOLD` | CPU temp threshold (°C) | 65 |
| `FAN_GPU_THRESHOLD` | GPU temp threshold (°C) | 60 |
| `CHECK_INTERVAL` | Seconds between checks | 10 |
| `API_PORT` | API/Dashboard port | 8086 |
| `API_TOKEN` | Bearer token for mutating endpoints | - (loopback-only) |

## Unraid Installation

1. Copy `unraid/only-fan-controller.xml` to `/boot/config/plugins/dockerMan/templates-user/`
2. Configure via the Unraid Docker UI
3. Or use Community Applications (search "Only Fan Controller")

## Requirements

- Dell PowerEdge server with iDRAC (tested on R730)
- iDRAC firmware with IPMI support
- For local mode: `/dev/ipmi0` device access
- For remote mode: IPMI over LAN enabled in iDRAC settings
- For GPU monitoring: NVIDIA drivers and `nvidia-smi`

## Building

```bash
# Build binary
go build -o only-fan-controller ./cmd/controller

# Build Docker image  
docker build -t only-fan-controller .
```

## License

MIT

## Credits

Inspired by [tigerblue77/Dell_iDRAC_fan_controller_Docker](https://github.com/tigerblue77/Dell_iDRAC_fan_controller_Docker)
