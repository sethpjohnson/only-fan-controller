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

## Safety

While running, this controller puts the iDRAC into **manual fan mode** via raw
IPMI OEM commands and is the **sole thing standing between your hardware and
the BMC's fans idling**. It is designed so that as many failure modes as
possible fail toward "the BMC keeps cooling the box," not toward "the fans
stay wherever they were and the machine cooks":

- **Abnormal exit restores auto mode.** SIGINT/SIGTERM, a fatal API server
  error, and even a panic in the control loop all go through the same
  shutdown path, which calls `RestoreAutoMode` before the process exits. A
  config that exists but fails safety validation makes the process refuse to
  start at all (see [Upgrading](#upgrading)) rather than run with unsafe
  defaults — since manual mode is never enabled in that case, the BMC simply
  keeps its own automatic control.
- **Sensor loss fails up, not down.** A failed temperature read is never
  treated as 0°C (which would ramp fans down). The controller holds the last
  fan speed and, after `sensor_failure_limit` consecutive failures, hands
  cooling back to BMC auto mode. This is recoverable: once sensor reads
  succeed again, manual control is reclaimed automatically.
- **Fan-write failure is a sticky fail-safe.** After `write_failure_limit`
  consecutive failures to *write* a fan speed, control is handed back to BMC
  auto mode and stays there until the process restarts — the controller does
  not probe the write channel again mid-run, since that would flap the BMC
  between manual and auto.
- **Restore is retried until confirmed.** If the `RestoreAutoMode` call itself
  fails (e.g. the BMC is unreachable), the controller keeps retrying on every
  control-loop tick until it succeeds; `restore_pending` in `/api/status`
  reflects this. Nothing re-enables manual mode while a restore is
  unconfirmed.
- **Critical temperatures bypass everything.** If `critical_cpu_temp` or
  `critical_gpu_temp` is reached, fans jump straight to `max_speed`,
  bypassing step ramping and any active manual override.

**What this does *not* protect against:** a `SIGKILL` (`kill -9`) or a power
loss bypasses the shutdown path entirely and cannot trigger a restore — the
BMC's own thermal protections are the last line of defense in that case, same
as on any other server. This controller is a cooling *policy* on top of the
BMC, not a replacement for its built-in safety logic.

Also note that the raw IPMI commands used here (`ipmitool raw 0x30 0x30 ...`)
are **Dell iDRAC-specific OEM commands** — they are not portable to other
vendors' BMCs.

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

## Home Assistant (MQTT)

Optional. Off by default. When enabled, the controller connects to your MQTT
broker, self-registers in Home Assistant via **MQTT Discovery**, publishes its
state every control tick, and accepts fan overrides and workload hints over MQTT
— full parity with the HTTP API.

Enable it in `config.yaml`:

```yaml
mqtt:
  enabled: true
  broker: "tcp://192.168.1.10:1883"
  username: "homeassistant"
  password: "your-broker-password"
  client_id: "only-fan-controller"
  base_topic: "only-fan-controller"
  discovery_prefix: "homeassistant"
```

Or via environment variables: `MQTT_ENABLED=true`, `MQTT_BROKER=tcp://…`,
`MQTT_USERNAME=…`, `MQTT_PASSWORD=…`.

### What appears in Home Assistant

All entities are grouped under one device, **Only Fan Controller**:

| Entity | Type | Notes |
|--------|------|-------|
| CPU Temperature | sensor | °C |
| GPU Temperature (Max) | sensor | °C; hottest card across all GPUs (drives fan logic) |
| GPU _N_ (_model_) Temperature / Utilization / Power | sensor | one set per detected GPU (°C / % / W) |
| Fan Speed, Target Fan Speed | sensor | % |
| Thermal Zone, Failsafe Reason | sensor | text |
| Failsafe Active, Restore Pending, Last Fan Write Failed | binary_sensor | `problem` class |
| Override Fan Speed | number | slider bound to `min_speed`/`max_speed`; sends a 1-hour override |
| Clear Fan Override | button | clears any active override |

**Per-GPU sensors are dynamic in card count.** On a two-GPU box you get two sets
of Temperature/Utilization/Power sensors, on a three-GPU box three, and so on.
Each card is announced as soon as the controller's first GPU read completes (a
few seconds after start — MQTT connects before the sensors are read, so the
per-card entities appear on the first control tick, not instantly). A GPU added
later is picked up automatically on the next tick. If the card count *shrinks*,
the stale per-card entities linger in Home Assistant as *unavailable* until you
delete them (the controller does not remove discovery entries).

The device is marked **unavailable** the instant the process dies — the broker
publishes the Last Will (`offline`) on ungraceful disconnect, giving you free
external monitoring for the fail-safe scenarios.

`broker` is forgiving about format: a bare host, `host:port`, or even a browser
URL pasted from Home Assistant (`http://ha.local:8123`) is normalized to
`tcp://host:port` at startup (the interpretation is logged). If the broker turns
out to be unreachable — e.g. you pointed it at HA's web UI port `8123` instead of
the MQTT broker's `1883` — the log calls that out explicitly instead of silently
timing out.

### Topics

- `only-fan-controller/availability` — retained `online`/`offline`.
- `only-fan-controller/state` — retained JSON, one document per control tick.
- `only-fan-controller/cmd/override` — `{"speed": 60, "duration_seconds": 3600, "reason": "..."}`
- `only-fan-controller/cmd/override/clear` — any payload clears the override.
- `only-fan-controller/cmd/hint` — `{"type": "transcode", "action": "start|stop", "intensity": "high", "source": "plex", "duration_estimate": 120}`

Commands go through the exact same safety clamps and validation as the HTTP API:
speed is clamped to `min_speed`/`max_speed`, override duration is capped at 24h,
the critical-temperature ramp overrides everything, and hint fields are charset/
length checked. A malformed command is logged and dropped, never applied.

Command topics must be published **without** the retain flag — a retained
command would be replayed by the broker on every reconnect and restart. The
bridge deliberately drops retained messages on the command topics (logging the
drop once per topic) so a stray `mosquitto_pub -r` or a misconfigured automation
cannot silently re-fire the fans forever.

Hints have no dedicated HA entity (a source/type/intensity tuple doesn't map to
one) — drive `cmd/hint` from an HA automation for the full-parity escape hatch.

### Security

**MQTT command authorization is your broker's authentication.** The HTTP bearer
token is HTTP-only and does not apply here — anyone who can publish to the broker
can command the fans, so protect it with broker credentials/ACLs. The blast
radius is still bounded by the controller-level clamps and the critical-temp
override. **There is no TLS in v1**; keep the broker on a trusted LAN or in front
of a TLS-terminating proxy.

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
  "idle_speed": 20,
  "failsafe_active": false,
  "failsafe_reason": "none",
  "restore_pending": false,
  "last_write_failed": false
}
```

The fail-safe fields (see [Safety](#safety) above):

- `failsafe_active` — `true` once cooling has been handed back to BMC auto
  mode (sensor loss or repeated fan-write failures).
- `failsafe_reason` — `"none"`, `"sensor-loss"`, or `"write-failure"`.
- `restore_pending` — `true` if fail-safe is active but the hand-back to BMC
  auto mode has not yet been confirmed (the BMC may still be in manual mode);
  the controller keeps retrying until this clears.
- `last_write_failed` — `true` if the most recent fan-speed write failed.

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

**Logging**: the controller logs to stderr only — there is no `logging.level`
or `logging.file` config. Under Docker, use `docker logs` and your Docker log
driver's rotation settings (e.g. `json-file` with `max-size`/`max-file`) to
capture and rotate logs.

### Key Settings

```yaml
fan_control:
  idle_speed: 20             # Base fan speed when cool (%)
  cpu_threshold: 65          # Increase fans when CPU exceeds this (°C)
  gpu_threshold: 60          # Increase fans when GPU exceeds this (°C)
  step_size: 10              # Fan increase per 5°C over threshold (%)
  cooldown_delay: 60         # Seconds below threshold before ramping down
  # Emergency ramp trigger — required, must exceed the (effective) threshold
  # above it or the service refuses to start. See Safety and Upgrading above.
  critical_cpu_temp: 85
  critical_gpu_temp: 90
  # Fail-safe: consecutive failures before handing cooling back to BMC auto.
  sensor_failure_limit: 3
  write_failure_limit: 3

storage:
  path: "/var/lib/only-fan-controller/history.db"
  retention_days: 30         # History readings older than this are pruned daily
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
| `MQTT_ENABLED` | Enable Home Assistant MQTT bridge | false |
| `MQTT_BROKER` | MQTT broker URL (`tcp://host:port`) | - (required when enabled) |
| `MQTT_USERNAME` | MQTT broker username | - |
| `MQTT_PASSWORD` | MQTT broker password | - |

## Unraid Installation

1. Copy `unraid/only-fan-controller.xml` to `/boot/config/plugins/dockerMan/templates-user/`
2. Configure via the Unraid Docker UI
3. Or use Community Applications (search "Only Fan Controller")

## Upgrading

**smart-fan-controller → only-fan-controller rename.** The project (and the
Docker image) were renamed from `smart-fan-controller` to
`only-fan-controller`. If you have an existing deployment:

- The container name changed from `smart-fan-controller` to
  `only-fan-controller` — update any `docker stop`/`docker logs`/etc. scripts
  that reference the old name.
- The GHCR image path changed accordingly (`ghcr.io/<owner>/only-fan-controller`
  instead of `.../smart-fan-controller`); update your `docker-compose.yml` /
  `docker run` image reference.
- `scripts/hint-client.sh` now reads `FAN_URL` instead of `SMART_FAN_URL` for
  the controller base URL, but still honors `SMART_FAN_URL` as a fallback if
  set — no forced script changes.

**Config validation now refuses to start on unsafe configs.** If your config
sets `cpu_threshold` (or `gpu_threshold`) at or above the corresponding
critical temperature's default — most notably `cpu_threshold >= 85` without
also setting `critical_cpu_temp` explicitly above it — the service now exits
at startup with a clear error instead of silently falling back to default
thresholds. Set `critical_cpu_temp`/`critical_gpu_temp` explicitly, above your
configured thresholds, to fix this.

**API token auth is a config-only addition.** Existing deployments keep
working unchanged; see [API Security](#api-security) above.

**History retention now defaults to 30 days.** The `readings` table is
pruned automatically (see `storage.retention_days` in
[config.example.yaml](config.example.yaml)); previously it grew unbounded.

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
