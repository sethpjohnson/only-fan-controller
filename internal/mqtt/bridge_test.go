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

// deliver invokes the handler registered for topic, simulating a live (non-
// retained) inbound broker message.
func (f *fakeClient) deliver(topic string, payload []byte) {
	f.deliverMsg(topic, payload, false)
}

// deliverRetained simulates the broker replaying a retained message on
// (re)subscribe, so tests can assert that command handlers reject it.
func (f *fakeClient) deliverRetained(topic string, payload []byte) {
	f.deliverMsg(topic, payload, true)
}

func (f *fakeClient) deliverMsg(topic string, payload []byte, retained bool) {
	f.mu.Lock()
	h := f.subs[topic]
	f.mu.Unlock()
	if h != nil {
		h(topic, payload, retained)
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
