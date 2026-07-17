# MQTT / Home Assistant Support Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add optional, off-by-default MQTT support so the controller self-registers in Home Assistant via MQTT Discovery, publishing retained state every control tick and accepting override/hint commands with the same safety semantics as the HTTP API.

**Architecture:** A new decoupled `internal/mqtt` package exposes a `Bridge` that talks to the controller through a small `Consumer` interface (exactly `GetStatus`/`SetOverride`/`ClearOverride`/`AddHint`/`RemoveHint`) and to the broker through a `Client` interface (real implementation wraps `paho.mqtt.golang`; tests use a fake). The bridge runs in its own goroutines with bounded publishes and no shared locks beyond the existing `GetStatus` read path, so a hung or unreachable broker can never stall the control loop. It is wired in `cmd/controller/main.go` `run()`, gated on `mqtt.enabled`, and torn down after the BMC `restore()` choke point with a bounded timeout.

**Tech Stack:** Go 1.25, `github.com/eclipse/paho.mqtt.golang` (MQTT client, built-in auto-reconnect), `github.com/mochi-mqtt/server/v2` (in-process broker, integration tests only), `gopkg.in/yaml.v3` (existing config), Go stdlib `testing`.

## Global Constraints

Copied verbatim from the approved spec (`docs/superpowers/specs/2026-07-17-mqtt-ha-design.md`). Every task's requirements implicitly include this section.

- **Off by default:** `mqtt.enabled` defaults to `false`; when disabled there is zero MQTT activity and zero behavior change.
- **Broker required when enabled:** `config.Validate()` requires `mqtt.broker` to be non-empty and parseable when `mqtt.enabled` is true; an invalid config keeps the existing refuse-to-start behavior.
- **Password never leaked:** `MQTTConfig.Password` carries `json:"-"`; `/api/config` must not expose it (regression test required).
- **No TLS in v1:** documented limitation; broker auth is username/password only. MQTT command authorization = broker authentication.
- **Publish cadence = `monitoring.interval`:** deliberately reuse the existing control-tick interval; no separate MQTT interval knob (YAGNI).
- **Discovery:** retained config topics under `<discovery_prefix>/…` (default `homeassistant`), republished on every (re)connect; all entities grouped into one HA device named "Only Fan Controller", identifier from `client_id`.
- **Availability:** `<base_topic>/availability`, retained `online`, LWT `offline`.
- **State:** `<base_topic>/state`, one retained JSON document per tick, derived from `GetStatus()`.
- **Commands (QoS 1):** `<base_topic>/cmd/override`, `<base_topic>/cmd/override/clear`, `<base_topic>/cmd/hint` — all safety clamping/validation lives in the controller and the shared validator; the bridge re-implements none of it.
- **Shutdown:** MQTT teardown is sequenced AFTER the BMC `restore()` choke point, publishes `offline` + disconnects with a short bounded timeout (abandon and log if exceeded). MQTT can never delay the safety-critical hand-back.
- **Client library:** `paho.mqtt.golang` for the client; `mochi-mqtt/server` for in-process integration tests (no Docker).
- **Env overrides:** `MQTT_ENABLED`, `MQTT_BROKER`, `MQTT_USERNAME`, `MQTT_PASSWORD`, following the existing `applyEnvOverrides` pattern.

---

## File Structure

**New files**
- `internal/config/config.go` (modify) — `MQTTConfig` struct, `Config.MQTT` field, defaults, validation.
- `internal/mqtt/client.go` — `Client` interface, `MessageHandler`, `ClientOptions`, `ClientFactory`, `NewPahoClient` (paho wrapper).
- `internal/mqtt/bridge.go` — `Consumer` interface, `Bridge`, `New`, lifecycle (`Start`/`Stop`), topic helpers, availability, rate limiter.
- `internal/mqtt/state.go` — `statePayload`, `buildStatePayload`, `publishState`, `publishLoop`.
- `internal/mqtt/discovery.go` — discovery entity builders, `discoveryEntities`, `publishDiscovery`.
- `internal/mqtt/command.go` — command structs, handlers, `subscribeCommands`.
- `internal/validate/validate.go` — shared request validation (used by both `api` and `mqtt`).
- `internal/mqtt/bridge_test.go`, `state_test.go`, `discovery_test.go`, `command_test.go`, `integration_test.go` — tests (the fakes `fakeClient`/`fakeConsumer`/`clientHolder` live in `bridge_test.go`, package-visible to all `_test.go` in `internal/mqtt`).

**Modified files**
- `cmd/controller/main.go` — `applyEnvOverrides` (MQTT vars), bridge wiring + shutdown sequencing.
- `cmd/controller/main_test.go` — env-override test.
- `internal/config/config_test.go` — example-yaml regression + validation cases + password-marshal test.
- `internal/api/server.go` + `internal/api/server_test.go` — switch to `internal/validate`.
- `config.example.yaml` — `mqtt:` block.
- `unraid/only-fan-controller.xml` — MQTT template fields.
- `README.md` — "Home Assistant (MQTT)" section + env-var table rows.

---

## Task 1: MQTT config section, validation, env overrides, example YAML

**Files:**
- Modify: `internal/config/config.go`
- Modify: `cmd/controller/main.go:24-96` (`applyEnvOverrides`)
- Modify: `config.example.yaml`
- Test: `internal/config/config_test.go`
- Test: `cmd/controller/main_test.go`

**Interfaces:**
- Consumes: nothing (first task).
- Produces:
  - `type config.MQTTConfig struct { Enabled bool; Broker string; Username string; Password string; ClientID string; BaseTopic string; DiscoveryPrefix string }`
  - `config.Config` gains field `MQTT MQTTConfig` (yaml `mqtt`).
  - `config.Default()` populates `MQTT` with `Enabled:false, ClientID:"only-fan-controller", BaseTopic:"only-fan-controller", DiscoveryPrefix:"homeassistant"`.
  - `config.Config.Validate()` enforces the broker rule when enabled.
  - `applyEnvOverrides` handles `MQTT_ENABLED/BROKER/USERNAME/PASSWORD`.

- [x] **Step 1: Write the failing config tests**

Add to `internal/config/config_test.go`:

```go
func TestMQTTValidation(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(c *Config)
		wantErr bool
	}{
		{
			name:    "disabled needs no broker",
			mutate:  func(c *Config) { c.MQTT.Enabled = false; c.MQTT.Broker = "" },
			wantErr: false,
		},
		{
			name:    "enabled with valid tcp broker is ok",
			mutate:  func(c *Config) { c.MQTT.Enabled = true; c.MQTT.Broker = "tcp://192.168.1.5:1883" },
			wantErr: false,
		},
		{
			name:    "enabled with empty broker is rejected",
			mutate:  func(c *Config) { c.MQTT.Enabled = true; c.MQTT.Broker = "" },
			wantErr: true,
		},
		{
			name:    "enabled with unparseable broker is rejected",
			mutate:  func(c *Config) { c.MQTT.Enabled = true; c.MQTT.Broker = "://nope" },
			wantErr: true,
		},
		{
			name:    "enabled with schemeless broker is rejected",
			mutate:  func(c *Config) { c.MQTT.Enabled = true; c.MQTT.Broker = "192.168.1.5:1883" },
			wantErr: true,
		},
		{
			name:    "enabled with blank client_id is rejected",
			mutate:  func(c *Config) { c.MQTT.Enabled = true; c.MQTT.Broker = "tcp://h:1883"; c.MQTT.ClientID = "" },
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := Default()
			tt.mutate(c)
			err := c.Validate()
			if tt.wantErr && err == nil {
				t.Fatal("expected validation error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected validation error: %v", err)
			}
		})
	}
}

func TestMQTTPasswordNotMarshaled(t *testing.T) {
	c := Default()
	c.MQTT.Password = "s3cr3t-broker-pw"
	b, err := json.Marshal(c.MQTT)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	if strings.Contains(string(b), "s3cr3t-broker-pw") {
		t.Fatalf("mqtt password leaked into JSON: %s", b)
	}
}
```

Add imports `encoding/json` and `strings` to the test file's import block.

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/config/ -run 'TestMQTTValidation|TestMQTTPasswordNotMarshaled' -v`
Expected: FAIL — `c.MQTT undefined (type *Config has no field or method MQTT)` (compile error).

- [x] **Step 3: Add the MQTT config struct, field, defaults, and validation**

In `internal/config/config.go`, add `"net/url"` to the import block. Add the `MQTT` field to `Config`:

```go
type Config struct {
	IDRAC      IDRACConfig      `yaml:"idrac"`
	Monitoring MonitoringConfig `yaml:"monitoring"`
	GPU        GPUConfig        `yaml:"gpu"`
	Zones      []Zone           `yaml:"zones"`
	FanControl FanControlConfig `yaml:"fan_control"`
	API        APIConfig        `yaml:"api"`
	Dashboard  DashboardConfig  `yaml:"dashboard"`
	Storage    StorageConfig    `yaml:"storage"`
	MQTT       MQTTConfig       `yaml:"mqtt"`
}
```

Add the struct (near the other config structs):

```go
// MQTTConfig configures the optional Home Assistant MQTT bridge. It is off by
// default; when Enabled, Broker is required and validated. Password carries
// json:"-" so it is never exposed via /api/config (same treatment as the iDRAC
// password and API token).
type MQTTConfig struct {
	Enabled         bool   `yaml:"enabled" json:"enabled"`
	Broker          string `yaml:"broker" json:"broker"`
	Username        string `yaml:"username" json:"username"`
	Password        string `yaml:"password" json:"-"`
	ClientID        string `yaml:"client_id" json:"client_id"`
	BaseTopic       string `yaml:"base_topic" json:"base_topic"`
	DiscoveryPrefix string `yaml:"discovery_prefix" json:"discovery_prefix"`
}
```

In `Default()`, add the `MQTT` field to the returned `&Config{...}` (after `Storage`):

```go
		MQTT: MQTTConfig{
			Enabled:         false,
			ClientID:        "only-fan-controller",
			BaseTopic:       "only-fan-controller",
			DiscoveryPrefix: "homeassistant",
		},
```

In `Validate()`, add this block just before the final `return nil`:

```go
	// MQTT is optional. When enabled, the broker must be a parseable URL with a
	// scheme and host, and the identity/topic roots must be non-empty (they
	// default to non-empty values, so this only trips if an operator blanks
	// them). When disabled, none of this is checked.
	if c.MQTT.Enabled {
		if c.MQTT.Broker == "" {
			return fmt.Errorf("mqtt.enabled is true but mqtt.broker is empty")
		}
		u, err := url.Parse(c.MQTT.Broker)
		if err != nil {
			return fmt.Errorf("invalid mqtt.broker %q: %v", c.MQTT.Broker, err)
		}
		switch u.Scheme {
		case "tcp", "ssl", "ws", "wss", "mqtt", "mqtts":
		default:
			return fmt.Errorf("invalid mqtt.broker %q: scheme must be one of tcp/ssl/ws/wss/mqtt/mqtts", c.MQTT.Broker)
		}
		if u.Host == "" {
			return fmt.Errorf("invalid mqtt.broker %q: missing host", c.MQTT.Broker)
		}
		if c.MQTT.ClientID == "" || c.MQTT.BaseTopic == "" || c.MQTT.DiscoveryPrefix == "" {
			return fmt.Errorf("mqtt.client_id, mqtt.base_topic and mqtt.discovery_prefix must be non-empty when mqtt is enabled")
		}
	}
```

- [x] **Step 4: Run config tests to verify they pass**

Run: `go test ./internal/config/ -run 'TestMQTTValidation|TestMQTTPasswordNotMarshaled|TestExampleConfigIsValid' -v`
Expected: PASS (all three).

- [x] **Step 5: Write the failing env-override test**

Add to `cmd/controller/main_test.go`:

```go
func TestApplyEnvOverridesMQTT(t *testing.T) {
	t.Setenv("MQTT_ENABLED", "true")
	t.Setenv("MQTT_BROKER", "tcp://10.0.0.9:1883")
	t.Setenv("MQTT_USERNAME", "ha")
	t.Setenv("MQTT_PASSWORD", "brokerpw")

	cfg := config.Default()
	applyEnvOverrides(cfg)

	if !cfg.MQTT.Enabled {
		t.Fatal("MQTT_ENABLED=true should enable mqtt")
	}
	if cfg.MQTT.Broker != "tcp://10.0.0.9:1883" {
		t.Fatalf("broker override not applied: %q", cfg.MQTT.Broker)
	}
	if cfg.MQTT.Username != "ha" || cfg.MQTT.Password != "brokerpw" {
		t.Fatalf("credentials override not applied: %q / %q", cfg.MQTT.Username, cfg.MQTT.Password)
	}
}
```

Add `"github.com/sethpjohnson/only-fan-controller/internal/config"` to the test file's import block.

- [x] **Step 6: Run env-override test to verify it fails**

Run: `go test ./cmd/controller/ -run TestApplyEnvOverridesMQTT -v`
Expected: FAIL — env vars are ignored (`cfg.MQTT.Enabled` stays false).

- [x] **Step 7: Add MQTT env overrides**

In `cmd/controller/main.go`, inside `applyEnvOverrides`, after the `API_TOKEN` block (before the closing brace of the function):

```go
	// MQTT / Home Assistant bridge
	if v := os.Getenv("MQTT_ENABLED"); v != "" {
		cfg.MQTT.Enabled = strings.ToLower(v) == "true" || v == "1"
	}
	if v := os.Getenv("MQTT_BROKER"); v != "" {
		cfg.MQTT.Broker = v
	}
	if v := os.Getenv("MQTT_USERNAME"); v != "" {
		cfg.MQTT.Username = v
	}
	if v := os.Getenv("MQTT_PASSWORD"); v != "" {
		cfg.MQTT.Password = v
	}
```

(`os` and `strings` are already imported in `main.go`.)

- [x] **Step 8: Run env-override test to verify it passes**

Run: `go test ./cmd/controller/ -run TestApplyEnvOverridesMQTT -v`
Expected: PASS.

- [x] **Step 9: Add the `mqtt:` block to `config.example.yaml`**

Append to `config.example.yaml` (after the `storage:` block, before the trailing logging comment):

```yaml

# Optional Home Assistant integration over MQTT. Off by default: when disabled
# there is zero MQTT activity and no behavior change. When enabled, `broker` is
# REQUIRED (the service refuses to start otherwise). There is NO TLS support —
# anyone who can publish to the broker can command the fans, so protect it with
# broker credentials/ACLs. Publish cadence reuses monitoring.interval above.
mqtt:
  enabled: false                     # off by default
  broker: "tcp://192.168.1.10:1883"  # required when enabled (tcp://host:port)
  username: ""                       # optional broker username
  password: ""                       # optional broker password; never exposed via /api/config
  client_id: "only-fan-controller"   # MQTT client id and HA device identifier
  base_topic: "only-fan-controller"  # root for state/command/availability topics
  discovery_prefix: "homeassistant"  # HA MQTT Discovery root
```

- [x] **Step 10: Verify the example config still loads/validates verbatim**

Run: `go test ./internal/config/ -run TestExampleConfigIsValid -v`
Expected: PASS (the example has `enabled: false`, so the broker is not required).

- [x] **Step 11: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go cmd/controller/main.go cmd/controller/main_test.go config.example.yaml
git commit --no-gpg-sign -m "feat(config): add optional MQTT config section, validation, and env overrides"
```

---

## Task 2: Bridge skeleton — Consumer/Client interfaces, lifecycle, LWT, availability

**Files:**
- Create: `internal/mqtt/client.go`
- Create: `internal/mqtt/bridge.go`
- Test: `internal/mqtt/bridge_test.go`
- Modify: `go.mod`, `go.sum` (add `github.com/eclipse/paho.mqtt.golang`)

**Interfaces:**
- Consumes: `config.Config` / `config.MQTTConfig` (Task 1); `controller.Status`, `controller.WorkloadHint` (existing).
- Produces:
  - `type Consumer interface { GetStatus() *controller.Status; SetOverride(speed int, duration time.Duration, reason string); ClearOverride(); AddHint(hint *controller.WorkloadHint); RemoveHint(source string) }`
  - `type MessageHandler func(topic string, payload []byte)`
  - `type Client interface { Connect() error; Publish(topic string, qos byte, retained bool, payload []byte) error; Subscribe(topic string, qos byte, handler MessageHandler) error; Disconnect(quiesceMs uint) }`
  - `type ClientOptions struct { Broker, ClientID, Username, Password, AvailabilityTopic, OnlinePayload, OfflinePayload string; OnConnect func() }`
  - `type ClientFactory func(opts ClientOptions) Client`
  - `func NewPahoClient(opts ClientOptions) Client`
  - `type Bridge struct { ... }`, `func New(cfg *config.Config, consumer Consumer, factory ClientFactory) *Bridge`
  - Methods: `(*Bridge).Start()`, `(*Bridge).Stop()`, `(*Bridge).onConnect()`, `(*Bridge).publishAvailability(online bool)`, topic helpers `availabilityTopic/stateTopic/cmdOverrideTopic/cmdOverrideClearTopic/cmdHintTopic() string`.

- [x] **Step 1: Add the paho dependency**

Run:
```bash
go get github.com/eclipse/paho.mqtt.golang@v1.5.0
```
Expected: `go.mod`/`go.sum` updated with `github.com/eclipse/paho.mqtt.golang v1.5.0` (plus `golang.org/x/sync` indirect, already present or added).

- [x] **Step 2: Write the failing lifecycle test**

Create `internal/mqtt/bridge_test.go`:

```go
package mqtt

import (
	"sync"
	"testing"
	"time"

	"github.com/sethpjohnson/only-fan-controller/internal/config"
	"github.com/sethpjohnson/only-fan-controller/internal/controller"
)

// publishedMsg records a single Publish call for assertions.
type publishedMsg struct {
	topic    string
	qos      byte
	retained bool
	payload  []byte
}

// fakeClient is an in-memory Client used across the mqtt package tests. It
// records publishes/subscriptions and invokes OnConnect synchronously from
// Connect (mirroring paho's OnConnect firing on every successful connection).
type fakeClient struct {
	mu               sync.Mutex
	opts             ClientOptions
	published        []publishedMsg
	subs             map[string]MessageHandler
	disconnectCalled bool
}

func (f *fakeClient) Connect() error {
	if f.opts.OnConnect != nil {
		f.opts.OnConnect()
	}
	return nil
}

func (f *fakeClient) Publish(topic string, qos byte, retained bool, payload []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.published = append(f.published, publishedMsg{topic, qos, retained, append([]byte(nil), payload...)})
	return nil
}

func (f *fakeClient) Subscribe(topic string, qos byte, h MessageHandler) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.subs[topic] = h
	return nil
}

func (f *fakeClient) Disconnect(quiesceMs uint) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.disconnectCalled = true
}

// lastPublishOn returns the most recent publish to topic.
func (f *fakeClient) lastPublishOn(topic string) (publishedMsg, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.published) - 1; i >= 0; i-- {
		if f.published[i].topic == topic {
			return f.published[i], true
		}
	}
	return publishedMsg{}, false
}

// deliver invokes the handler registered for topic, simulating an inbound
// broker message.
func (f *fakeClient) deliver(topic string, payload []byte) {
	f.mu.Lock()
	h := f.subs[topic]
	f.mu.Unlock()
	if h != nil {
		h(topic, payload)
	}
}

// clientHolder captures the fakeClient the Bridge creates via the factory.
type clientHolder struct{ client *fakeClient }

func (h *clientHolder) factory(opts ClientOptions) Client {
	h.client = &fakeClient{opts: opts, subs: map[string]MessageHandler{}}
	return h.client
}

// fakeConsumer records controller calls and returns a canned status.
type fakeConsumer struct {
	mu             sync.Mutex
	status         *controller.Status
	overrideSpeed  int
	overrideDur    time.Duration
	overrideReason string
	overrideSet    bool
	cleared        bool
	addedHints     []*controller.WorkloadHint
	removedSources []string
}

func (c *fakeConsumer) GetStatus() *controller.Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.status != nil {
		return c.status
	}
	return &controller.Status{}
}

func (c *fakeConsumer) SetOverride(speed int, duration time.Duration, reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.overrideSpeed, c.overrideDur, c.overrideReason, c.overrideSet = speed, duration, reason, true
}

func (c *fakeConsumer) ClearOverride() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cleared = true
}

func (c *fakeConsumer) AddHint(hint *controller.WorkloadHint) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.addedHints = append(c.addedHints, hint)
}

func (c *fakeConsumer) RemoveHint(source string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.removedSources = append(c.removedSources, source)
}

// testConfig returns a valid enabled-MQTT config for tests.
func testConfig() *config.Config {
	cfg := config.Default()
	cfg.MQTT.Enabled = true
	cfg.MQTT.Broker = "tcp://127.0.0.1:1883"
	return cfg
}

func TestStartConfiguresLWTAndPublishesOnline(t *testing.T) {
	h := &clientHolder{}
	b := New(testConfig(), &fakeConsumer{}, h.factory)
	b.Start()

	if h.client.opts.AvailabilityTopic != "only-fan-controller/availability" {
		t.Fatalf("LWT topic = %q", h.client.opts.AvailabilityTopic)
	}
	if h.client.opts.OfflinePayload != "offline" {
		t.Fatalf("LWT payload = %q, want offline", h.client.opts.OfflinePayload)
	}
	msg, ok := h.client.lastPublishOn("only-fan-controller/availability")
	if !ok {
		t.Fatal("no availability publish on connect")
	}
	if string(msg.payload) != "online" || !msg.retained {
		t.Fatalf("availability publish = %q retained=%v, want online/true", msg.payload, msg.retained)
	}
}

func TestStopPublishesOfflineAndDisconnects(t *testing.T) {
	h := &clientHolder{}
	b := New(testConfig(), &fakeConsumer{}, h.factory)
	b.Start()
	b.Stop()

	msg, ok := h.client.lastPublishOn("only-fan-controller/availability")
	if !ok || string(msg.payload) != "offline" || !msg.retained {
		t.Fatalf("expected retained offline on stop, got %q retained=%v ok=%v", msg.payload, msg.retained, ok)
	}
	if !h.client.disconnectCalled {
		t.Fatal("Disconnect was not called on Stop")
	}
}
```

- [x] **Step 3: Run tests to verify they fail**

Run: `go test ./internal/mqtt/ -run 'TestStart|TestStop' -v`
Expected: FAIL — package does not compile (`New`, `Bridge`, `Client`, etc. undefined).

- [x] **Step 4: Create the Client interface and paho wrapper**

Create `internal/mqtt/client.go`:

```go
package mqtt

import (
	"fmt"
	"log"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"
)

// MessageHandler receives an inbound message's topic and raw payload.
type MessageHandler func(topic string, payload []byte)

// Client is the minimal broker surface the Bridge needs. The real
// implementation wraps paho; tests use an in-memory fake. All methods must be
// bounded: no call may block the control loop or shutdown indefinitely.
type Client interface {
	// Connect starts (or resumes) the connection. It must NOT block on an
	// unreachable broker — the real implementation is fire-and-forget and lets
	// paho's auto-reconnect handle background connection.
	Connect() error
	Publish(topic string, qos byte, retained bool, payload []byte) error
	Subscribe(topic string, qos byte, handler MessageHandler) error
	Disconnect(quiesceMs uint)
}

// ClientOptions carries everything NewPahoClient needs, including the LWT
// (AvailabilityTopic + OfflinePayload) and an OnConnect callback invoked on
// every successful (re)connection.
type ClientOptions struct {
	Broker            string
	ClientID          string
	Username          string
	Password          string
	AvailabilityTopic string
	OnlinePayload     string
	OfflinePayload    string
	OnConnect         func()
}

// ClientFactory builds a Client from options. Production passes NewPahoClient;
// tests pass a fake factory.
type ClientFactory func(opts ClientOptions) Client

// pahoClient adapts the paho client to the Client interface.
type pahoClient struct {
	client paho.Client
}

// NewPahoClient builds a paho-backed Client with auto-reconnect, connect-retry,
// and the LWT configured. OnConnect is wired so discovery/availability are
// republished on every (re)connection.
func NewPahoClient(opts ClientOptions) Client {
	o := paho.NewClientOptions()
	o.AddBroker(opts.Broker)
	o.SetClientID(opts.ClientID)
	if opts.Username != "" {
		o.SetUsername(opts.Username)
	}
	if opts.Password != "" {
		o.SetPassword(opts.Password)
	}
	o.SetWill(opts.AvailabilityTopic, opts.OfflinePayload, 1, true)
	o.SetAutoReconnect(true)
	o.SetConnectRetry(true)
	o.SetConnectRetryInterval(5 * time.Second)
	o.SetMaxReconnectInterval(30 * time.Second)
	o.SetCleanSession(true)
	o.SetOnConnectHandler(func(_ paho.Client) {
		log.Printf("MQTT: connected to %s", opts.Broker)
		if opts.OnConnect != nil {
			opts.OnConnect()
		}
	})
	o.SetConnectionLostHandler(func(_ paho.Client, err error) {
		log.Printf("MQTT: connection lost (auto-reconnecting): %v", err)
	})
	return &pahoClient{client: paho.NewClient(o)}
}

// Connect is fire-and-forget: it starts paho's connect loop and returns
// immediately so an unreachable broker never blocks startup. Success/failure is
// surfaced via the OnConnect / ConnectionLost handlers.
func (p *pahoClient) Connect() error {
	p.client.Connect()
	return nil
}

// Publish is bounded so a wedged broker can never stall the publish ticker.
func (p *pahoClient) Publish(topic string, qos byte, retained bool, payload []byte) error {
	token := p.client.Publish(topic, qos, retained, payload)
	if !token.WaitTimeout(2 * time.Second) {
		return fmt.Errorf("publish to %s timed out", topic)
	}
	return token.Error()
}

// Subscribe runs from the OnConnect handler (paho's goroutine), so waiting on
// the token here is safe.
func (p *pahoClient) Subscribe(topic string, qos byte, handler MessageHandler) error {
	token := p.client.Subscribe(topic, qos, func(_ paho.Client, m paho.Message) {
		handler(m.Topic(), m.Payload())
	})
	token.Wait()
	return token.Error()
}

func (p *pahoClient) Disconnect(quiesceMs uint) {
	p.client.Disconnect(quiesceMs)
}
```

- [x] **Step 5: Create the Bridge skeleton and lifecycle**

Create `internal/mqtt/bridge.go`:

```go
package mqtt

import (
	"log"
	"time"

	"github.com/sethpjohnson/only-fan-controller/internal/config"
	"github.com/sethpjohnson/only-fan-controller/internal/controller"
)

// Consumer is the exact controller surface the bridge needs. *controller.
// FanController satisfies it. All safety semantics (clamping, the 24h override
// cap, the critical-temp ramp, hint validation) live behind these methods, so
// MQTT commands inherit them for free — the bridge re-implements none of them.
type Consumer interface {
	GetStatus() *controller.Status
	SetOverride(speed int, duration time.Duration, reason string)
	ClearOverride()
	AddHint(hint *controller.WorkloadHint)
	RemoveHint(source string)
}

// disconnectQuiesceMs bounds the graceful disconnect wait so shutdown cannot be
// delayed by a slow broker.
const disconnectQuiesceMs = 250

// Bridge publishes controller state to MQTT and routes MQTT commands into the
// controller. It owns its own goroutines and never shares a lock with the
// control loop beyond the Consumer.GetStatus read path.
type Bridge struct {
	cfg       *config.Config
	consumer  Consumer
	newClient ClientFactory
	client    Client
}

// New builds a Bridge. factory is NewPahoClient in production and a fake in
// tests. Start actually connects.
func New(cfg *config.Config, consumer Consumer, factory ClientFactory) *Bridge {
	return &Bridge{
		cfg:       cfg,
		consumer:  consumer,
		newClient: factory,
	}
}

func (b *Bridge) availabilityTopic() string      { return b.cfg.MQTT.BaseTopic + "/availability" }
func (b *Bridge) stateTopic() string             { return b.cfg.MQTT.BaseTopic + "/state" }
func (b *Bridge) cmdOverrideTopic() string       { return b.cfg.MQTT.BaseTopic + "/cmd/override" }
func (b *Bridge) cmdOverrideClearTopic() string  { return b.cfg.MQTT.BaseTopic + "/cmd/override/clear" }
func (b *Bridge) cmdHintTopic() string           { return b.cfg.MQTT.BaseTopic + "/cmd/hint" }

// Start builds the client and initiates connection. Connect is non-blocking, so
// an unreachable broker does not delay startup. onConnect (fired on every
// (re)connection) republishes availability.
func (b *Bridge) Start() {
	opts := ClientOptions{
		Broker:            b.cfg.MQTT.Broker,
		ClientID:          b.cfg.MQTT.ClientID,
		Username:          b.cfg.MQTT.Username,
		Password:          b.cfg.MQTT.Password,
		AvailabilityTopic: b.availabilityTopic(),
		OnlinePayload:     "online",
		OfflinePayload:    "offline",
		OnConnect:         b.onConnect,
	}
	b.client = b.newClient(opts)
	log.Printf("MQTT: bridge starting (broker %s, base topic %s)", b.cfg.MQTT.Broker, b.cfg.MQTT.BaseTopic)
	if err := b.client.Connect(); err != nil {
		log.Printf("MQTT: initial connect error (will keep retrying in background): %v", err)
	}
}

// onConnect runs on every successful (re)connection. Task 4 adds discovery and
// Task 6 adds command subscriptions here.
func (b *Bridge) onConnect() {
	b.publishAvailability(true)
}

// publishAvailability publishes the retained availability state.
func (b *Bridge) publishAvailability(online bool) {
	payload := "offline"
	if online {
		payload = "online"
	}
	if err := b.client.Publish(b.availabilityTopic(), 1, true, []byte(payload)); err != nil {
		log.Printf("MQTT: failed to publish availability: %v", err)
	}
}

// Stop publishes the offline availability state and disconnects. Task 3 extends
// this to stop the publish loop first. It is bounded (disconnectQuiesceMs) so it
// cannot delay the safety-critical shutdown path.
func (b *Bridge) Stop() {
	if b.client == nil {
		return
	}
	b.publishAvailability(false)
	b.client.Disconnect(disconnectQuiesceMs)
	log.Printf("MQTT: bridge stopped")
}
```

- [x] **Step 6: Run tests to verify they pass**

Run: `go test ./internal/mqtt/ -run 'TestStart|TestStop' -v`
Expected: PASS.

- [x] **Step 7: Verify the whole module still builds and vets**

Run: `go build ./... && go vet ./internal/mqtt/`
Expected: no output (success).

- [x] **Step 8: Commit**

```bash
git add go.mod go.sum internal/mqtt/client.go internal/mqtt/bridge.go internal/mqtt/bridge_test.go
git commit --no-gpg-sign -m "feat(mqtt): add bridge skeleton, client interface, and connect/LWT lifecycle"
```

---

## Task 3: State publishing — retained JSON per tick

**Files:**
- Create: `internal/mqtt/state.go`
- Modify: `internal/mqtt/bridge.go` (add `stopCh`/`doneCh`/`pubLimiter` fields; start/stop the loop)
- Test: `internal/mqtt/state_test.go`

**Interfaces:**
- Consumes: `Bridge` + topic helpers (Task 2); `Consumer.GetStatus() *controller.Status`; `controller.Status`, `controller.CPUReading`/`GPUReading`, `controller.Override`.
- Produces:
  - `type statePayload struct { ... }` (JSON keys: `cpu_temp, gpu_temp, fan_speed, target_speed, zone, mode, failsafe_active, failsafe_reason, restore_pending, last_write_failed, override_speed, override_reason, override_expires, active_hint_count`).
  - `func buildStatePayload(status *controller.Status) statePayload`
  - `func (b *Bridge) publishState()`
  - `func (b *Bridge) publishLoop()`
  - `type rateLimiter struct{...}`, `func (r *rateLimiter) allow() bool`

- [x] **Step 1: Write the failing state tests**

Create `internal/mqtt/state_test.go`:

```go
package mqtt

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/sethpjohnson/only-fan-controller/internal/controller"
	"github.com/sethpjohnson/only-fan-controller/internal/monitor"
)

func TestBuildStatePayloadFull(t *testing.T) {
	exp := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	status := &controller.Status{
		CPU:             &monitor.CPUReading{Max: 71},
		GPU:             &monitor.GPUReading{Max: 63},
		CurrentSpeed:    40,
		TargetSpeed:     55,
		Zone:            "warm",
		Mode:            "override",
		FailsafeActive:  true,
		FailsafeReason:  "sensor-loss",
		RestorePending:  true,
		LastWriteFailed: false,
		Override:        &controller.Override{Speed: 55, Reason: "burn-in", ExpiresAt: exp},
		ActiveHints:     []*controller.WorkloadHint{{Source: "a"}, {Source: "b"}},
	}

	p := buildStatePayload(status)

	if p.CPUTemp == nil || *p.CPUTemp != 71 {
		t.Fatalf("cpu_temp = %v, want 71", p.CPUTemp)
	}
	if p.GPUTemp == nil || *p.GPUTemp != 63 {
		t.Fatalf("gpu_temp = %v, want 63", p.GPUTemp)
	}
	if p.FanSpeed != 40 || p.TargetSpeed != 55 {
		t.Fatalf("speeds = %d/%d, want 40/55", p.FanSpeed, p.TargetSpeed)
	}
	if p.OverrideSpeed == nil || *p.OverrideSpeed != 55 {
		t.Fatalf("override_speed = %v, want 55", p.OverrideSpeed)
	}
	if p.OverrideReason == nil || *p.OverrideReason != "burn-in" {
		t.Fatalf("override_reason = %v, want burn-in", p.OverrideReason)
	}
	if p.OverrideExpires == nil || *p.OverrideExpires != exp.Format(time.RFC3339) {
		t.Fatalf("override_expires = %v", p.OverrideExpires)
	}
	if p.ActiveHintCount != 2 {
		t.Fatalf("active_hint_count = %d, want 2", p.ActiveHintCount)
	}
	if !p.FailsafeActive || p.FailsafeReason != "sensor-loss" || !p.RestorePending {
		t.Fatalf("failsafe fields wrong: %+v", p)
	}
}

func TestBuildStatePayloadNilsBecomeNull(t *testing.T) {
	p := buildStatePayload(&controller.Status{})
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{"cpu_temp", "gpu_temp", "override_speed", "override_reason", "override_expires"} {
		if m[k] != nil {
			t.Fatalf("%s = %v, want null", k, m[k])
		}
	}
}

func TestPublishStatePublishesRetainedJSON(t *testing.T) {
	h := &clientHolder{}
	consumer := &fakeConsumer{status: &controller.Status{CurrentSpeed: 33, Zone: "idle"}}
	b := New(testConfig(), consumer, h.factory)
	b.Start()

	b.publishState()

	msg, ok := h.client.lastPublishOn("only-fan-controller/state")
	if !ok {
		t.Fatal("no publish on state topic")
	}
	if !msg.retained || msg.qos != 1 {
		t.Fatalf("state publish retained=%v qos=%d, want true/1", msg.retained, msg.qos)
	}
	var m map[string]any
	if err := json.Unmarshal(msg.payload, &m); err != nil {
		t.Fatalf("state payload not valid JSON: %v", err)
	}
	if m["fan_speed"].(float64) != 33 || m["zone"].(string) != "idle" {
		t.Fatalf("state payload wrong: %v", m)
	}
}
```

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/mqtt/ -run 'TestBuildStatePayload|TestPublishState' -v`
Expected: FAIL — `buildStatePayload` and `publishState` undefined.

- [x] **Step 3: Create the state payload and publisher**

Create `internal/mqtt/state.go`:

```go
package mqtt

import (
	"encoding/json"
	"log"
	"time"

	"github.com/sethpjohnson/only-fan-controller/internal/controller"
)

// statePayload is the retained JSON document published to <base_topic>/state on
// every tick. Pointer fields marshal to null when absent (nil sensor reading /
// no override) so HA templates can distinguish "no data" from zero.
type statePayload struct {
	CPUTemp         *int    `json:"cpu_temp"`
	GPUTemp         *int    `json:"gpu_temp"`
	FanSpeed        int     `json:"fan_speed"`
	TargetSpeed     int     `json:"target_speed"`
	Zone            string  `json:"zone"`
	Mode            string  `json:"mode"`
	FailsafeActive  bool    `json:"failsafe_active"`
	FailsafeReason  string  `json:"failsafe_reason"`
	RestorePending  bool    `json:"restore_pending"`
	LastWriteFailed bool    `json:"last_write_failed"`
	OverrideSpeed   *int    `json:"override_speed"`
	OverrideReason  *string `json:"override_reason"`
	OverrideExpires *string `json:"override_expires"`
	ActiveHintCount int     `json:"active_hint_count"`
}

// buildStatePayload derives the wire payload from a controller status snapshot.
func buildStatePayload(status *controller.Status) statePayload {
	p := statePayload{
		FanSpeed:        status.CurrentSpeed,
		TargetSpeed:     status.TargetSpeed,
		Zone:            status.Zone,
		Mode:            status.Mode,
		FailsafeActive:  status.FailsafeActive,
		FailsafeReason:  status.FailsafeReason,
		RestorePending:  status.RestorePending,
		LastWriteFailed: status.LastWriteFailed,
		ActiveHintCount: len(status.ActiveHints),
	}
	if status.CPU != nil {
		v := status.CPU.Max
		p.CPUTemp = &v
	}
	if status.GPU != nil {
		v := status.GPU.Max
		p.GPUTemp = &v
	}
	if status.Override != nil {
		s := status.Override.Speed
		r := status.Override.Reason
		e := status.Override.ExpiresAt.Format(time.RFC3339)
		p.OverrideSpeed = &s
		p.OverrideReason = &r
		p.OverrideExpires = &e
	}
	return p
}

// publishState marshals the current status and publishes it retained to the
// state topic. A marshal or publish error is rate-limited and dropped — the
// next tick republishes anyway, so no buffering is needed.
func (b *Bridge) publishState() {
	payload, err := json.Marshal(buildStatePayload(b.consumer.GetStatus()))
	if err != nil {
		if b.pubLimiter.allow() {
			log.Printf("MQTT: failed to marshal state: %v", err)
		}
		return
	}
	if err := b.client.Publish(b.stateTopic(), 1, true, payload); err != nil {
		if b.pubLimiter.allow() {
			log.Printf("MQTT: failed to publish state: %v", err)
		}
	}
}

// publishLoop publishes state on the monitoring interval until stopCh closes.
func (b *Bridge) publishLoop() {
	defer close(b.doneCh)
	interval := time.Duration(b.cfg.Monitoring.Interval) * time.Second
	if interval <= 0 {
		interval = 10 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			b.publishState()
		case <-b.stopCh:
			return
		}
	}
}
```

- [x] **Step 4: Add the loop plumbing and rate limiter to the Bridge**

In `internal/mqtt/bridge.go`, add `"sync"` to the import block, extend the `Bridge` struct, add the `rateLimiter` type, and wire the loop into `Start`/`Stop`.

Replace the `Bridge` struct with:

```go
type Bridge struct {
	cfg        *config.Config
	consumer   Consumer
	newClient  ClientFactory
	client     Client
	stopCh     chan struct{}
	doneCh     chan struct{}
	pubLimiter rateLimiter
}
```

Add near the top of the file (after the constants):

```go
// stopWaitTimeout bounds how long Stop waits for publishLoop to exit.
const stopWaitTimeout = 2 * time.Second

// rateLimiter permits an action at most once per min interval. Used to keep
// repeated broker failures from flooding the log.
type rateLimiter struct {
	mu   sync.Mutex
	last time.Time
	min  time.Duration
}

func (r *rateLimiter) allow() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	if r.min == 0 {
		r.min = 30 * time.Second
	}
	if !r.last.IsZero() && now.Sub(r.last) < r.min {
		return false
	}
	r.last = now
	return true
}
```

In `Start`, after the `Connect` block, start the loop:

```go
	b.stopCh = make(chan struct{})
	b.doneCh = make(chan struct{})
	go b.publishLoop()
```

Replace `Stop` with a version that stops the loop first (bounded), then publishes offline and disconnects:

```go
// Stop halts the publish loop (bounded by stopWaitTimeout), publishes the
// offline availability state, and disconnects. It is safe to call once. All
// waits are bounded so it can never delay the safety-critical shutdown path.
func (b *Bridge) Stop() {
	if b.client == nil {
		return
	}
	if b.stopCh != nil {
		close(b.stopCh)
		select {
		case <-b.doneCh:
		case <-time.After(stopWaitTimeout):
			log.Printf("MQTT: publish loop did not stop within %s; abandoning it", stopWaitTimeout)
		}
		b.stopCh = nil
	}
	b.publishAvailability(false)
	b.client.Disconnect(disconnectQuiesceMs)
	log.Printf("MQTT: bridge stopped")
}
```

- [x] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/mqtt/ -run 'TestBuildStatePayload|TestPublishState|TestStart|TestStop' -v`
Expected: PASS (state tests plus the Task 2 lifecycle tests still green).

- [x] **Step 6: Commit**

```bash
git add internal/mqtt/state.go internal/mqtt/bridge.go internal/mqtt/state_test.go
git commit --no-gpg-sign -m "feat(mqtt): publish retained state JSON on the monitoring interval"
```

---

## Task 4: Discovery payloads — sensors, binary_sensors, number, button

**Files:**
- Create: `internal/mqtt/discovery.go`
- Modify: `internal/mqtt/bridge.go` (`onConnect` publishes discovery)
- Test: `internal/mqtt/discovery_test.go`

**Interfaces:**
- Consumes: `Bridge` + topic helpers (Task 2); `cfg.FanControl.MinSpeed/MaxSpeed`, `cfg.MQTT.ClientID/DiscoveryPrefix/BaseTopic`.
- Produces:
  - `type discoveryEntity struct { topic string; payload []byte }`
  - `func (b *Bridge) discoveryEntities() []discoveryEntity`
  - `func (b *Bridge) publishDiscovery()`
  - helpers: `deviceBlock()`, `discoveryTopic(component, objectID string) string`, `sensorConfig(...)`, `binarySensorConfig(...)`, `numberConfig()`, `buttonConfig()`.

- [x] **Step 1: Write the failing discovery tests**

Create `internal/mqtt/discovery_test.go`:

```go
package mqtt

import (
	"encoding/json"
	"testing"
)

func TestDiscoveryEntitiesCoverAllEntities(t *testing.T) {
	h := &clientHolder{}
	b := New(testConfig(), &fakeConsumer{}, h.factory)

	entities := b.discoveryEntities()

	wantTopics := []string{
		"homeassistant/sensor/only-fan-controller/cpu_temp/config",
		"homeassistant/sensor/only-fan-controller/gpu_temp/config",
		"homeassistant/sensor/only-fan-controller/fan_speed/config",
		"homeassistant/sensor/only-fan-controller/target_speed/config",
		"homeassistant/sensor/only-fan-controller/zone/config",
		"homeassistant/sensor/only-fan-controller/failsafe_reason/config",
		"homeassistant/binary_sensor/only-fan-controller/failsafe_active/config",
		"homeassistant/binary_sensor/only-fan-controller/restore_pending/config",
		"homeassistant/binary_sensor/only-fan-controller/last_write_failed/config",
		"homeassistant/number/only-fan-controller/override_speed/config",
		"homeassistant/button/only-fan-controller/override_clear/config",
	}
	got := map[string]bool{}
	for _, e := range entities {
		got[e.topic] = true
	}
	if len(entities) != len(wantTopics) {
		t.Fatalf("got %d entities, want %d", len(entities), len(wantTopics))
	}
	for _, w := range wantTopics {
		if !got[w] {
			t.Fatalf("missing discovery topic %q", w)
		}
	}
}

func TestNumberEntityBoundsToConfiguredSpeeds(t *testing.T) {
	cfg := testConfig()
	cfg.FanControl.MinSpeed = 15
	cfg.FanControl.MaxSpeed = 90
	h := &clientHolder{}
	b := New(cfg, &fakeConsumer{}, h.factory)

	var payload map[string]any
	for _, e := range b.discoveryEntities() {
		if e.topic == "homeassistant/number/only-fan-controller/override_speed/config" {
			if err := json.Unmarshal(e.payload, &payload); err != nil {
				t.Fatalf("unmarshal number config: %v", err)
			}
		}
	}
	if payload == nil {
		t.Fatal("number entity not found")
	}
	if payload["min"].(float64) != 15 || payload["max"].(float64) != 90 {
		t.Fatalf("number min/max = %v/%v, want 15/90", payload["min"], payload["max"])
	}
	if payload["command_topic"].(string) != "only-fan-controller/cmd/override" {
		t.Fatalf("number command_topic = %v", payload["command_topic"])
	}
	dev := payload["device"].(map[string]any)
	ids := dev["identifiers"].([]any)
	if ids[0].(string) != "only-fan-controller" {
		t.Fatalf("device identifier = %v, want only-fan-controller", ids[0])
	}
}

func TestBinarySensorUsesProblemClassAndOnOff(t *testing.T) {
	h := &clientHolder{}
	b := New(testConfig(), &fakeConsumer{}, h.factory)
	var payload map[string]any
	for _, e := range b.discoveryEntities() {
		if e.topic == "homeassistant/binary_sensor/only-fan-controller/failsafe_active/config" {
			_ = json.Unmarshal(e.payload, &payload)
		}
	}
	if payload["device_class"].(string) != "problem" {
		t.Fatalf("device_class = %v, want problem", payload["device_class"])
	}
	if payload["payload_on"].(string) != "ON" || payload["payload_off"].(string) != "OFF" {
		t.Fatalf("payload_on/off = %v/%v", payload["payload_on"], payload["payload_off"])
	}
}

func TestPublishDiscoveryOnConnect(t *testing.T) {
	h := &clientHolder{}
	b := New(testConfig(), &fakeConsumer{}, h.factory)
	b.Start()

	msg, ok := h.client.lastPublishOn("homeassistant/sensor/only-fan-controller/cpu_temp/config")
	if !ok {
		t.Fatal("discovery not published on connect")
	}
	if !msg.retained || msg.qos != 1 {
		t.Fatalf("discovery publish retained=%v qos=%d, want true/1", msg.retained, msg.qos)
	}
}
```

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/mqtt/ -run 'TestDiscovery|TestNumberEntity|TestBinarySensor|TestPublishDiscovery' -v`
Expected: FAIL — `discoveryEntities` undefined.

- [x] **Step 3: Create the discovery builders**

Create `internal/mqtt/discovery.go`:

```go
package mqtt

import (
	"encoding/json"
	"fmt"
	"log"
)

// discoveryEntity is a single retained HA MQTT Discovery config message.
type discoveryEntity struct {
	topic   string
	payload []byte
}

// deviceBlock is the shared HA device all entities belong to. Its identifier is
// the MQTT client_id, so every entity is grouped under one "Only Fan Controller"
// device in Home Assistant.
func (b *Bridge) deviceBlock() map[string]any {
	return map[string]any{
		"identifiers":  []string{b.cfg.MQTT.ClientID},
		"name":         "Only Fan Controller",
		"manufacturer": "only-fan-controller",
		"model":        "IPMI Fan Controller",
	}
}

// discoveryTopic builds <prefix>/<component>/<client_id>/<object_id>/config.
func (b *Bridge) discoveryTopic(component, objectID string) string {
	return fmt.Sprintf("%s/%s/%s/%s/config", b.cfg.MQTT.DiscoveryPrefix, component, b.cfg.MQTT.ClientID, objectID)
}

// baseEntity returns the availability/device fields common to every entity.
func (b *Bridge) baseEntity(objectID, name string) map[string]any {
	return map[string]any{
		"name":                  name,
		"unique_id":             b.cfg.MQTT.ClientID + "_" + objectID,
		"object_id":             b.cfg.MQTT.ClientID + "_" + objectID,
		"availability_topic":    b.availabilityTopic(),
		"payload_available":     "online",
		"payload_not_available": "offline",
		"device":                b.deviceBlock(),
	}
}

// sensorConfig builds a read-only sensor bound to a value_json key.
func (b *Bridge) sensorConfig(objectID, name, valueKey, deviceClass, unit, icon string) map[string]any {
	e := b.baseEntity(objectID, name)
	e["state_topic"] = b.stateTopic()
	e["value_template"] = "{{ value_json." + valueKey + " }}"
	if deviceClass != "" {
		e["device_class"] = deviceClass
	}
	if unit != "" {
		e["unit_of_measurement"] = unit
	}
	if icon != "" {
		e["icon"] = icon
	}
	return e
}

// binarySensorConfig builds an ON/OFF binary sensor from a boolean value_json
// key, using device_class "problem" (HA shows red when true).
func (b *Bridge) binarySensorConfig(objectID, name, valueKey string) map[string]any {
	e := b.baseEntity(objectID, name)
	e["state_topic"] = b.stateTopic()
	e["value_template"] = "{{ 'ON' if value_json." + valueKey + " else 'OFF' }}"
	e["payload_on"] = "ON"
	e["payload_off"] = "OFF"
	e["device_class"] = "problem"
	return e
}

// numberConfig builds the override-fan-speed number. min/max are bound to the
// configured MinSpeed/MaxSpeed so the HA UI cannot request an out-of-range value
// (the controller still clamps regardless). The command wraps the raw value into
// the cmd/override JSON with a default 1h duration.
func (b *Bridge) numberConfig() map[string]any {
	e := b.baseEntity("override_speed", "Override Fan Speed")
	e["command_topic"] = b.cmdOverrideTopic()
	e["command_template"] = `{"speed": {{ value }}, "duration_seconds": 3600, "reason": "home assistant"}`
	e["state_topic"] = b.stateTopic()
	e["value_template"] = "{{ value_json.override_speed | default(0) }}"
	e["min"] = b.cfg.FanControl.MinSpeed
	e["max"] = b.cfg.FanControl.MaxSpeed
	e["step"] = 1
	e["unit_of_measurement"] = "%"
	e["mode"] = "slider"
	e["icon"] = "mdi:fan"
	return e
}

// buttonConfig builds the clear-override button.
func (b *Bridge) buttonConfig() map[string]any {
	e := b.baseEntity("override_clear", "Clear Fan Override")
	e["command_topic"] = b.cmdOverrideClearTopic()
	e["payload_press"] = "PRESS"
	e["icon"] = "mdi:fan-off"
	return e
}

// discoveryEntities returns every HA discovery config message. Marshal failures
// (not expected for these primitive maps) are logged and skipped.
func (b *Bridge) discoveryEntities() []discoveryEntity {
	type spec struct {
		component string
		objectID  string
		config    map[string]any
	}
	specs := []spec{
		{"sensor", "cpu_temp", b.sensorConfig("cpu_temp", "CPU Temperature", "cpu_temp", "temperature", "°C", "")},
		{"sensor", "gpu_temp", b.sensorConfig("gpu_temp", "GPU Temperature", "gpu_temp", "temperature", "°C", "")},
		{"sensor", "fan_speed", b.sensorConfig("fan_speed", "Fan Speed", "fan_speed", "", "%", "mdi:fan")},
		{"sensor", "target_speed", b.sensorConfig("target_speed", "Target Fan Speed", "target_speed", "", "%", "mdi:fan")},
		{"sensor", "zone", b.sensorConfig("zone", "Thermal Zone", "zone", "", "", "mdi:thermometer")},
		{"sensor", "failsafe_reason", b.sensorConfig("failsafe_reason", "Failsafe Reason", "failsafe_reason", "", "", "mdi:shield-alert")},
		{"binary_sensor", "failsafe_active", b.binarySensorConfig("failsafe_active", "Failsafe Active", "failsafe_active")},
		{"binary_sensor", "restore_pending", b.binarySensorConfig("restore_pending", "Restore Pending", "restore_pending")},
		{"binary_sensor", "last_write_failed", b.binarySensorConfig("last_write_failed", "Last Fan Write Failed", "last_write_failed")},
		{"number", "override_speed", b.numberConfig()},
		{"button", "override_clear", b.buttonConfig()},
	}
	entities := make([]discoveryEntity, 0, len(specs))
	for _, s := range specs {
		payload, err := json.Marshal(s.config)
		if err != nil {
			log.Printf("MQTT: failed to marshal discovery for %s/%s: %v", s.component, s.objectID, err)
			continue
		}
		entities = append(entities, discoveryEntity{
			topic:   b.discoveryTopic(s.component, s.objectID),
			payload: payload,
		})
	}
	return entities
}

// publishDiscovery publishes all discovery config messages, retained.
func (b *Bridge) publishDiscovery() {
	for _, e := range b.discoveryEntities() {
		if err := b.client.Publish(e.topic, 1, true, e.payload); err != nil {
			log.Printf("MQTT: failed to publish discovery %s: %v", e.topic, err)
		}
	}
}
```

- [x] **Step 4: Publish discovery on connect**

In `internal/mqtt/bridge.go`, update `onConnect` to publish discovery before availability (so HA has the entities registered before it sees them go online):

```go
func (b *Bridge) onConnect() {
	b.publishDiscovery()
	b.publishAvailability(true)
}
```

- [x] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/mqtt/ -run 'TestDiscovery|TestNumberEntity|TestBinarySensor|TestPublishDiscovery' -v`
Expected: PASS.

- [x] **Step 6: Commit**

```bash
git add internal/mqtt/discovery.go internal/mqtt/bridge.go internal/mqtt/discovery_test.go
git commit --no-gpg-sign -m "feat(mqtt): publish HA MQTT Discovery for all entities on connect"
```

---

## Task 5: Extract shared request validation into `internal/validate`

**Files:**
- Create: `internal/validate/validate.go`
- Modify: `internal/api/server.go`
- Test: `internal/validate/validate_test.go`
- Modify: `internal/api/server_test.go:210`

**Interfaces:**
- Consumes: nothing new.
- Produces:
  - `const validate.MaxHintFieldLen = 64`, `const validate.MaxOverrideReasonLen = 128`
  - `func validate.HintField(name, value string) error`
  - `func validate.HintAction(action string) error`
  - `func validate.Intensity(intensity string) error`
  - `func validate.OverrideSpeed(speed int) error`
  - `func validate.OverrideReason(reason string) error`

- [x] **Step 1: Write the failing validation tests**

Create `internal/validate/validate_test.go`:

```go
package validate

import "testing"

func TestHintField(t *testing.T) {
	if err := HintField("source", "plex.transcode-1"); err != nil {
		t.Fatalf("valid source rejected: %v", err)
	}
	if err := HintField("source", "bad source!"); err == nil {
		t.Fatal("source with space/bang should be rejected")
	}
	if err := HintField("type", string(make([]byte, MaxHintFieldLen+1))); err == nil {
		t.Fatal("overlong type should be rejected")
	}
}

func TestHintAction(t *testing.T) {
	if err := HintAction("start"); err != nil {
		t.Fatalf("start rejected: %v", err)
	}
	if err := HintAction("stop"); err != nil {
		t.Fatalf("stop rejected: %v", err)
	}
	if err := HintAction("pause"); err == nil {
		t.Fatal("pause should be rejected")
	}
}

func TestIntensity(t *testing.T) {
	for _, ok := range []string{"", "low", "medium", "high"} {
		if err := Intensity(ok); err != nil {
			t.Fatalf("intensity %q rejected: %v", ok, err)
		}
	}
	if err := Intensity("extreme"); err == nil {
		t.Fatal("extreme should be rejected")
	}
}

func TestOverrideSpeed(t *testing.T) {
	for _, ok := range []int{0, 50, 100} {
		if err := OverrideSpeed(ok); err != nil {
			t.Fatalf("speed %d rejected: %v", ok, err)
		}
	}
	if err := OverrideSpeed(-1); err == nil {
		t.Fatal("negative speed should be rejected")
	}
	if err := OverrideSpeed(101); err == nil {
		t.Fatal("speed > 100 should be rejected")
	}
}

func TestOverrideReason(t *testing.T) {
	if err := OverrideReason("manual burn-in test (2h)"); err != nil {
		t.Fatalf("normal prose rejected: %v", err)
	}
	if err := OverrideReason("bad\x00null"); err == nil {
		t.Fatal("control character should be rejected")
	}
	if err := OverrideReason(string(make([]byte, MaxOverrideReasonLen+1))); err == nil {
		t.Fatal("overlong reason should be rejected")
	}
}
```

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/validate/ -v`
Expected: FAIL — package `internal/validate` does not exist (build error).

- [x] **Step 3: Create the validate package**

Create `internal/validate/validate.go`:

```go
// Package validate holds request-field validation shared by the HTTP API and
// the MQTT bridge, so both control surfaces enforce identical rules (charset,
// length, closed sets, control-character rejection) on operator-supplied input.
package validate

import (
	"fmt"
	"regexp"
	"unicode"
)

const (
	// MaxHintFieldLen bounds free-form hint identifiers (source/type). Small on
	// purpose: these are process names, not prose.
	MaxHintFieldLen = 64
	// MaxOverrideReasonLen bounds the free-text override reason.
	MaxOverrideReasonLen = 128
)

// hintFieldPattern is the allowed charset for hint source/type. Restricting to
// this set means no stored hint string can carry HTML/script even if a dashboard
// interpolation is ever missed.
var hintFieldPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// allowedIntensities is the closed set of intensity values the controller
// understands ("" means "unspecified").
var allowedIntensities = map[string]bool{"": true, "low": true, "medium": true, "high": true}

// allowedHintActions is the closed set of hint actions the controller acts on.
var allowedHintActions = map[string]bool{"start": true, "stop": true}

// HintField enforces length and charset on a hint source/type. name is used only
// for the error message.
func HintField(name, value string) error {
	if len(value) > MaxHintFieldLen {
		return fmt.Errorf("%s exceeds %d characters", name, MaxHintFieldLen)
	}
	if !hintFieldPattern.MatchString(value) {
		return fmt.Errorf("%s must match [A-Za-z0-9_.-]", name)
	}
	return nil
}

// HintAction enforces the closed set of hint actions.
func HintAction(action string) error {
	if !allowedHintActions[action] {
		return fmt.Errorf("action must be one of start, stop")
	}
	return nil
}

// Intensity enforces the closed set of hint intensities.
func Intensity(intensity string) error {
	if !allowedIntensities[intensity] {
		return fmt.Errorf("intensity must be one of low, medium, high")
	}
	return nil
}

// OverrideSpeed enforces the 0..100 percentage range. The controller still
// clamps to the configured min/max band; this only rejects nonsensical input.
func OverrideSpeed(speed int) error {
	if speed < 0 || speed > 100 {
		return fmt.Errorf("speed must be 0-100")
	}
	return nil
}

// OverrideReason enforces a length cap and rejects control characters on the
// human-readable override reason. Normal punctuation, spaces, and quotes are
// valid free text.
func OverrideReason(reason string) error {
	if len(reason) > MaxOverrideReasonLen {
		return fmt.Errorf("reason exceeds %d characters", MaxOverrideReasonLen)
	}
	for _, r := range reason {
		if unicode.IsControl(r) {
			return fmt.Errorf("reason must not contain control characters")
		}
	}
	return nil
}
```

- [x] **Step 4: Run validate tests to verify they pass**

Run: `go test ./internal/validate/ -v`
Expected: PASS (all five tests).

- [x] **Step 5: Refactor the API to use `internal/validate`**

In `internal/api/server.go`:

1. Delete these now-relocated declarations: `maxHintFieldLen`, `hintFieldPattern`, `allowedIntensities`, `allowedHintActions`, `maxOverrideReasonLen`, `validateOverrideReason`, and `validateHintField` (lines 23-56 and 223-231 in the original file).
2. Remove `"regexp"` and `"unicode"` from the import block (no longer used) and add `"github.com/sethpjohnson/only-fan-controller/internal/validate"`.
3. Replace `validateHintRequest` with:

```go
// validateHintRequest enforces length, charset, and closed-set bounds on the
// client-controlled hint fields before they are stored or echoed back, using the
// shared validate package so the HTTP and MQTT surfaces agree.
func validateHintRequest(req *HintRequest) error {
	if err := validate.HintField("source", req.Source); err != nil {
		return err
	}
	if err := validate.HintField("type", req.Type); err != nil {
		return err
	}
	if err := validate.HintAction(req.Action); err != nil {
		return err
	}
	if err := validate.Intensity(req.Intensity); err != nil {
		return err
	}
	if req.DurationEstimate < 0 {
		return fmt.Errorf("duration_estimate must not be negative")
	}
	return nil
}
```

4. In `handleOverride`, replace the inline speed check and reason check:

```go
	if err := validate.OverrideSpeed(req.Speed); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if err := validate.OverrideReason(req.Reason); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
```

- [x] **Step 6: Update the one API test that referenced the moved constant**

In `internal/api/server_test.go`, line 210 references `maxOverrideReasonLen`. Change it to `validate.MaxOverrideReasonLen` and add `"github.com/sethpjohnson/only-fan-controller/internal/validate"` to that test file's import block:

```go
		{"overlong reason rejected", strings.Repeat("a", validate.MaxOverrideReasonLen+1), http.StatusBadRequest},
```

- [x] **Step 7: Run the API and validate suites to verify nothing regressed**

Run: `go test ./internal/api/ ./internal/validate/ -v`
Expected: PASS (API behavior unchanged; validation centralized).

- [x] **Step 8: Commit**

```bash
git add internal/validate/validate.go internal/validate/validate_test.go internal/api/server.go internal/api/server_test.go
git commit --no-gpg-sign -m "refactor: extract shared request validation into internal/validate"
```

---

## Task 6: Command handling — override / clear / hint over MQTT

**Files:**
- Create: `internal/mqtt/command.go`
- Modify: `internal/mqtt/bridge.go` (`onConnect` subscribes to commands)
- Test: `internal/mqtt/command_test.go`

**Interfaces:**
- Consumes: `Bridge`, topic helpers, `Consumer` (Task 2); `fakeClient.deliver` (Task 2 test helper); `validate.*` (Task 5); `controller.WorkloadHint`.
- Produces:
  - `type overrideCommand struct { Speed int; DurationSeconds int; Reason string }`
  - `type hintCommand struct { Type, Action, Intensity, Source string; DurationEstimate int }`
  - `func (b *Bridge) subscribeCommands()`
  - `func (b *Bridge) handleOverrideCommand(payload []byte)`
  - `func (b *Bridge) handleClearCommand(payload []byte)`
  - `func (b *Bridge) handleHintCommand(payload []byte)`

- [x] **Step 1: Write the failing command tests**

Create `internal/mqtt/command_test.go`:

```go
package mqtt

import (
	"testing"
	"time"
)

func startBridge(t *testing.T, consumer *fakeConsumer) *fakeClient {
	t.Helper()
	h := &clientHolder{}
	b := New(testConfig(), consumer, h.factory)
	b.Start()
	return h.client
}

func TestOverrideCommandRoutesToConsumer(t *testing.T) {
	consumer := &fakeConsumer{}
	client := startBridge(t, consumer)

	client.deliver("only-fan-controller/cmd/override",
		[]byte(`{"speed": 60, "duration_seconds": 1800, "reason": "burn-in"}`))

	consumer.mu.Lock()
	defer consumer.mu.Unlock()
	if !consumer.overrideSet {
		t.Fatal("SetOverride was not called")
	}
	if consumer.overrideSpeed != 60 {
		t.Fatalf("speed = %d, want 60", consumer.overrideSpeed)
	}
	if consumer.overrideDur != 1800*time.Second {
		t.Fatalf("duration = %v, want 30m", consumer.overrideDur)
	}
	if consumer.overrideReason != "burn-in" {
		t.Fatalf("reason = %q, want burn-in", consumer.overrideReason)
	}
}

func TestOverrideCommandRejectsOutOfRangeSpeed(t *testing.T) {
	consumer := &fakeConsumer{}
	client := startBridge(t, consumer)

	client.deliver("only-fan-controller/cmd/override", []byte(`{"speed": 150}`))

	consumer.mu.Lock()
	defer consumer.mu.Unlock()
	if consumer.overrideSet {
		t.Fatal("SetOverride should not be called for out-of-range speed")
	}
}

func TestOverrideCommandRejectsBadJSON(t *testing.T) {
	consumer := &fakeConsumer{}
	client := startBridge(t, consumer)

	client.deliver("only-fan-controller/cmd/override", []byte(`not json`))

	consumer.mu.Lock()
	defer consumer.mu.Unlock()
	if consumer.overrideSet {
		t.Fatal("SetOverride should not be called for invalid JSON")
	}
}

func TestClearCommandRoutesToConsumer(t *testing.T) {
	consumer := &fakeConsumer{}
	client := startBridge(t, consumer)

	client.deliver("only-fan-controller/cmd/override/clear", []byte("PRESS"))

	consumer.mu.Lock()
	defer consumer.mu.Unlock()
	if !consumer.cleared {
		t.Fatal("ClearOverride was not called")
	}
}

func TestHintCommandStartAddsHint(t *testing.T) {
	consumer := &fakeConsumer{}
	client := startBridge(t, consumer)

	client.deliver("only-fan-controller/cmd/hint",
		[]byte(`{"type": "transcode", "action": "start", "intensity": "high", "source": "plex", "duration_estimate": 120}`))

	consumer.mu.Lock()
	defer consumer.mu.Unlock()
	if len(consumer.addedHints) != 1 {
		t.Fatalf("added hints = %d, want 1", len(consumer.addedHints))
	}
	if consumer.addedHints[0].Source != "plex" || consumer.addedHints[0].Intensity != "high" {
		t.Fatalf("hint wrong: %+v", consumer.addedHints[0])
	}
	if consumer.addedHints[0].ExpiresAt.IsZero() {
		t.Fatal("expected an expiry from duration_estimate")
	}
}

func TestHintCommandStopRemovesHint(t *testing.T) {
	consumer := &fakeConsumer{}
	client := startBridge(t, consumer)

	client.deliver("only-fan-controller/cmd/hint",
		[]byte(`{"type": "transcode", "action": "stop", "source": "plex"}`))

	consumer.mu.Lock()
	defer consumer.mu.Unlock()
	if len(consumer.removedSources) != 1 || consumer.removedSources[0] != "plex" {
		t.Fatalf("removed sources = %v, want [plex]", consumer.removedSources)
	}
}

func TestHintCommandRejectsBadSource(t *testing.T) {
	consumer := &fakeConsumer{}
	client := startBridge(t, consumer)

	client.deliver("only-fan-controller/cmd/hint",
		[]byte(`{"type": "transcode", "action": "start", "source": "bad source!"}`))

	consumer.mu.Lock()
	defer consumer.mu.Unlock()
	if len(consumer.addedHints) != 0 {
		t.Fatal("hint with invalid source should be rejected")
	}
}

func TestSubscribeCommandsRegistersAllTopics(t *testing.T) {
	consumer := &fakeConsumer{}
	client := startBridge(t, consumer)

	client.mu.Lock()
	defer client.mu.Unlock()
	for _, topic := range []string{
		"only-fan-controller/cmd/override",
		"only-fan-controller/cmd/override/clear",
		"only-fan-controller/cmd/hint",
	} {
		if _, ok := client.subs[topic]; !ok {
			t.Fatalf("not subscribed to %q", topic)
		}
	}
}
```

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/mqtt/ -run 'TestOverrideCommand|TestClearCommand|TestHintCommand|TestSubscribeCommands' -v`
Expected: FAIL — `subscribeCommands`/`handle*Command` undefined and no command subscriptions registered.

- [x] **Step 3: Create the command handlers**

Create `internal/mqtt/command.go`:

```go
package mqtt

import (
	"encoding/json"
	"log"
	"time"

	"github.com/sethpjohnson/only-fan-controller/internal/controller"
	"github.com/sethpjohnson/only-fan-controller/internal/validate"
)

// overrideCommand is the JSON schema for <base_topic>/cmd/override.
type overrideCommand struct {
	Speed           int    `json:"speed"`
	DurationSeconds int    `json:"duration_seconds"`
	Reason          string `json:"reason"`
}

// hintCommand is the JSON schema for <base_topic>/cmd/hint, mirroring the HTTP
// POST /api/hint body.
type hintCommand struct {
	Type             string `json:"type"`
	Action           string `json:"action"`
	Intensity        string `json:"intensity"`
	Source           string `json:"source"`
	DurationEstimate int    `json:"duration_estimate"`
}

// subscribeCommands registers QoS-1 handlers for the three command topics. It
// runs from onConnect, so it re-subscribes on every (re)connection.
func (b *Bridge) subscribeCommands() {
	subs := []struct {
		topic   string
		handler MessageHandler
	}{
		{b.cmdOverrideTopic(), func(_ string, p []byte) { b.handleOverrideCommand(p) }},
		{b.cmdOverrideClearTopic(), func(_ string, p []byte) { b.handleClearCommand(p) }},
		{b.cmdHintTopic(), func(_ string, p []byte) { b.handleHintCommand(p) }},
	}
	for _, s := range subs {
		if err := b.client.Subscribe(s.topic, 1, s.handler); err != nil {
			log.Printf("MQTT: failed to subscribe to %s: %v", s.topic, err)
		}
	}
}

// handleOverrideCommand parses, validates, and applies an override. Validation
// reuses the shared validate package; final clamping/cap live in the controller.
func (b *Bridge) handleOverrideCommand(payload []byte) {
	var cmd overrideCommand
	if err := json.Unmarshal(payload, &cmd); err != nil {
		log.Printf("MQTT: invalid override command: %v", err)
		return
	}
	if err := validate.OverrideSpeed(cmd.Speed); err != nil {
		log.Printf("MQTT: rejecting override command: %v", err)
		return
	}
	if err := validate.OverrideReason(cmd.Reason); err != nil {
		log.Printf("MQTT: rejecting override command: %v", err)
		return
	}
	duration := time.Duration(cmd.DurationSeconds) * time.Second
	b.consumer.SetOverride(cmd.Speed, duration, cmd.Reason)
	log.Printf("MQTT: override set via command: %d%% (%s)", cmd.Speed, cmd.Reason)
}

// handleClearCommand clears any active override. The payload is ignored (any
// message on the clear topic triggers it), mirroring an HA button press.
func (b *Bridge) handleClearCommand(_ []byte) {
	b.consumer.ClearOverride()
	log.Printf("MQTT: override cleared via command")
}

// handleHintCommand parses, validates, and applies a workload hint. action
// "stop" removes the hint by source; anything else starts it.
func (b *Bridge) handleHintCommand(payload []byte) {
	var cmd hintCommand
	if err := json.Unmarshal(payload, &cmd); err != nil {
		log.Printf("MQTT: invalid hint command: %v", err)
		return
	}
	if err := validate.HintField("source", cmd.Source); err != nil {
		log.Printf("MQTT: rejecting hint command: %v", err)
		return
	}
	if err := validate.HintField("type", cmd.Type); err != nil {
		log.Printf("MQTT: rejecting hint command: %v", err)
		return
	}
	if err := validate.HintAction(cmd.Action); err != nil {
		log.Printf("MQTT: rejecting hint command: %v", err)
		return
	}
	if err := validate.Intensity(cmd.Intensity); err != nil {
		log.Printf("MQTT: rejecting hint command: %v", err)
		return
	}
	if cmd.DurationEstimate < 0 {
		log.Printf("MQTT: rejecting hint command: duration_estimate must not be negative")
		return
	}

	if cmd.Action == "stop" {
		b.consumer.RemoveHint(cmd.Source)
		log.Printf("MQTT: hint removed via command: %s", cmd.Source)
		return
	}

	hint := &controller.WorkloadHint{
		Type:      cmd.Type,
		Action:    cmd.Action,
		Intensity: cmd.Intensity,
		Source:    cmd.Source,
	}
	if cmd.DurationEstimate > 0 {
		hint.ExpiresAt = time.Now().Add(time.Duration(cmd.DurationEstimate) * time.Second)
	}
	b.consumer.AddHint(hint)
	log.Printf("MQTT: hint registered via command: %s from %s", cmd.Action, cmd.Source)
}
```

- [x] **Step 4: Subscribe to commands on connect**

In `internal/mqtt/bridge.go`, update `onConnect` to also subscribe:

```go
func (b *Bridge) onConnect() {
	b.publishDiscovery()
	b.subscribeCommands()
	b.publishAvailability(true)
}
```

- [x] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/mqtt/ -run 'TestOverrideCommand|TestClearCommand|TestHintCommand|TestSubscribeCommands' -v`
Expected: PASS.

- [x] **Step 6: Run the whole mqtt package suite**

Run: `go test ./internal/mqtt/ -v`
Expected: PASS (all unit tests across Tasks 2-6).

- [x] **Step 7: Commit**

```bash
git add internal/mqtt/command.go internal/mqtt/bridge.go internal/mqtt/command_test.go
git commit --no-gpg-sign -m "feat(mqtt): route override/clear/hint commands with shared validation"
```

---

## Task 7: Wire the bridge into `main.go` with shutdown sequencing

**Files:**
- Modify: `cmd/controller/main.go` (`run()`)

**Interfaces:**
- Consumes: `mqtt.New`, `mqtt.NewPahoClient`, `(*Bridge).Start`, `(*Bridge).Stop` (Tasks 2-6); `*controller.FanController` (satisfies `mqtt.Consumer`); `cfg.MQTT.Enabled` (Task 1).
- Produces: no new exported API; the running program starts/stops the bridge.

- [x] **Step 1: Add the mqtt import**

In `cmd/controller/main.go`, add to the import block:

```go
	"github.com/sethpjohnson/only-fan-controller/internal/mqtt"
```

- [x] **Step 2: Add the shutdown-timeout constant**

Near `cleanupShutdownTimeout` (line 135), add:

```go
// mqttShutdownTimeout bounds how long shutdown waits for the MQTT bridge to
// publish offline + disconnect. Thermal safety (restore()) always runs before
// this, so a wedged broker must never stall process exit.
const mqttShutdownTimeout = 3 * time.Second
```

- [x] **Step 3: Start the bridge after the API server starts**

In `run()`, after the `log.Printf("Dashboard: ...")` line (around line 274) and before the signal-handling block, add:

```go
	// Optional MQTT / Home Assistant bridge. Off unless mqtt.enabled. It talks to
	// the controller only through the exported Consumer methods (the same ones the
	// HTTP handlers use), so a hung/unreachable broker can never stall fan control.
	// fanCtrl is a *controller.FanController in both real and demo mode, so the
	// bridge (and thus HA) works in demo mode too.
	var mqttBridge *mqtt.Bridge
	if cfg.MQTT.Enabled {
		mqttBridge = mqtt.New(cfg, fanCtrl, mqtt.NewPahoClient)
		mqttBridge.Start()
	}
```

- [x] **Step 4: Stop the bridge after restore(), bounded**

In `run()`, immediately after the `restore()` block (the `if err := restore(); err != nil { ... }` around lines 293-295) and before the history-cleanup shutdown, add:

```go
	// Tear down the MQTT bridge AFTER the BMC hand-back choke point, bounded so a
	// wedged broker cannot delay exit. Mirrors the history-cleanup shutdown pattern.
	if mqttBridge != nil {
		stopped := make(chan struct{})
		go func() {
			mqttBridge.Stop()
			close(stopped)
		}()
		select {
		case <-stopped:
		case <-time.After(mqttShutdownTimeout):
			log.Printf("Warning: MQTT bridge did not stop within %s; abandoning it", mqttShutdownTimeout)
		}
	}
```

- [x] **Step 5: Verify the program builds and the existing main tests pass**

Run: `go build ./... && go test ./cmd/controller/ -v`
Expected: build succeeds; all `cmd/controller` tests PASS (including `TestApplyEnvOverridesMQTT` from Task 1).

- [x] **Step 6: Smoke-test that demo mode with MQTT disabled behaves identically**

Run: `go run ./cmd/controller -demo -config /nonexistent.yaml 2>&1 | head -n 12`
Expected: normal demo startup logs; NO `MQTT:` lines appear (bridge is off by default). Stop with Ctrl-C.

- [x] **Step 7: Commit**

```bash
git add cmd/controller/main.go
git commit --no-gpg-sign -m "feat(mqtt): wire bridge into run() with bounded post-restore shutdown"
```

---

## Task 8: Integration test with an in-process mochi-mqtt broker

**Files:**
- Create: `internal/mqtt/integration_test.go`
- Modify: `go.mod`, `go.sum` (add `github.com/mochi-mqtt/server/v2` test dep)

**Interfaces:**
- Consumes: everything from Tasks 2-6 (real `NewPahoClient`, `New`, `Start`, `Stop`, `publishState`); `fakeConsumer` (Task 2 test helper).
- Produces: no production code; a real connect → discovery → command → state → LWT round-trip.

- [x] **Step 1: Add the mochi-mqtt test dependency**

Run:
```bash
go get github.com/mochi-mqtt/server/v2@v2.6.6
```
Expected: `go.mod`/`go.sum` updated with `github.com/mochi-mqtt/server/v2 v2.6.6`.

- [x] **Step 2: Write the integration test**

Create `internal/mqtt/integration_test.go`:

```go
package mqtt

import (
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"
	mochi "github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/hooks/auth"
	"github.com/mochi-mqtt/server/v2/listeners"

	"github.com/sethpjohnson/only-fan-controller/internal/config"
	"github.com/sethpjohnson/only-fan-controller/internal/controller"
	"github.com/sethpjohnson/only-fan-controller/internal/monitor"
)

// freeAddr returns a currently-free 127.0.0.1 TCP address for the broker.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to grab a free port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// startBroker starts an in-process mochi broker on addr and returns it.
func startBroker(t *testing.T, addr string) *mochi.Server {
	t.Helper()
	server := mochi.New(&mochi.Options{InlineClient: false})
	if err := server.AddHook(new(auth.AllowHook), nil); err != nil {
		t.Fatalf("add auth hook: %v", err)
	}
	tcp := listeners.NewTCP(listeners.Config{ID: "t1", Address: addr})
	if err := server.AddListener(tcp); err != nil {
		t.Fatalf("add listener: %v", err)
	}
	go func() { _ = server.Serve() }()
	t.Cleanup(func() { _ = server.Close() })
	return server
}

// observer subscribes to "#" and records the FULL per-topic history, so the test
// can assert on discovery/state/availability traffic. History (not just the
// latest value) matters for the LWT check: paho auto-reconnects and republishes
// "online" moments after the forced disconnect, so we must assert that "offline"
// appeared at some point, not that it is the latest value.
type observer struct {
	mu      sync.Mutex
	history map[string][]string
	client  paho.Client
}

func newObserver(t *testing.T, broker string) *observer {
	t.Helper()
	o := &observer{history: map[string][]string{}}
	opts := paho.NewClientOptions().AddBroker(broker).SetClientID("observer")
	o.client = paho.NewClient(opts)
	tok := o.client.Connect()
	if !tok.WaitTimeout(3*time.Second) || tok.Error() != nil {
		t.Fatalf("observer connect failed: %v", tok.Error())
	}
	sub := o.client.Subscribe("#", 1, func(_ paho.Client, m paho.Message) {
		o.mu.Lock()
		o.history[m.Topic()] = append(o.history[m.Topic()], string(m.Payload()))
		o.mu.Unlock()
	})
	sub.Wait()
	t.Cleanup(func() { o.client.Disconnect(100) })
	return o
}

// latest returns the most recent payload seen on topic.
func (o *observer) latest(topic string) (string, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	h := o.history[topic]
	if len(h) == 0 {
		return "", false
	}
	return h[len(h)-1], true
}

// seen reports whether payload was ever observed on topic.
func (o *observer) seen(topic, payload string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, p := range o.history[topic] {
		if p == payload {
			return true
		}
	}
	return false
}

// waitFor polls cond up to 3s; fails the test with msg otherwise.
func waitFor(t *testing.T, msg string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", msg)
}

func TestIntegrationConnectDiscoveryCommandStateLWT(t *testing.T) {
	addr := freeAddr(t)
	server := startBroker(t, addr)
	broker := "tcp://" + addr

	obs := newObserver(t, broker)

	cfg := config.Default()
	cfg.MQTT.Enabled = true
	cfg.MQTT.Broker = broker
	cfg.Monitoring.Interval = 1
	consumer := &fakeConsumer{status: &controller.Status{
		CPU:          &monitor.CPUReading{Max: 55},
		CurrentSpeed: 42,
		Zone:         "warm",
	}}

	bridge := New(cfg, consumer, NewPahoClient)
	bridge.Start()
	t.Cleanup(func() { bridge.Stop() })

	// 1. Availability online + discovery published on connect.
	waitFor(t, "availability online", func() bool {
		return obs.seen("only-fan-controller/availability", "online")
	})
	waitFor(t, "cpu_temp discovery", func() bool {
		_, ok := obs.latest("homeassistant/sensor/only-fan-controller/cpu_temp/config")
		return ok
	})

	// 2. Command round-trip: publish an override command, expect SetOverride.
	pub := obs.client.Publish("only-fan-controller/cmd/override", 1, false,
		[]byte(`{"speed": 70, "duration_seconds": 600, "reason": "integration"}`))
	pub.Wait()
	waitFor(t, "SetOverride called", func() bool {
		consumer.mu.Lock()
		defer consumer.mu.Unlock()
		return consumer.overrideSet && consumer.overrideSpeed == 70
	})

	// 3. State publish (ticker at 1s interval) reaches the state topic.
	waitFor(t, "state published", func() bool {
		v, ok := obs.latest("only-fan-controller/state")
		return ok && len(v) > 0
	})

	// 4. LWT: forcibly stop the bridge's broker-side connection (ungraceful) and
	// expect the retained will "offline" to be published.
	waitFor(t, "bridge client registered on broker", func() bool {
		_, ok := server.Clients.Get(cfg.MQTT.ClientID)
		return ok
	})
	cl, _ := server.Clients.Get(cfg.MQTT.ClientID)
	cl.Stop(errors.New("integration: force ungraceful disconnect"))
	waitFor(t, "LWT offline", func() bool {
		return obs.seen("only-fan-controller/availability", "offline")
	})
}
```

The import block above is complete: `config` (`config.Default()`), `controller` (`controller.Status`), and `monitor` (`monitor.CPUReading`) are all listed, and `fmt` is deliberately absent. If a future edit adds an unused import, `go vet` will flag it — keep the block tight.

- [x] **Step 3: Run the integration test**

Run: `go test ./internal/mqtt/ -run TestIntegration -v`
Expected: PASS — the log shows connect, discovery, command routing, state, and the LWT offline message observed.

- [x] **Step 4: Run the full mqtt suite (unit + integration) with the race detector**

Run: `go test ./internal/mqtt/ -race -v`
Expected: PASS with no data races.

- [x] **Step 5: Commit**

```bash
git add go.mod go.sum internal/mqtt/integration_test.go
git commit --no-gpg-sign -m "test(mqtt): in-process mochi broker integration (discovery/command/state/LWT)"
```

---

## Task 9: Documentation, Unraid template, and `/api/config` leak regression

**Files:**
- Modify: `README.md`
- Modify: `unraid/only-fan-controller.xml`
- Test: `internal/api/server_test.go` (add `/api/config` password-leak regression)

**Interfaces:**
- Consumes: `handleGetConfig` (existing); the running server test harness in `server_test.go`.
- Produces: docs + a regression test guaranteeing `/api/config` never exposes `mqtt.password`.

- [x] **Step 1: Write the failing `/api/config` regression test**

Add to `internal/api/server_test.go`. This assumes the existing test harness exposes a way to build a server and issue a request; use the same pattern the other tests in that file use (a `*config.Config`, `NewServer`, and `httptest`). Concretely:

```go
func TestGetConfigDoesNotLeakMQTTPassword(t *testing.T) {
	cfg := config.Default()
	cfg.MQTT.Enabled = true
	cfg.MQTT.Broker = "tcp://10.0.0.5:1883"
	cfg.MQTT.Password = "super-secret-broker-pw"

	srv := NewServer(cfg, &controller.FanController{}, &storage.Store{})

	req := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	rec := httptest.NewRecorder()
	srv.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/config = %d, want 200", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "super-secret-broker-pw") {
		t.Fatalf("/api/config leaked mqtt.password: %s", rec.Body.String())
	}
}
```

Ensure the test file imports `net/http`, `net/http/httptest`, `strings`, `github.com/sethpjohnson/only-fan-controller/internal/config`, `.../internal/controller`, and `.../internal/storage`. If `server_test.go` already constructs servers via a local helper (e.g. `newTestServer(t, cfg)`), use that helper instead of calling `NewServer` directly with zero-value dependencies, and drop the unused imports. (Read the top of `server_test.go` first and match its established construction pattern; the assertion — body must not contain the password — is the load-bearing part.)

- [x] **Step 2: Run the regression test to verify it passes**

Run: `go test ./internal/api/ -run TestGetConfigDoesNotLeakMQTTPassword -v`
Expected: PASS immediately — `handleGetConfig` builds a hand-picked map that never includes `mqtt`, so the password cannot appear. (This test locks that behavior in against future edits; if it FAILS, `handleGetConfig` was changed to include MQTT and must be corrected.)

- [x] **Step 3: Add the "Home Assistant (MQTT)" section to the README**

Insert a new section in `README.md` after the `## API Security` section (before `## API Endpoints`, around line 157):

````markdown
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
| CPU Temperature, GPU Temperature | sensor | °C |
| Fan Speed, Target Fan Speed | sensor | % |
| Thermal Zone, Failsafe Reason | sensor | text |
| Failsafe Active, Restore Pending, Last Fan Write Failed | binary_sensor | `problem` class |
| Override Fan Speed | number | slider bound to `min_speed`/`max_speed`; sends a 1-hour override |
| Clear Fan Override | button | clears any active override |

The device is marked **unavailable** the instant the process dies — the broker
publishes the Last Will (`offline`) on ungraceful disconnect, giving you free
external monitoring for the fail-safe scenarios.

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

Hints have no dedicated HA entity (a source/type/intensity tuple doesn't map to
one) — drive `cmd/hint` from an HA automation for the full-parity escape hatch.

### Security

**MQTT command authorization is your broker's authentication.** The HTTP bearer
token is HTTP-only and does not apply here — anyone who can publish to the broker
can command the fans, so protect it with broker credentials/ACLs. The blast
radius is still bounded by the controller-level clamps and the critical-temp
override. **There is no TLS in v1**; keep the broker on a trusted LAN or in front
of a TLS-terminating proxy.
````

- [x] **Step 4: Add the MQTT env-var rows to the README table**

In the `## Environment Variables` table (around line 319), add after the `API_TOKEN` row:

```markdown
| `MQTT_ENABLED` | Enable Home Assistant MQTT bridge | false |
| `MQTT_BROKER` | MQTT broker URL (`tcp://host:port`) | - (required when enabled) |
| `MQTT_USERNAME` | MQTT broker username | - |
| `MQTT_PASSWORD` | MQTT broker password | - |
```

- [x] **Step 5: Add the MQTT fields to the Unraid template**

In `unraid/only-fan-controller.xml`, add these `<Config>` elements after the `API Token` entry (before `Enable GPU Monitoring`):

```xml
  <Config Name="Enable MQTT" Target="MQTT_ENABLED" Default="false" Mode="" Description="Enable the Home Assistant MQTT bridge (self-registers via MQTT Discovery)" Type="Variable" Display="always" Required="false" Mask="false">false</Config>
  <Config Name="MQTT Broker" Target="MQTT_BROKER" Default="" Mode="" Description="MQTT broker URL, e.g. tcp://192.168.1.10:1883 (required when MQTT is enabled)" Type="Variable" Display="always" Required="false" Mask="false"/>
  <Config Name="MQTT Username" Target="MQTT_USERNAME" Default="" Mode="" Description="MQTT broker username (optional)" Type="Variable" Display="always" Required="false" Mask="false"/>
  <Config Name="MQTT Password" Target="MQTT_PASSWORD" Default="" Mode="" Description="MQTT broker password (optional)" Type="Variable" Display="always" Required="false" Mask="true"/>
```

- [x] **Step 6: Verify the full test suite passes**

Run: `go test ./...`
Expected: PASS across all packages (`config`, `api`, `validate`, `mqtt`, `controller`, `cmd/controller`, …).

- [x] **Step 7: Commit**

```bash
git add README.md unraid/only-fan-controller.xml internal/api/server_test.go
git commit --no-gpg-sign -m "docs(mqtt): README HA section, Unraid MQTT fields, /api/config leak regression"
```

---

## Final verification

- [ ] Run the whole suite with the race detector: `go test ./... -race`
  Expected: PASS, no data races.
- [ ] `go vet ./...` — no findings.
- [ ] `go build ./...` — succeeds.
- [ ] Confirm `mqtt.enabled: false` (default) produces zero `MQTT:` log lines in a demo run (Task 7 Step 6) — the off-by-default guarantee.
