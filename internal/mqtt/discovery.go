package mqtt

import (
	"encoding/json"
	"fmt"
	"log"
)

// discoveryEntity is a single retained HA MQTT Discovery config message.
type discoveryEntity struct {
	topic   string
	payload []byte
}

// deviceBlock is the shared HA device all entities belong to. Its identifier is
// the MQTT client_id, so every entity is grouped under one "Only Fan Controller"
// device in Home Assistant.
func (b *Bridge) deviceBlock() map[string]any {
	return map[string]any{
		"identifiers":  []string{b.cfg.MQTT.ClientID},
		"name":         "Only Fan Controller",
		"manufacturer": "only-fan-controller",
		"model":        "IPMI Fan Controller",
	}
}

// discoveryTopic builds <prefix>/<component>/<client_id>/<object_id>/config.
func (b *Bridge) discoveryTopic(component, objectID string) string {
	return fmt.Sprintf("%s/%s/%s/%s/config", b.cfg.MQTT.DiscoveryPrefix, component, b.cfg.MQTT.ClientID, objectID)
}

// baseEntity returns the availability/device fields common to every entity.
func (b *Bridge) baseEntity(objectID, name string) map[string]any {
	return map[string]any{
		"name":                  name,
		"unique_id":             b.cfg.MQTT.ClientID + "_" + objectID,
		"object_id":             b.cfg.MQTT.ClientID + "_" + objectID,
		"availability_topic":    b.availabilityTopic(),
		"payload_available":     "online",
		"payload_not_available": "offline",
		"device":                b.deviceBlock(),
	}
}

// sensorConfig builds a read-only sensor bound to a value_json key.
func (b *Bridge) sensorConfig(objectID, name, valueKey, deviceClass, unit, icon string) map[string]any {
	e := b.baseEntity(objectID, name)
	e["state_topic"] = b.stateTopic()
	e["value_template"] = "{{ value_json." + valueKey + " }}"
	if deviceClass != "" {
		e["device_class"] = deviceClass
	}
	if unit != "" {
		e["unit_of_measurement"] = unit
	}
	if icon != "" {
		e["icon"] = icon
	}
	return e
}

// binarySensorConfig builds an ON/OFF binary sensor from a boolean value_json
// key, using device_class "problem" (HA shows red when true).
func (b *Bridge) binarySensorConfig(objectID, name, valueKey string) map[string]any {
	e := b.baseEntity(objectID, name)
	e["state_topic"] = b.stateTopic()
	e["value_template"] = "{{ 'ON' if value_json." + valueKey + " else 'OFF' }}"
	e["payload_on"] = "ON"
	e["payload_off"] = "OFF"
	e["device_class"] = "problem"
	return e
}

// numberConfig builds the override-fan-speed number. min/max are bound to the
// configured MinSpeed/MaxSpeed so the HA UI cannot request an out-of-range value
// (the controller still clamps regardless). The command wraps the raw value into
// the cmd/override JSON with a default 1h duration.
func (b *Bridge) numberConfig() map[string]any {
	e := b.baseEntity("override_speed", "Override Fan Speed")
	e["command_topic"] = b.cmdOverrideTopic()
	e["command_template"] = `{"speed": {{ value }}, "duration_seconds": 3600, "reason": "home assistant"}`
	e["state_topic"] = b.stateTopic()
	e["value_template"] = "{{ value_json.override_speed | default(0) }}"
	e["min"] = b.cfg.FanControl.MinSpeed
	e["max"] = b.cfg.FanControl.MaxSpeed
	e["step"] = 1
	e["unit_of_measurement"] = "%"
	e["mode"] = "slider"
	e["icon"] = "mdi:fan"
	return e
}

// buttonConfig builds the clear-override button.
func (b *Bridge) buttonConfig() map[string]any {
	e := b.baseEntity("override_clear", "Clear Fan Override")
	e["command_topic"] = b.cmdOverrideClearTopic()
	e["payload_press"] = "PRESS"
	e["icon"] = "mdi:fan-off"
	return e
}

// discoveryEntities returns every HA discovery config message. Marshal failures
// (not expected for these primitive maps) are logged and skipped.
func (b *Bridge) discoveryEntities() []discoveryEntity {
	type spec struct {
		component string
		objectID  string
		config    map[string]any
	}
	specs := []spec{
		{"sensor", "cpu_temp", b.sensorConfig("cpu_temp", "CPU Temperature", "cpu_temp", "temperature", "°C", "")},
		{"sensor", "gpu_temp", b.sensorConfig("gpu_temp", "GPU Temperature", "gpu_temp", "temperature", "°C", "")},
		{"sensor", "fan_speed", b.sensorConfig("fan_speed", "Fan Speed", "fan_speed", "", "%", "mdi:fan")},
		{"sensor", "target_speed", b.sensorConfig("target_speed", "Target Fan Speed", "target_speed", "", "%", "mdi:fan")},
		{"sensor", "zone", b.sensorConfig("zone", "Thermal Zone", "zone", "", "", "mdi:thermometer")},
		{"sensor", "failsafe_reason", b.sensorConfig("failsafe_reason", "Failsafe Reason", "failsafe_reason", "", "", "mdi:shield-alert")},
		{"binary_sensor", "failsafe_active", b.binarySensorConfig("failsafe_active", "Failsafe Active", "failsafe_active")},
		{"binary_sensor", "restore_pending", b.binarySensorConfig("restore_pending", "Restore Pending", "restore_pending")},
		{"binary_sensor", "last_write_failed", b.binarySensorConfig("last_write_failed", "Last Fan Write Failed", "last_write_failed")},
		{"number", "override_speed", b.numberConfig()},
		{"button", "override_clear", b.buttonConfig()},
	}
	entities := make([]discoveryEntity, 0, len(specs))
	for _, s := range specs {
		payload, err := json.Marshal(s.config)
		if err != nil {
			log.Printf("MQTT: failed to marshal discovery for %s/%s: %v", s.component, s.objectID, err)
			continue
		}
		entities = append(entities, discoveryEntity{
			topic:   b.discoveryTopic(s.component, s.objectID),
			payload: payload,
		})
	}
	return entities
}

// publishDiscovery publishes all discovery config messages, retained.
func (b *Bridge) publishDiscovery() {
	for _, e := range b.discoveryEntities() {
		if err := b.client.Publish(e.topic, 1, true, e.payload); err != nil {
			log.Printf("MQTT: failed to publish discovery %s: %v", e.topic, err)
		}
	}
}
