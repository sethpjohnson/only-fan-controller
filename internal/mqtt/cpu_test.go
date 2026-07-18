package mqtt

import (
	"encoding/json"
	"fmt"
	"regexp"
	"testing"

	"github.com/sethpjohnson/only-fan-controller/internal/controller"
	"github.com/sethpjohnson/only-fan-controller/internal/monitor"
)

// cpuStatus builds a controller status with n CPU sockets carrying distinct,
// recognizable per-socket temperatures.
func cpuStatus(n int) *controller.Status {
	temps := make([]int, n)
	maxTemp := 0
	for i := 0; i < n; i++ {
		temp := 50 + i
		if temp > maxTemp {
			maxTemp = temp
		}
		temps[i] = temp
	}
	return &controller.Status{CPU: &monitor.CPUReading{Temps: temps, Max: maxTemp}}
}

// cpuDiscTopicRe matches a per-CPU discovery config topic (cpu<digits>_temp), so
// it does NOT match the aggregate cpu_temp sensor.
var cpuDiscTopicRe = regexp.MustCompile(`/cpu\d+_temp/config$`)

// countCPUDiscovery counts per-CPU discovery configs published so far (the fake
// client keeps full history, so this is cumulative across ticks/reconnects).
func countCPUDiscovery(f *fakeClient) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, m := range f.published {
		if cpuDiscTopicRe.MatchString(m.topic) {
			n++
		}
	}
	return n
}

// TestCPUDiscoveryColdStart mirrors the per-GPU cold-start regression: MQTT
// connects before the control loop's first CPU read, so per-CPU discovery must be
// published lazily from the publish loop once sockets appear, exactly once per
// index per connection, and re-announced on reconnect.
func TestCPUDiscoveryColdStart(t *testing.T) {
	// Start with NO CPU reading known (connect-before-first-read).
	consumer := &fakeConsumer{status: &controller.Status{}}
	h := &clientHolder{}
	b := New(testConfig(), consumer, h.factory)
	b.Start()

	// (1) At connect, zero per-CPU discovery is published (no reading yet).
	if n := countCPUDiscovery(h.client); n != 0 {
		t.Fatalf("per-CPU discovery at connect = %d, want 0", n)
	}
	// A tick while the reading is still absent must not publish anything either.
	b.publishState()
	if n := countCPUDiscovery(h.client); n != 0 {
		t.Fatalf("per-CPU discovery after empty tick = %d, want 0", n)
	}

	// (2) The first CPU read completes: two sockets appear. The next tick must
	// publish one temperature sensor for each of the two sockets (2 total).
	consumer.setStatus(cpuStatus(2))
	b.publishState()
	if n := countCPUDiscovery(h.client); n != 2 {
		t.Fatalf("per-CPU discovery after sockets appear = %d, want 2", n)
	}
	for i := 0; i < 2; i++ {
		topic := fmt.Sprintf("homeassistant/sensor/only-fan-controller/cpu%d_temp/config", i)
		if _, ok := h.client.lastPublishOn(topic); !ok {
			t.Errorf("missing per-CPU discovery topic %q", topic)
		}
	}

	// (3) Each socket is announced exactly once per connection: further ticks do
	// not republish.
	b.publishState()
	b.publishState()
	if n := countCPUDiscovery(h.client); n != 2 {
		t.Fatalf("per-CPU discovery republished on later ticks = %d, want 2 (once per index)", n)
	}

	// (4) On reconnect the published set is cleared, so the next tick re-announces
	// every socket (keeping HA's retained configs fresh after a broker restart).
	b.onConnect() // simulate paho's OnConnect firing again on reconnect
	b.publishState()
	if n := countCPUDiscovery(h.client); n != 4 {
		t.Fatalf("per-CPU discovery after reconnect = %d, want 4 (re-published)", n)
	}
}

// TestCPUDiscoveryScalesWithSocketCount verifies the per-socket sensor set scales
// with the detected socket count (1, 2, and 4 sockets) and that onConnect alone
// never publishes per-CPU discovery.
func TestCPUDiscoveryScalesWithSocketCount(t *testing.T) {
	for _, n := range []int{1, 2, 4} {
		t.Run(fmt.Sprintf("%d_sockets", n), func(t *testing.T) {
			consumer := &fakeConsumer{status: cpuStatus(n)}
			h := &clientHolder{}
			b := New(testConfig(), consumer, h.factory)
			b.Start()

			// Even with a reading already available, onConnect must not publish it.
			if got := countCPUDiscovery(h.client); got != 0 {
				t.Fatalf("onConnect published %d per-CPU configs, want 0 (publish loop owns this)", got)
			}
			b.publishState()
			if got := countCPUDiscovery(h.client); got != n {
				t.Fatalf("per-CPU discovery after tick = %d, want %d", got, n)
			}
		})
	}
}

// TestCPUDeviceSpecsClassesAndUnits verifies the per-socket sensor carries the
// right object_id, 1-based label, device_class/unit, and shares the common device
// block + availability.
func TestCPUDeviceSpecsClassesAndUnits(t *testing.T) {
	h := &clientHolder{}
	b := New(testConfig(), &fakeConsumer{}, h.factory)

	specs := b.cpuDeviceSpecs(1)
	if len(specs) != 1 {
		t.Fatalf("cpuDeviceSpecs returned %d specs, want 1", len(specs))
	}
	s := specs[0]
	if s.objectID != "cpu1_temp" {
		t.Errorf("objectID = %q, want cpu1_temp", s.objectID)
	}
	if name, _ := s.config["name"].(string); name != "CPU 2 Temperature" {
		t.Errorf("name = %q, want %q", name, "CPU 2 Temperature")
	}
	if dc, _ := s.config["device_class"].(string); dc != "temperature" {
		t.Errorf("device_class = %q, want temperature", dc)
	}
	if u, _ := s.config["unit_of_measurement"].(string); u != "°C" {
		t.Errorf("unit = %q, want °C", u)
	}
	if tmpl, _ := s.config["value_template"].(string); tmpl != "{{ value_json.cpus[1].temp | default('') }}" {
		t.Errorf("value_template = %q, want %q", tmpl, "{{ value_json.cpus[1].temp | default('') }}")
	}
	if at, _ := s.config["availability_topic"].(string); at != "only-fan-controller/availability" {
		t.Errorf("availability_topic = %q", at)
	}
	dev, ok := s.config["device"].(map[string]any)
	if !ok {
		t.Fatalf("missing device block")
	}
	if ids := dev["identifiers"].([]string); ids[0] != "only-fan-controller" {
		t.Errorf("device identifier = %v", ids[0])
	}

	// Socket 0 uses object_id cpu0_temp and 1-based label "CPU 1 Temperature".
	zero := b.cpuDeviceSpecs(0)[0]
	if zero.objectID != "cpu0_temp" {
		t.Errorf("objectID = %q, want cpu0_temp", zero.objectID)
	}
	if name, _ := zero.config["name"].(string); name != "CPU 1 Temperature" {
		t.Errorf("name = %q, want %q", name, "CPU 1 Temperature")
	}
}

// TestBuildStatePayloadCPUs verifies the cpus array marshals with the expected
// keys, per-socket values, and order, and that the aggregate cpu_temp (max) is
// preserved.
func TestBuildStatePayloadCPUs(t *testing.T) {
	for _, n := range []int{2, 4} {
		t.Run(fmt.Sprintf("%d_sockets", n), func(t *testing.T) {
			status := cpuStatus(n)
			p := buildStatePayload(status)
			if p.CPUTemp == nil || *p.CPUTemp != status.CPU.Max {
				t.Fatalf("cpu_temp (max) = %v, want %d", p.CPUTemp, status.CPU.Max)
			}
			b, err := json.Marshal(p)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var m struct {
				CPUs []map[string]any `json:"cpus"`
			}
			if err := json.Unmarshal(b, &m); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if len(m.CPUs) != n {
				t.Fatalf("cpus length = %d, want %d", len(m.CPUs), n)
			}
			for i, c := range m.CPUs {
				for _, k := range []string{"index", "temp"} {
					if _, ok := c[k]; !ok {
						t.Errorf("cpus[%d] missing key %q", i, k)
					}
				}
				if int(c["index"].(float64)) != i {
					t.Errorf("cpus[%d].index = %v, want %d", i, c["index"], i)
				}
				if int(c["temp"].(float64)) != status.CPU.Temps[i] {
					t.Errorf("cpus[%d].temp = %v, want %d", i, c["temp"], status.CPU.Temps[i])
				}
			}
		})
	}
}

// TestBuildStatePayloadCPUsNilWhenNoReading verifies cpus is nil/empty when the
// controller reports no CPU reading.
func TestBuildStatePayloadCPUsNilWhenNoReading(t *testing.T) {
	p := buildStatePayload(&controller.Status{})
	if p.CPUs != nil {
		t.Fatalf("cpus = %v, want nil when status.CPU == nil", p.CPUs)
	}
}

// cpuValueJSONKeyRe pulls "<index>" and "<key>" out of a value_template like
// "{{ value_json.cpus[0].temp | default('') }}".
var cpuValueJSONKeyRe = regexp.MustCompile(`value_json\.cpus\[(\d+)\]\.(\w+)`)

// TestCPUDiscoveryTemplatesMatchStateKeys guards against drift between the
// per-CPU discovery value_templates and the state JSON: every key a template
// references must actually exist in the marshaled cpus array at that index.
func TestCPUDiscoveryTemplatesMatchStateKeys(t *testing.T) {
	status := cpuStatus(2)
	h := &clientHolder{}
	b := New(testConfig(), &fakeConsumer{status: status}, h.factory)

	// Marshal the state payload and index the cpus array as generic maps.
	stateJSON, err := json.Marshal(buildStatePayload(status))
	if err != nil {
		t.Fatalf("marshal state: %v", err)
	}
	var state struct {
		CPUs []map[string]any `json:"cpus"`
	}
	if err := json.Unmarshal(stateJSON, &state); err != nil {
		t.Fatalf("unmarshal state: %v", err)
	}

	checked := 0
	for i := range status.CPU.Temps {
		for _, s := range b.cpuDeviceSpecs(i) {
			tmpl, _ := s.config["value_template"].(string)
			match := cpuValueJSONKeyRe.FindStringSubmatch(tmpl)
			if match == nil {
				t.Fatalf("per-CPU sensor %q has no value_json.cpus[..] template: %q", s.objectID, tmpl)
			}
			var arrIdx int
			fmt.Sscanf(match[1], "%d", &arrIdx)
			key := match[2]
			if arrIdx < 0 || arrIdx >= len(state.CPUs) {
				t.Errorf("template %q indexes cpus[%d] but state has %d entries", tmpl, arrIdx, len(state.CPUs))
				continue
			}
			if _, ok := state.CPUs[arrIdx][key]; !ok {
				t.Errorf("template %q references cpus[%d].%s which is absent from state JSON", tmpl, arrIdx, key)
			}
			checked++
		}
	}
	if checked != 2 { // 2 sockets * 1 sensor
		t.Fatalf("checked %d per-CPU templates, want 2", checked)
	}
}
