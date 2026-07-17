package mqtt

import (
	"encoding/json"
	"fmt"
	"regexp"
	"testing"

	"github.com/sethpjohnson/only-fan-controller/internal/controller"
	"github.com/sethpjohnson/only-fan-controller/internal/monitor"
)

// gpuStatus builds a controller status with n GPU devices carrying distinct,
// recognizable per-card values.
func gpuStatus(n int) *controller.Status {
	devices := make([]monitor.GPUDevice, n)
	maxTemp := 0
	for i := 0; i < n; i++ {
		temp := 40 + i
		if temp > maxTemp {
			maxTemp = temp
		}
		devices[i] = monitor.GPUDevice{
			Index:       i,
			Name:        "Tesla P40",
			Temp:        temp,
			Utilization: i * 5,
			MemoryUsed:  1024,
			PowerDraw:   50 + i,
		}
	}
	return &controller.Status{GPU: &monitor.GPUReading{Devices: devices, Max: maxTemp}}
}

// gpuDiscTopicRe matches a per-GPU discovery config topic (gpu<digits>_<metric>),
// so it does NOT match the aggregate gpu_temp sensor.
var gpuDiscTopicRe = regexp.MustCompile(`/gpu\d+_(temp|utilization|power)/config$`)

// countGPUDiscovery counts per-GPU discovery configs published so far (the fake
// client keeps full history, so this is cumulative across ticks/reconnects).
func countGPUDiscovery(f *fakeClient) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, m := range f.published {
		if gpuDiscTopicRe.MatchString(m.topic) {
			n++
		}
	}
	return n
}

// TestGPUDiscoveryColdStart is the regression test for the bug found on the live
// R730: MQTT connects before the control loop's first GPU read, so no per-GPU
// discovery was ever published. It asserts the lazy publish path fixes that.
func TestGPUDiscoveryColdStart(t *testing.T) {
	// Start with NO GPU devices known (connect-before-first-read).
	consumer := &fakeConsumer{status: &controller.Status{}}
	h := &clientHolder{}
	b := New(testConfig(), consumer, h.factory)
	b.Start()

	// (1) At connect, zero per-GPU discovery is published (device set empty).
	if n := countGPUDiscovery(h.client); n != 0 {
		t.Fatalf("per-GPU discovery at connect = %d, want 0", n)
	}
	// A tick while the device set is still empty must not publish anything either.
	b.publishState()
	if n := countGPUDiscovery(h.client); n != 0 {
		t.Fatalf("per-GPU discovery after empty tick = %d, want 0", n)
	}

	// (2) The first GPU read completes: two devices appear. The next tick must
	// publish all three sensors for each of the two cards (6 total).
	consumer.setStatus(gpuStatus(2))
	b.publishState()
	if n := countGPUDiscovery(h.client); n != 6 {
		t.Fatalf("per-GPU discovery after devices appear = %d, want 6", n)
	}
	for i := 0; i < 2; i++ {
		for _, suffix := range []string{"temp", "utilization", "power"} {
			topic := fmt.Sprintf("homeassistant/sensor/only-fan-controller/gpu%d_%s/config", i, suffix)
			if _, ok := h.client.lastPublishOn(topic); !ok {
				t.Errorf("missing per-GPU discovery topic %q", topic)
			}
		}
	}

	// (3) Each card is announced exactly once per connection: further ticks do
	// not republish.
	b.publishState()
	b.publishState()
	if n := countGPUDiscovery(h.client); n != 6 {
		t.Fatalf("per-GPU discovery republished on later ticks = %d, want 6 (once per index)", n)
	}

	// (4) On reconnect the published set is cleared, so the next tick re-announces
	// every card (keeping HA's retained configs fresh after a broker restart).
	b.onConnect() // simulate paho's OnConnect firing again on reconnect
	b.publishState()
	if n := countGPUDiscovery(h.client); n != 12 {
		t.Fatalf("per-GPU discovery after reconnect = %d, want 12 (re-published)", n)
	}
}

// TestGPUDiscoveryScalesWithDeviceCount verifies the per-card sensor set scales
// with the detected device count (1, 2, and 3 cards) and that onConnect alone
// never publishes per-GPU discovery.
func TestGPUDiscoveryScalesWithDeviceCount(t *testing.T) {
	for _, n := range []int{1, 2, 3} {
		t.Run(fmt.Sprintf("%d_devices", n), func(t *testing.T) {
			consumer := &fakeConsumer{status: gpuStatus(n)}
			h := &clientHolder{}
			b := New(testConfig(), consumer, h.factory)
			b.Start()

			// Even with devices already available, onConnect must not publish them.
			if got := countGPUDiscovery(h.client); got != 0 {
				t.Fatalf("onConnect published %d per-GPU configs, want 0 (publish loop owns this)", got)
			}
			b.publishState()
			if got := countGPUDiscovery(h.client); got != n*3 {
				t.Fatalf("per-GPU discovery after tick = %d, want %d", got, n*3)
			}
		})
	}
}

// TestGPUDeviceSpecsClassesAndUnits verifies each per-card sensor carries the
// right device_class/unit and shares the common device block + availability.
func TestGPUDeviceSpecsClassesAndUnits(t *testing.T) {
	h := &clientHolder{}
	b := New(testConfig(), &fakeConsumer{}, h.factory)

	specs := b.gpuDeviceSpecs(0, monitor.GPUDevice{Index: 0, Name: "Tesla P40"})
	want := map[string]struct {
		deviceClass string
		unit        string
	}{
		"gpu0_temp":        {"temperature", "°C"},
		"gpu0_utilization": {"", "%"},
		"gpu0_power":       {"power", "W"},
	}
	seen := map[string]bool{}
	for _, s := range specs {
		exp, ok := want[s.objectID]
		if !ok {
			t.Fatalf("unexpected per-GPU objectID %q", s.objectID)
		}
		seen[s.objectID] = true
		if dc, _ := s.config["device_class"].(string); dc != exp.deviceClass {
			t.Errorf("%s device_class = %q, want %q", s.objectID, dc, exp.deviceClass)
		}
		if u, _ := s.config["unit_of_measurement"].(string); u != exp.unit {
			t.Errorf("%s unit = %q, want %q", s.objectID, u, exp.unit)
		}
		if at, _ := s.config["availability_topic"].(string); at != "only-fan-controller/availability" {
			t.Errorf("%s availability_topic = %q", s.objectID, at)
		}
		dev, ok := s.config["device"].(map[string]any)
		if !ok {
			t.Fatalf("%s missing device block", s.objectID)
		}
		if ids := dev["identifiers"].([]string); ids[0] != "only-fan-controller" {
			t.Errorf("%s device identifier = %v", s.objectID, ids[0])
		}
	}
	for id := range want {
		if !seen[id] {
			t.Errorf("per-GPU sensor %q not emitted", id)
		}
	}
}

// TestBuildStatePayloadGPUs verifies the gpus array marshals with the expected
// keys and per-card values, and that the aggregate gpu_temp (max) is preserved.
func TestBuildStatePayloadGPUs(t *testing.T) {
	p := buildStatePayload(gpuStatus(2))
	if p.GPUTemp == nil || *p.GPUTemp != 41 {
		t.Fatalf("gpu_temp (max) = %v, want 41", p.GPUTemp)
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m struct {
		GPUs []map[string]any `json:"gpus"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(m.GPUs) != 2 {
		t.Fatalf("gpus length = %d, want 2", len(m.GPUs))
	}
	for i, g := range m.GPUs {
		for _, k := range []string{"index", "name", "temp", "utilization", "power"} {
			if _, ok := g[k]; !ok {
				t.Errorf("gpus[%d] missing key %q", i, k)
			}
		}
		if int(g["index"].(float64)) != i {
			t.Errorf("gpus[%d].index = %v", i, g["index"])
		}
		if g["name"].(string) != "Tesla P40" {
			t.Errorf("gpus[%d].name = %v", i, g["name"])
		}
	}
}

// valueJSONKeyRe pulls "<index>" and "<key>" out of a value_template like
// "{{ value_json.gpus[0].temp | default('') }}".
var valueJSONKeyRe = regexp.MustCompile(`value_json\.gpus\[(\d+)\]\.(\w+)`)

// TestGPUDiscoveryTemplatesMatchStateKeys guards against drift between the
// per-GPU discovery value_templates and the state JSON: every key a template
// references must actually exist in the marshaled gpus array at that index.
func TestGPUDiscoveryTemplatesMatchStateKeys(t *testing.T) {
	status := gpuStatus(2)
	h := &clientHolder{}
	b := New(testConfig(), &fakeConsumer{status: status}, h.factory)

	// Marshal the state payload and index the gpus array as generic maps.
	stateJSON, err := json.Marshal(buildStatePayload(status))
	if err != nil {
		t.Fatalf("marshal state: %v", err)
	}
	var state struct {
		GPUs []map[string]any `json:"gpus"`
	}
	if err := json.Unmarshal(stateJSON, &state); err != nil {
		t.Fatalf("unmarshal state: %v", err)
	}

	checked := 0
	for i, d := range status.GPU.Devices {
		for _, s := range b.gpuDeviceSpecs(i, d) {
			tmpl, _ := s.config["value_template"].(string)
			match := valueJSONKeyRe.FindStringSubmatch(tmpl)
			if match == nil {
				t.Fatalf("per-GPU sensor %q has no value_json.gpus[..] template: %q", s.objectID, tmpl)
			}
			var arrIdx int
			fmt.Sscanf(match[1], "%d", &arrIdx)
			key := match[2]
			if arrIdx < 0 || arrIdx >= len(state.GPUs) {
				t.Errorf("template %q indexes gpus[%d] but state has %d entries", tmpl, arrIdx, len(state.GPUs))
				continue
			}
			if _, ok := state.GPUs[arrIdx][key]; !ok {
				t.Errorf("template %q references gpus[%d].%s which is absent from state JSON", tmpl, arrIdx, key)
			}
			checked++
		}
	}
	if checked != 6 { // 2 devices * 3 sensors
		t.Fatalf("checked %d per-GPU templates, want 6", checked)
	}
}
