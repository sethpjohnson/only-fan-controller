package mqtt

import (
	"bytes"
	"log"
	"strings"
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

func TestRetainedCommandIsDroppedAndLoggedOnce(t *testing.T) {
	consumer := &fakeConsumer{}
	client := startBridge(t, consumer)

	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(old)

	// A retained override command must NOT reach the controller (it would replay
	// on every reconnect/restart otherwise).
	client.deliverRetained("only-fan-controller/cmd/override",
		[]byte(`{"speed": 88, "duration_seconds": 600, "reason": "retained-replay"}`))
	// A second retained delivery on the same topic must not log again.
	client.deliverRetained("only-fan-controller/cmd/override",
		[]byte(`{"speed": 88, "duration_seconds": 600, "reason": "retained-replay"}`))

	consumer.mu.Lock()
	set := consumer.overrideSet
	consumer.mu.Unlock()
	if set {
		t.Fatal("retained override command must not be applied to the controller")
	}

	logged := buf.String()
	if !strings.Contains(logged, "dropping RETAINED message") ||
		!strings.Contains(logged, "only-fan-controller/cmd/override") {
		t.Fatalf("expected a dropped-retained log line, got: %q", logged)
	}
	if got := strings.Count(logged, "dropping RETAINED message"); got != 1 {
		t.Fatalf("expected the drop to be logged exactly once per topic, got %d", got)
	}

	// A subsequent live (non-retained) command on the same topic must still work,
	// proving the drop is specific to retained delivery.
	client.deliver("only-fan-controller/cmd/override",
		[]byte(`{"speed": 61, "duration_seconds": 600, "reason": "live"}`))
	consumer.mu.Lock()
	defer consumer.mu.Unlock()
	if !consumer.overrideSet || consumer.overrideSpeed != 61 {
		t.Fatalf("live command after a retained drop was not applied: set=%v speed=%d", consumer.overrideSet, consumer.overrideSpeed)
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
