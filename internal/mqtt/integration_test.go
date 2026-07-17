package mqtt

import (
	"errors"
	"fmt"
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

func TestIntegrationRetainedCommandIsDropped(t *testing.T) {
	addr := freeAddr(t)
	startBroker(t, addr)
	broker := "tcp://" + addr

	obs := newObserver(t, broker)

	// Pre-publish a RETAINED override command. The broker will replay it to the
	// bridge the instant it subscribes on connect.
	pub := paho.NewClient(paho.NewClientOptions().AddBroker(broker).SetClientID("retain-pub"))
	if tok := pub.Connect(); !tok.WaitTimeout(3*time.Second) || tok.Error() != nil {
		t.Fatalf("retain publisher connect failed: %v", tok.Error())
	}
	rt := pub.Publish("only-fan-controller/cmd/override", 1, true,
		[]byte(`{"speed": 88, "duration_seconds": 600, "reason": "retained-replay"}`))
	rt.Wait()
	pub.Disconnect(100)

	cfg := config.Default()
	cfg.MQTT.Enabled = true
	cfg.MQTT.Broker = broker
	consumer := &fakeConsumer{status: &controller.Status{}}
	bridge := New(cfg, consumer, NewPahoClient)
	bridge.Start()
	t.Cleanup(func() { bridge.Stop() })

	// onConnect subscribes to the command topics BEFORE it publishes
	// availability=online, so once we observe "online" the broker has already
	// delivered the retained command to the bridge's subscription.
	waitFor(t, "bridge online", func() bool {
		return obs.seen("only-fan-controller/availability", "online")
	})
	// Give the retained delivery a moment to be processed (and dropped).
	time.Sleep(300 * time.Millisecond)

	consumer.mu.Lock()
	replayed := consumer.overrideSet
	consumer.mu.Unlock()
	if replayed {
		t.Fatal("retained command was replayed into the controller on connect")
	}

	// A normal (non-retained) command must still be applied, proving the drop is
	// specific to retained delivery and not a dead command path.
	live := obs.client.Publish("only-fan-controller/cmd/override", 1, false,
		[]byte(`{"speed": 71, "duration_seconds": 600, "reason": "live"}`))
	live.Wait()
	waitFor(t, "live command applied", func() bool {
		consumer.mu.Lock()
		defer consumer.mu.Unlock()
		return consumer.overrideSet && consumer.overrideSpeed == 71
	})
}

// TestIntegrationGPUDiscoveryAppearsAfterFirstRead reproduces the live-hardware
// cold-start bug against a real broker: MQTT connects before the first GPU read
// (no devices at connect), so per-GPU discovery must NOT be published at connect,
// but MUST appear on a later tick once the devices are known.
func TestIntegrationGPUDiscoveryAppearsAfterFirstRead(t *testing.T) {
	addr := freeAddr(t)
	startBroker(t, addr)
	broker := "tcp://" + addr

	obs := newObserver(t, broker)

	cfg := config.Default()
	cfg.MQTT.Enabled = true
	cfg.MQTT.Broker = broker
	cfg.Monitoring.Interval = 1
	// Connect-before-first-read: GPU is nil at startup.
	consumer := &fakeConsumer{status: &controller.Status{CPU: &monitor.CPUReading{Max: 50}}}

	bridge := New(cfg, consumer, NewPahoClient)
	bridge.Start()
	t.Cleanup(func() { bridge.Stop() })

	// Wait until the bridge is connected (availability online).
	waitFor(t, "availability online", func() bool {
		return obs.seen("only-fan-controller/availability", "online")
	})
	// The aggregate discovery is present, but NO per-GPU discovery yet.
	waitFor(t, "aggregate gpu_temp discovery", func() bool {
		_, ok := obs.latest("homeassistant/sensor/only-fan-controller/gpu_temp/config")
		return ok
	})
	if _, ok := obs.latest("homeassistant/sensor/only-fan-controller/gpu0_temp/config"); ok {
		t.Fatal("per-GPU discovery published at connect, before the first GPU read")
	}

	// The first GPU read completes: two cards appear.
	consumer.setStatus(gpuStatus(2))

	// Within a tick or two, all three sensors for both cards must be announced.
	for i := 0; i < 2; i++ {
		for _, suffix := range []string{"temp", "utilization", "power"} {
			topic := fmt.Sprintf("homeassistant/sensor/only-fan-controller/gpu%d_%s/config", i, suffix)
			waitFor(t, "per-GPU discovery "+topic, func() bool {
				_, ok := obs.latest(topic)
				return ok
			})
		}
	}
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
