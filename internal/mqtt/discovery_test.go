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

func TestCPUAggregateSensorLabel(t *testing.T) {
	h := &clientHolder{}
	b := New(testConfig(), &fakeConsumer{}, h.factory)
	var payload map[string]any
	for _, e := range b.discoveryEntities() {
		if e.topic == "homeassistant/sensor/only-fan-controller/cpu_temp/config" {
			if err := json.Unmarshal(e.payload, &payload); err != nil {
				t.Fatalf("unmarshal cpu_temp config: %v", err)
			}
		}
	}
	if payload == nil {
		t.Fatal("cpu_temp aggregate sensor not found")
	}
	if payload["name"].(string) != "CPU Temperature (Max)" {
		t.Fatalf("cpu_temp name = %v, want %q", payload["name"], "CPU Temperature (Max)")
	}
	// The aggregate binds to the flat cpu_temp key (the max), NOT the per-socket array.
	if payload["value_template"].(string) != "{{ value_json.cpu_temp }}" {
		t.Fatalf("cpu_temp value_template = %v", payload["value_template"])
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
