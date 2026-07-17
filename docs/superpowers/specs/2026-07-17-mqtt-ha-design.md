# MQTT / Home Assistant Support — Design

Date: 2026-07-17
Status: Approved by Seth (brainstorm session)
Base: stacked on the fail-safe hardening, API security, deps, and housekeeping branches (PRs #1–#5)

## Goal

Optional MQTT support so the controller's status appears in Home Assistant as a
self-registered device (MQTT Discovery), with full control parity: everything
the HTTP API can do (overrides, hints) is also possible over MQTT. Off by
default; zero behavior change when disabled.

## Decisions (from brainstorm Q&A)

| Question | Decision |
|---|---|
| Control scope | Full parity: sensors + override control + hints |
| HA registration | MQTT Discovery (retained config topics under `homeassistant/`) |
| Broker security | Username/password, no TLS (documented; consistent with project stance) |
| Publish cadence | Every control tick (`monitoring.interval`), retained state, LWT availability |
| Architecture | Decoupled bridge package — broker problems can never stall the control loop |

## Architecture

New package `internal/mqtt` using `github.com/eclipse/paho.mqtt.golang`
(standard Go client, built-in auto-reconnect).

- `mqtt.Bridge` consumes a small, bridge-defined consumer interface with
  exactly the controller methods it needs: `GetStatus()`, `SetOverride()`,
  `ClearOverride()`, `AddHint()`, `RemoveHint()` — the same methods the HTTP
  handlers call. All safety semantics (min/max clamping, 24h override cap,
  critical-temp ramp overriding everything, hint validation) live in the
  controller and therefore apply to MQTT commands automatically. The bridge
  re-implements none of them.
- Wired in `cmd/controller/main.go` `run()`, gated on `mqtt.enabled`
  (default `false`). The controller package remains MQTT-unaware.
- The bridge runs in its own goroutine(s). A hung, slow, or unreachable broker
  cannot affect fan control by construction: no shared locks with the control
  loop beyond the existing `GetStatus` read path, no unbounded queues, no
  blocking publishes from loop context.

## Configuration

New `mqtt:` section in the YAML config:

```yaml
mqtt:
  enabled: false                    # off by default
  broker: "tcp://192.168.1.x:1883"  # required when enabled
  username: ""                      # optional
  password: ""                      # optional; never exposed via /api/config
  client_id: "only-fan-controller"  # default
  base_topic: "only-fan-controller" # default; state/command topic root
  discovery_prefix: "homeassistant" # default; HA discovery root
```

- Env overrides: `MQTT_ENABLED`, `MQTT_BROKER`, `MQTT_USERNAME`,
  `MQTT_PASSWORD` (following the existing `applyEnvOverrides` pattern).
- `config.Validate()`: `broker` must be non-empty and parseable when
  `enabled`; invalid config keeps the existing refuse-to-start behavior.
- `Password` gets `json:"-"` (same treatment as the iDRAC password and API
  token). `/api/config` must not leak it (regression test).
- Publish cadence deliberately reuses `monitoring.interval` — no separate
  interval knob (YAGNI).

## Topics & entities

Availability: `<base_topic>/availability` — retained `online`, LWT `offline`.
HA marks the entire device unavailable the moment the process dies (free
external monitoring for the fail-safe scenarios).

State: `<base_topic>/state` — one retained JSON document per tick, derived
from `GetStatus()`: cpu max temp, gpu max temp, current fan %, target %, zone,
failsafe_active, failsafe_reason, restore_pending, last_write_failed, override
(speed/reason/expires), active hint count.

Commands (QoS 1 subscriptions):
- `<base_topic>/cmd/override` — JSON `{"speed": N, "duration_seconds": N, "reason": "..."}`
  → `SetOverride` (controller clamps speed, caps duration, validates reason
  using the same rules as HTTP).
- `<base_topic>/cmd/override/clear` — any payload → `ClearOverride`.
- `<base_topic>/cmd/hint` — JSON matching `POST /api/hint`'s schema, same
  validation (charset/length/closed sets), `action: start|stop`.

Discovery (retained, republished on every (re)connect, under
`<discovery_prefix>/…`), all entities grouped into one HA device
("Only Fan Controller", identifier from `client_id`):
- `sensor`: cpu_temp, gpu_temp, fan_speed, target_speed, zone, failsafe_reason
- `binary_sensor`: failsafe_active, restore_pending, last_write_failed
- `number`: override fan speed — min/max bound to the configured
  `min_speed`/`max_speed` so the HA UI cannot request an out-of-range value
  (the controller still clamps regardless); command topic maps to
  `cmd/override` with a default duration (1h, documented).
- `button`: clear override → `cmd/override/clear`.

Hints intentionally have no dedicated HA entity (a source/type/intensity tuple
doesn't map to an entity type); HA automations use the JSON command topic.
This is the "full parity" escape hatch.

## Error handling & lifecycle

- Auto-reconnect with paho's built-in backoff; discovery + availability
  republished on reconnect.
- Broker unreachable at startup: service starts normally; bridge connects in
  the background and logs (rate-limited) until it succeeds.
- Publish failure: log (rate-limited) and drop — state is republished next
  tick anyway. No unbounded buffering.
- Shutdown: mirrors the history-cleanup pattern — sequenced AFTER the BMC
  `restore()` choke point, publishes `offline` + disconnects with a short
  bounded timeout (abandon and log if exceeded). MQTT can never delay the
  safety-critical hand-back.

## Security

- MQTT command authorization = broker authentication. The HTTP bearer token is
  HTTP-only. README states plainly: anyone who can publish to the broker can
  command the fans — use broker credentials/ACLs. Blast radius remains bounded
  by controller-level clamps and the critical-temp override in all cases.
- No TLS support in v1 (documented limitation, consistent with project
  stance; revisit if a remote broker is ever needed).

## Testing

- Unit: fake client behind a bridge-defined publisher/subscriber interface —
  discovery payload correctness, state JSON shape, command validation and
  routing (including rejection paths), config validation.
- Integration: in-process broker (`mochi-mqtt/server`) — real
  connect → discovery → command → state round-trip, LWT verification on
  ungraceful disconnect. No Docker required.
- Demo mode works with MQTT enabled (bridge publishes mock data), so
  acceptance can drive the full flow live against a real or in-process broker.
- Regression: `/api/config` does not leak `mqtt.password`; `mqtt.enabled:
  false` produces zero MQTT activity; control-loop behavior identical with
  bridge enabled and broker unreachable.

## Documentation

- README: "Home Assistant (MQTT)" section — broker setup, discovery behavior,
  entity list, command topic examples, the security note above.
- `config.example.yaml`: full `mqtt:` block, commented (must still validate
  verbatim — extend the existing regression test).
- Unraid template: `MQTT_ENABLED`, `MQTT_BROKER`, `MQTT_USERNAME`,
  `MQTT_PASSWORD` (masked) fields.

## Out of scope (explicit)

- TLS/mqtts, HA entity for hints, separate publish interval, MQTT v5-specific
  features, bridging history data, HA config-flow/native integration.
