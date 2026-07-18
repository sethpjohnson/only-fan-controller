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
	// CPUs carries one entry per CPU socket, so Home Assistant can show per-socket
	// temperature on a multi-socket box. CPUTemp above stays the overall max (fan
	// logic and the aggregate sensor depend on it). IPMI reports only per-socket
	// temperature — no utilization/power. The per-socket discovery entities index
	// into this array by position (cpus[0], cpus[1], ...).
	CPUs []cpuState `json:"cpus"`
	// GPUs carries one entry per detected GPU device, so Home Assistant can show
	// per-card temperature/utilization/power. GPUTemp above stays the overall max
	// (fan logic and the aggregate sensor depend on it). The per-card discovery
	// entities index into this array by position (gpus[0], gpus[1], ...).
	GPUs []gpuState `json:"gpus"`
}

// cpuState is one per-socket entry in statePayload.CPUs. Keys here MUST stay in
// sync with the value_templates emitted by the per-CPU discovery sensors (see
// discovery.go); the key-correspondence test guards against drift.
type cpuState struct {
	Index int `json:"index"`
	Temp  int `json:"temp"`
}

// gpuState is one per-card entry in statePayload.GPUs. Keys here MUST stay in
// sync with the value_templates emitted by the per-GPU discovery sensors (see
// discovery.go); the key-correspondence test guards against drift.
type gpuState struct {
	Index       int    `json:"index"`
	Name        string `json:"name"`
	Temp        int    `json:"temp"`
	Utilization int    `json:"utilization"`
	Power       int    `json:"power"`
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
		for i, t := range status.CPU.Temps {
			p.CPUs = append(p.CPUs, cpuState{Index: i, Temp: t})
		}
	}
	if status.GPU != nil {
		v := status.GPU.Max
		p.GPUTemp = &v
		for _, d := range status.GPU.Devices {
			p.GPUs = append(p.GPUs, gpuState{
				Index:       d.Index,
				Name:        d.Name,
				Temp:        d.Temp,
				Utilization: d.Utilization,
				Power:       d.PowerDraw,
			})
		}
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
	status := b.consumer.GetStatus()
	payload, err := json.Marshal(buildStatePayload(status))
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
		// Surface a targeted hint the first time publishing fails (rearmed after
		// the next success). The rate-limited error above says *that* publishing
		// failed; this says the most likely *why* — a wrong broker host/port — the
		// exact pain the 8123-instead-of-1883 mistake causes.
		if b.unreachableHint.arm() {
			log.Printf("MQTT: still unable to reach broker %s — is the host/port correct? Home Assistant's MQTT broker is usually port 1883, not the 8123 web UI", b.broker)
		}
		return
	}
	b.unreachableHint.rearm()
	// The state publish just succeeded, so the broker is reachable. Announce
	// discovery for any GPU that has appeared since the last connect. Doing this
	// only after a good state publish (rather than in onConnect) is what makes
	// per-GPU entities show up: at cold start MQTT connects before the first GPU
	// read, so the device set is empty at connect and only populated by later
	// ticks. Gating on the successful publish also avoids retrying/logging
	// discovery while the broker is down.
	b.publishNewGPUDiscovery(status)
	b.publishNewCPUDiscovery(status)
}

// publishNewCPUDiscovery publishes retained discovery configs for any CPU socket
// in status whose index has not yet been announced on the current connection,
// then records it so it is not republished every tick. The socket count is fixed
// at boot, so this settles on the first tick — but the lazy, gated approach is
// still required for the same cold-start reason GPUs use: MQTT connects before
// the control loop's first CPU read, so the reading is empty at connect and only
// populated by later ticks. cpuDiscovered is mutex-guarded because onConnect
// clears it from paho's goroutine while this runs on the publish-loop goroutine.
// A socket is marked announced only if its config published cleanly, so a
// transient failure is retried on a later tick.
func (b *Bridge) publishNewCPUDiscovery(status *controller.Status) {
	if status == nil || status.CPU == nil {
		return
	}
	for i := range status.CPU.Temps {
		b.cpuDiscMu.Lock()
		already := b.cpuDiscovered[i]
		b.cpuDiscMu.Unlock()
		if already {
			continue
		}
		published := true
		for _, s := range b.cpuDeviceSpecs(i) {
			payload, err := json.Marshal(s.config)
			if err != nil {
				log.Printf("MQTT: failed to marshal CPU discovery for %s/%s: %v", s.component, s.objectID, err)
				published = false
				continue
			}
			topic := b.discoveryTopic(s.component, s.objectID)
			if err := b.client.Publish(topic, 1, true, payload); err != nil {
				log.Printf("MQTT: failed to publish CPU discovery %s: %v", topic, err)
				published = false
			}
		}
		if published {
			b.cpuDiscMu.Lock()
			b.cpuDiscovered[i] = true
			b.cpuDiscMu.Unlock()
		}
	}
}

// publishNewGPUDiscovery publishes retained discovery configs for any GPU device
// in status whose index has not yet been announced on the current connection,
// then records it so it is not republished every tick. It handles a GPU that
// appears seconds after startup (the cold-start case) or is hot-added later.
// gpuDiscovered is mutex-guarded because onConnect clears it from paho's
// goroutine while this runs on the publish-loop goroutine. A device is marked
// announced only if all of its configs published cleanly, so a transient failure
// is retried on a later tick.
func (b *Bridge) publishNewGPUDiscovery(status *controller.Status) {
	if status == nil || status.GPU == nil {
		return
	}
	for i, d := range status.GPU.Devices {
		b.gpuDiscMu.Lock()
		already := b.gpuDiscovered[d.Index]
		b.gpuDiscMu.Unlock()
		if already {
			continue
		}
		published := true
		for _, s := range b.gpuDeviceSpecs(i, d) {
			payload, err := json.Marshal(s.config)
			if err != nil {
				log.Printf("MQTT: failed to marshal GPU discovery for %s/%s: %v", s.component, s.objectID, err)
				published = false
				continue
			}
			topic := b.discoveryTopic(s.component, s.objectID)
			if err := b.client.Publish(topic, 1, true, payload); err != nil {
				log.Printf("MQTT: failed to publish GPU discovery %s: %v", topic, err)
				published = false
			}
		}
		if published {
			b.gpuDiscMu.Lock()
			b.gpuDiscovered[d.Index] = true
			b.gpuDiscMu.Unlock()
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
