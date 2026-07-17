package mqtt

import (
	"bytes"
	"errors"
	"log"
	"os"
	"strings"
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
	// publishErr, when non-nil, makes every Publish fail with it (simulating an
	// unreachable/wedged broker where the paho token times out).
	publishErr error
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
	return f.publishErr
}

// setPublishErr toggles whether subsequent Publish calls fail.
func (f *fakeClient) setPublishErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.publishErr = err
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

// setStatus swaps the status returned by GetStatus, simulating the control loop
// populating GPU devices after the first read.
func (c *fakeConsumer) setStatus(s *controller.Status) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.status = s
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

// TestStartNormalizesBroker verifies the bridge hands the paho client a
// normalized broker (bare host -> tcp://host:1883) rather than the raw config
// value.
func TestStartNormalizesBroker(t *testing.T) {
	cfg := testConfig()
	cfg.MQTT.Broker = "192.168.1.50"
	h := &clientHolder{}
	b := New(cfg, &fakeConsumer{}, h.factory)
	b.Start()

	if h.client.opts.Broker != "tcp://192.168.1.50:1883" {
		t.Fatalf("client broker = %q, want tcp://192.168.1.50:1883", h.client.opts.Broker)
	}
	if b.broker != "tcp://192.168.1.50:1883" {
		t.Fatalf("bridge broker = %q, want tcp://192.168.1.50:1883", b.broker)
	}
}

// TestUnreachableBrokerHintOnce verifies the "unable to reach broker" hint is
// logged once while publishing keeps failing, then rearms after a success.
func TestUnreachableBrokerHintOnce(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(os.Stderr)
		log.SetFlags(log.LstdFlags)
	})

	h := &clientHolder{}
	b := New(testConfig(), &fakeConsumer{}, h.factory)
	b.Start()
	h.client.setPublishErr(errors.New("publish to x timed out"))

	const hint = "still unable to reach broker"
	for i := 0; i < 3; i++ {
		b.publishState()
	}
	if got := strings.Count(buf.String(), hint); got != 1 {
		t.Fatalf("hint logged %d times across 3 failing publishes, want 1", got)
	}

	// A successful publish rearms the hint so a later outage surfaces again.
	h.client.setPublishErr(nil)
	b.publishState()
	buf.Reset()
	h.client.setPublishErr(errors.New("publish to x timed out"))
	b.publishState()
	if got := strings.Count(buf.String(), hint); got != 1 {
		t.Fatalf("hint logged %d times after rearm, want 1", got)
	}
}

// TestHAWebUIPortWarning verifies a broker resolving to port 8123 triggers the
// startup warning about Home Assistant's web UI port.
func TestHAWebUIPortWarning(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(os.Stderr)
		log.SetFlags(log.LstdFlags)
	})

	cfg := testConfig()
	cfg.MQTT.Broker = "tcp://homeassistant.local:8123"
	h := &clientHolder{}
	b := New(cfg, &fakeConsumer{}, h.factory)
	b.Start()

	if !strings.Contains(buf.String(), "8123") || !strings.Contains(buf.String(), "web UI") {
		t.Fatalf("expected HA web UI port warning, log was:\n%s", buf.String())
	}
}
