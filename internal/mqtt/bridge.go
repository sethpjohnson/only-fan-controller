package mqtt

import (
	"log"
	"sync"
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

// Bridge publishes controller state to MQTT and routes MQTT commands into the
// controller. It owns its own goroutines and never shares a lock with the
// control loop beyond the Consumer.GetStatus read path.
type Bridge struct {
	cfg        *config.Config
	consumer   Consumer
	newClient  ClientFactory
	client     Client
	stopCh     chan struct{}
	doneCh     chan struct{}
	pubLimiter rateLimiter
	// broker is the normalized broker address (set in Start) used in log
	// messages, including the unreachable-broker hint.
	broker string
	// unreachableHint gates the one-time "still unable to reach broker" hint so a
	// wrong host/port surfaces once instead of on every failing publish tick.
	unreachableHint onceHint
	// gpuDiscovered tracks which GPU device indices have had their discovery
	// configs published on the CURRENT connection, so each card is announced
	// exactly once per connection rather than on every publish tick. It is guarded
	// by gpuDiscMu because it is cleared from the OnConnect goroutine and
	// checked/updated from the publish-loop goroutine. onConnect clears it so a
	// reconnect re-announces every card (keeping HA's retained configs fresh).
	gpuDiscMu     sync.Mutex
	gpuDiscovered map[int]bool
	// retainWarned tracks command topics we have already logged a dropped
	// retained message for, so reconnect replays warn once per topic instead of
	// flooding the log.
	retainWarnMu sync.Mutex
	retainWarned map[string]bool
}

// onceHint fires at most once until it is rearmed. It backs the one-time
// unreachable-broker hint: arm()-ing on a failing publish returns true only the
// first time, and rearm() (called after a successful publish) lets the hint fire
// again if the broker later drops out.
type onceHint struct {
	mu    sync.Mutex
	shown bool
}

func (h *onceHint) arm() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.shown {
		return false
	}
	h.shown = true
	return true
}

func (h *onceHint) rearm() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.shown = false
}

// New builds a Bridge. factory is NewPahoClient in production and a fake in
// tests. Start actually connects.
func New(cfg *config.Config, consumer Consumer, factory ClientFactory) *Bridge {
	return &Bridge{
		cfg:           cfg,
		consumer:      consumer,
		newClient:     factory,
		gpuDiscovered: map[int]bool{},
		retainWarned:  map[string]bool{},
	}
}

func (b *Bridge) availabilityTopic() string     { return b.cfg.MQTT.BaseTopic + "/availability" }
func (b *Bridge) stateTopic() string            { return b.cfg.MQTT.BaseTopic + "/state" }
func (b *Bridge) cmdOverrideTopic() string      { return b.cfg.MQTT.BaseTopic + "/cmd/override" }
func (b *Bridge) cmdOverrideClearTopic() string { return b.cfg.MQTT.BaseTopic + "/cmd/override/clear" }
func (b *Bridge) cmdHintTopic() string          { return b.cfg.MQTT.BaseTopic + "/cmd/hint" }

// Start builds the client and initiates connection. Connect is non-blocking, so
// an unreachable broker does not delay startup. onConnect (fired on every
// (re)connection) republishes availability.
func (b *Bridge) Start() {
	broker, note := normalizeBrokerURL(b.cfg.MQTT.Broker)
	if note != "" {
		log.Printf("MQTT: broker %q interpreted as %q (%s)", b.cfg.MQTT.Broker, broker, note)
	}
	if hostPortIsHAWebUI(broker) {
		log.Printf("MQTT: WARNING broker %q uses port 8123, which is typically Home Assistant's web UI, not the MQTT broker — Mosquitto usually listens on 1883", broker)
	}
	b.broker = broker
	opts := ClientOptions{
		Broker:            broker,
		ClientID:          b.cfg.MQTT.ClientID,
		Username:          b.cfg.MQTT.Username,
		Password:          b.cfg.MQTT.Password,
		AvailabilityTopic: b.availabilityTopic(),
		OnlinePayload:     "online",
		OfflinePayload:    "offline",
		OnConnect:         b.onConnect,
	}
	b.client = b.newClient(opts)
	log.Printf("MQTT: bridge starting (broker %s, base topic %s)", broker, b.cfg.MQTT.BaseTopic)
	if err := b.client.Connect(); err != nil {
		log.Printf("MQTT: initial connect error (will keep retrying in background): %v", err)
	}
	b.stopCh = make(chan struct{})
	b.doneCh = make(chan struct{})
	go b.publishLoop()
}

// onConnect runs on every successful (re)connection. It publishes discovery,
// subscribes to the command topics, then marks availability online — so HA has
// the entities registered and the command paths live before it sees them go
// online.
//
// Per-GPU discovery is deliberately NOT published here: at cold start MQTT
// connects before the control loop's first GPU read, so the device set is still
// empty. It is announced lazily from the publish loop as devices appear (see
// publishNewGPUDiscovery). Clearing gpuDiscovered on every (re)connect makes that
// loop re-announce every card on the new connection.
func (b *Bridge) onConnect() {
	b.resetGPUDiscovery()
	b.publishDiscovery()
	b.subscribeCommands()
	b.publishAvailability(true)
}

// resetGPUDiscovery forgets which GPU indices have been announced, so the publish
// loop re-publishes per-GPU discovery on the next tick. Called on (re)connect.
func (b *Bridge) resetGPUDiscovery() {
	b.gpuDiscMu.Lock()
	b.gpuDiscovered = map[int]bool{}
	b.gpuDiscMu.Unlock()
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
