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
