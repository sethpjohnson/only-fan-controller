// Package validate holds request-field validation shared by the HTTP API and
// the MQTT bridge, so both control surfaces enforce identical rules (charset,
// length, closed sets, control-character rejection) on operator-supplied input.
package validate

import (
	"fmt"
	"regexp"
	"unicode"
)

const (
	// MaxHintFieldLen bounds free-form hint identifiers (source/type). Small on
	// purpose: these are process names, not prose.
	MaxHintFieldLen = 64
	// MaxOverrideReasonLen bounds the free-text override reason.
	MaxOverrideReasonLen = 128
)

// hintFieldPattern is the allowed charset for hint source/type. Restricting to
// this set means no stored hint string can carry HTML/script even if a dashboard
// interpolation is ever missed.
var hintFieldPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// allowedIntensities is the closed set of intensity values the controller
// understands ("" means "unspecified").
var allowedIntensities = map[string]bool{"": true, "low": true, "medium": true, "high": true}

// allowedHintActions is the closed set of hint actions the controller acts on.
var allowedHintActions = map[string]bool{"start": true, "stop": true}

// HintField enforces length and charset on a hint source/type. name is used only
// for the error message.
func HintField(name, value string) error {
	if len(value) > MaxHintFieldLen {
		return fmt.Errorf("%s exceeds %d characters", name, MaxHintFieldLen)
	}
	if !hintFieldPattern.MatchString(value) {
		return fmt.Errorf("%s must match [A-Za-z0-9_.-]", name)
	}
	return nil
}

// HintAction enforces the closed set of hint actions.
func HintAction(action string) error {
	if !allowedHintActions[action] {
		return fmt.Errorf("action must be one of start, stop")
	}
	return nil
}

// Intensity enforces the closed set of hint intensities.
func Intensity(intensity string) error {
	if !allowedIntensities[intensity] {
		return fmt.Errorf("intensity must be one of low, medium, high")
	}
	return nil
}

// OverrideSpeed enforces the 0..100 percentage range. The controller still
// clamps to the configured min/max band; this only rejects nonsensical input.
func OverrideSpeed(speed int) error {
	if speed < 0 || speed > 100 {
		return fmt.Errorf("speed must be 0-100")
	}
	return nil
}

// OverrideReason enforces a length cap and rejects control characters on the
// human-readable override reason. Normal punctuation, spaces, and quotes are
// valid free text.
func OverrideReason(reason string) error {
	if len(reason) > MaxOverrideReasonLen {
		return fmt.Errorf("reason exceeds %d characters", MaxOverrideReasonLen)
	}
	for _, r := range reason {
		if unicode.IsControl(r) {
			return fmt.Errorf("reason must not contain control characters")
		}
	}
	return nil
}
