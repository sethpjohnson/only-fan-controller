package mqtt

import (
	"encoding/json"
	"log"
	"time"

	"github.com/sethpjohnson/only-fan-controller/internal/controller"
	"github.com/sethpjohnson/only-fan-controller/internal/validate"
)

// overrideCommand is the JSON schema for <base_topic>/cmd/override.
type overrideCommand struct {
	Speed           int    `json:"speed"`
	DurationSeconds int    `json:"duration_seconds"`
	Reason          string `json:"reason"`
}

// hintCommand is the JSON schema for <base_topic>/cmd/hint, mirroring the HTTP
// POST /api/hint body.
type hintCommand struct {
	Type             string `json:"type"`
	Action           string `json:"action"`
	Intensity        string `json:"intensity"`
	Source           string `json:"source"`
	DurationEstimate int    `json:"duration_estimate"`
}

// subscribeCommands registers QoS-1 handlers for the three command topics. It
// runs from onConnect, so it re-subscribes on every (re)connection.
//
// Every handler drops RETAINED deliveries: commands are live imperatives, not
// state. A command ever published with the retain flag (a `mosquitto_pub -r`
// test, a misconfigured automation) would otherwise be replayed by the broker on
// every reconnect and process restart — silently re-executing forever. Dropping
// them here (once-per-topic warning) keeps that from happening while still
// surfacing the misconfiguration to an operator.
func (b *Bridge) subscribeCommands() {
	subs := []struct {
		topic string
		apply func(payload []byte)
	}{
		{b.cmdOverrideTopic(), b.handleOverrideCommand},
		{b.cmdOverrideClearTopic(), b.handleClearCommand},
		{b.cmdHintTopic(), b.handleHintCommand},
	}
	for _, s := range subs {
		apply := s.apply
		handler := func(topic string, payload []byte, retained bool) {
			if retained {
				b.warnRetainedCommand(topic)
				return
			}
			apply(payload)
		}
		if err := b.client.Subscribe(s.topic, 1, handler); err != nil {
			log.Printf("MQTT: failed to subscribe to %s: %v", s.topic, err)
		}
	}
}

// warnRetainedCommand logs a dropped retained command at most once per topic.
func (b *Bridge) warnRetainedCommand(topic string) {
	b.retainWarnMu.Lock()
	first := !b.retainWarned[topic]
	b.retainWarned[topic] = true
	b.retainWarnMu.Unlock()
	if first {
		log.Printf("MQTT: dropping RETAINED message on command topic %s; commands must be published WITHOUT the retain flag (a retained command would replay on every reconnect)", topic)
	}
}

// handleOverrideCommand parses, validates, and applies an override. Validation
// reuses the shared validate package; final clamping/cap live in the controller.
func (b *Bridge) handleOverrideCommand(payload []byte) {
	var cmd overrideCommand
	if err := json.Unmarshal(payload, &cmd); err != nil {
		log.Printf("MQTT: invalid override command: %v", err)
		return
	}
	if err := validate.OverrideSpeed(cmd.Speed); err != nil {
		log.Printf("MQTT: rejecting override command: %v", err)
		return
	}
	if err := validate.OverrideReason(cmd.Reason); err != nil {
		log.Printf("MQTT: rejecting override command: %v", err)
		return
	}
	duration := time.Duration(cmd.DurationSeconds) * time.Second
	b.consumer.SetOverride(cmd.Speed, duration, cmd.Reason)
	log.Printf("MQTT: override set via command: %d%% (%s)", cmd.Speed, cmd.Reason)
}

// handleClearCommand clears any active override. The payload is ignored (any
// message on the clear topic triggers it), mirroring an HA button press.
func (b *Bridge) handleClearCommand(_ []byte) {
	b.consumer.ClearOverride()
	log.Printf("MQTT: override cleared via command")
}

// handleHintCommand parses, validates, and applies a workload hint. action
// "stop" removes the hint by source; anything else starts it.
func (b *Bridge) handleHintCommand(payload []byte) {
	var cmd hintCommand
	if err := json.Unmarshal(payload, &cmd); err != nil {
		log.Printf("MQTT: invalid hint command: %v", err)
		return
	}
	if err := validate.HintField("source", cmd.Source); err != nil {
		log.Printf("MQTT: rejecting hint command: %v", err)
		return
	}
	if err := validate.HintField("type", cmd.Type); err != nil {
		log.Printf("MQTT: rejecting hint command: %v", err)
		return
	}
	if err := validate.HintAction(cmd.Action); err != nil {
		log.Printf("MQTT: rejecting hint command: %v", err)
		return
	}
	if err := validate.Intensity(cmd.Intensity); err != nil {
		log.Printf("MQTT: rejecting hint command: %v", err)
		return
	}
	if cmd.DurationEstimate < 0 {
		log.Printf("MQTT: rejecting hint command: duration_estimate must not be negative")
		return
	}

	if cmd.Action == "stop" {
		b.consumer.RemoveHint(cmd.Source)
		log.Printf("MQTT: hint removed via command: %s", cmd.Source)
		return
	}

	hint := &controller.WorkloadHint{
		Type:      cmd.Type,
		Action:    cmd.Action,
		Intensity: cmd.Intensity,
		Source:    cmd.Source,
	}
	if cmd.DurationEstimate > 0 {
		hint.ExpiresAt = time.Now().Add(time.Duration(cmd.DurationEstimate) * time.Second)
	}
	b.consumer.AddHint(hint)
	log.Printf("MQTT: hint registered via command: %s from %s", cmd.Action, cmd.Source)
}
