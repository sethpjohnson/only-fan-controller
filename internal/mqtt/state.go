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
	}
	if status.GPU != nil {
		v := status.GPU.Max
		p.GPUTemp = &v
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
	payload, err := json.Marshal(buildStatePayload(b.consumer.GetStatus()))
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
