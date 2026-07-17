package controller

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os/exec"
	"sync"
	"time"

	"github.com/sethpjohnson/only-fan-controller/internal/config"
	"github.com/sethpjohnson/only-fan-controller/internal/monitor"
	"github.com/sethpjohnson/only-fan-controller/internal/storage"
)

// cpuReader and gpuReader abstract the temperature sources so the control loop
// can be exercised with fakes in tests (the real implementations are the
// ipmitool/nvidia-smi backed monitors).
type cpuReader interface {
	Read() (*monitor.CPUReading, error)
}

type gpuReader interface {
	Read() (*monitor.GPUReading, error)
}

// runCommandFunc executes an external command with a context deadline. It is a
// field on FanController so tests can substitute a stub in place of real
// ipmitool invocations.
type runCommandFunc func(ctx context.Context, name string, args ...string) error

// failsafeCause records why the controller handed cooling back to the BMC.
type failsafeCause int

const (
	failsafeNone   failsafeCause = iota
	failsafeSensor               // sensor loss — recoverable
	failsafeWrite                // fan-write failures — sticky until restart
)

func (c failsafeCause) String() string {
	switch c {
	case failsafeSensor:
		return "sensor-loss"
	case failsafeWrite:
		return "write-failure"
	default:
		return "none"
	}
}

type FanController struct {
	cfg    *config.Config
	cpuMon cpuReader
	gpuMon gpuReader
	store  *storage.Store

	// runCommand runs external commands (ipmitool). Defaults to realRunCommand.
	runCommand runCommandFunc

	// State
	mu             sync.RWMutex
	currentSpeed   int
	targetSpeed    int
	currentZone    string
	lastCPUReading *monitor.CPUReading
	lastGPUReading *monitor.GPUReading
	hints          map[string]*WorkloadHint
	override       *Override
	running        bool
	stopChan       chan struct{}

	// Fail-safe state. failsafeCause and lastWriteFailed are read by GetStatus
	// (API goroutine) so they are guarded by mu. The consecutive-failure counters
	// are only ever touched from the control loop goroutine.
	//
	// The cause is tracked (not a bare bool) so recovery is coherent per domain:
	//   - a SENSOR fail-safe is recoverable — a healthy sensor read reclaims
	//     manual control;
	//   - a WRITE fail-safe is STICKY — once the fan-write channel proves
	//     unreliable we leave the BMC in charge (no auto-recovery), because
	//     probing it every tick would flap the BMC between manual and auto.
	failsafeCause    failsafeCause
	restoreConfirmed bool // true once RestoreAutoMode has actually succeeded for the current fail-safe
	lastWriteFailed  bool
	sensorFailCount  int
	writeFailCount   int

	// History for trend analysis
	cpuHistory []tempPoint
	gpuHistory []tempPoint

	// Hysteresis tracking
	lastOverThreshold time.Time
}

type tempPoint struct {
	temp      int
	timestamp time.Time
}

type WorkloadHint struct {
	Type        string    `json:"type"`
	Action      string    `json:"action"`
	Intensity   string    `json:"intensity"`
	Source      string    `json:"source"`
	MinFanSpeed int       `json:"min_fan_speed"`
	ExpiresAt   time.Time `json:"expires_at"`
	CreatedAt   time.Time `json:"created_at"`
}

type Override struct {
	Speed     int       `json:"speed"`
	Reason    string    `json:"reason"`
	ExpiresAt time.Time `json:"expires_at"`
	CreatedAt time.Time `json:"created_at"`
}

type Status struct {
	Timestamp    time.Time           `json:"timestamp"`
	CPU          *monitor.CPUReading `json:"cpu"`
	GPU          *monitor.GPUReading `json:"gpu"`
	CurrentSpeed int                 `json:"current_speed"`
	TargetSpeed  int                 `json:"target_speed"`
	Zone         string              `json:"zone"`
	Mode         string              `json:"mode"`
	ActiveHints  []*WorkloadHint     `json:"active_hints"`
	Override     *Override           `json:"override,omitempty"`
	CPUTrend     float64             `json:"cpu_trend"`
	GPUTrend     float64             `json:"gpu_trend"`
	Zones        []config.Zone       `json:"zones"`
	CPUThreshold int                 `json:"cpu_threshold"`
	GPUThreshold int                 `json:"gpu_threshold"`
	IdleSpeed    int                 `json:"idle_speed"`
	// Fail-safe visibility for operators / the dashboard.
	FailsafeActive  bool   `json:"failsafe_active"`   // true when cooling has been handed back to BMC auto mode
	FailsafeReason  string `json:"failsafe_reason"`   // "none", "sensor-loss" or "write-failure"
	RestorePending  bool   `json:"restore_pending"`   // true when in fail-safe but RestoreAutoMode has not yet succeeded (BMC may still be in manual mode)
	LastWriteFailed bool   `json:"last_write_failed"` // true when the most recent fan-speed write failed
}

func NewFanController(cfg *config.Config, cpuMon cpuReader, gpuMon gpuReader, store *storage.Store) *FanController {
	return &FanController{
		cfg:        cfg,
		cpuMon:     cpuMon,
		gpuMon:     gpuMon,
		store:      store,
		runCommand: realRunCommand,
		hints:      make(map[string]*WorkloadHint),
		stopChan:   make(chan struct{}),
		cpuHistory: make([]tempPoint, 0),
		gpuHistory: make([]tempPoint, 0),
	}
}

// realRunCommand runs an external command bounded by the context deadline. On a
// deadline it returns a descriptive error so the caller can distinguish a hung
// command (e.g. a stuck `ipmitool -I lanplus`) from a normal failure.
func realRunCommand(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("%s timed out: %w", name, err)
		}
		return fmt.Errorf("%s error: %v, stderr: %s", name, err, stderr.String())
	}
	return nil
}

func (fc *FanController) Run() {
	fc.mu.Lock()
	fc.running = true
	fc.mu.Unlock()

	// Enable manual fan control mode
	if err := fc.enableManualMode(); err != nil {
		log.Printf("Warning: Failed to enable manual fan mode: %v", err)
	}

	ticker := time.NewTicker(time.Duration(fc.cfg.Monitoring.Interval) * time.Second)
	defer ticker.Stop()

	// Run immediately on start
	fc.controlLoop()

	for {
		select {
		case <-ticker.C:
			fc.controlLoop()
		case <-fc.stopChan:
			return
		}
	}
}

func (fc *FanController) Stop() {
	fc.mu.Lock()
	fc.running = false
	fc.mu.Unlock()
	close(fc.stopChan)
}

func (fc *FanController) controlLoop() {
	// If we are in fail-safe but the hand-back to BMC auto has not been confirmed
	// (a previous RestoreAutoMode failed — e.g. the BMC was unreachable, which is
	// exactly the condition that trips fail-safe), keep retrying every tick until
	// it succeeds. This does not reintroduce flapping: restore-to-auto is
	// idempotent and nothing re-enables manual mode while a restore is unconfirmed.
	fc.ensureAutoRestored()

	// A WRITE fail-safe is sticky: once the fan-write channel has proven
	// unreliable we leave the BMC in charge until the process restarts. Probing
	// it every tick (reclaim manual -> write fails -> restore auto) would flap the
	// BMC forever, so we deliberately do not auto-recover. We still refresh the
	// readings for /api/status visibility.
	if fc.currentFailsafeCause() == failsafeWrite {
		if cpuReading, gpuReading, ok := fc.readSensors(); ok {
			fc.recordReadings(cpuReading, gpuReading)
		}
		return
	}

	// Read temperatures. A read error must NEVER be treated as 0°C ("cold"),
	// which would ramp the fans down and cook the machine. Instead we hold the
	// current speed and, after repeated failures, hand cooling back to the BMC.
	cpuReading, gpuReading, ok := fc.readSensors()
	if !ok {
		fc.handleSensorFailure()
		return
	}

	// Sensors are healthy again: clear the failure count.
	if fc.sensorFailCount > 0 {
		log.Printf("Sensors recovered after %d consecutive failure(s)", fc.sensorFailCount)
		fc.sensorFailCount = 0
	}

	// If we were in a (recoverable) sensor fail-safe, reclaim manual control
	// before acting on the reading — but only from a CONFIRMED-restored state, so
	// the manual<->auto handoff stays coherent. If the hand-back to BMC auto is
	// still unconfirmed, ensureAutoRestored (above) keeps retrying and we wait.
	// If reclaiming the write channel fails we stay in fail-safe and retry next
	// tick; a failed enableManualMode does not change the BMC state, so it cannot
	// flap.
	if fc.currentFailsafeCause() == failsafeSensor {
		if !fc.isRestoreConfirmed() {
			log.Printf("Sensors recovered but BMC auto hand-back not yet confirmed; deferring manual reclaim")
			return
		}
		if err := fc.enableManualMode(); err != nil {
			log.Printf("Sensors recovered but reclaiming manual mode failed; staying in BMC auto: %v", err)
			return
		}
		log.Printf("FAILSAFE CLEARED: sensors recovered, manual fan control reclaimed")
		fc.clearFailsafe()
	}

	fc.recordReadings(cpuReading, gpuReading)

	// Calculate target fan speed
	target := fc.calculateTarget(cpuReading, gpuReading)

	// Apply fan speed. A failed write is a watchdog concern: after repeated
	// failures we restore BMC auto mode rather than leave the fans wherever
	// they were last set.
	if err := fc.setFanSpeed(target); err != nil {
		fc.handleWriteFailure(err)
	} else {
		fc.writeFailCount = 0
	}

	// Store reading
	fc.store.RecordReading(cpuReading.Max, gpuReading.Max, target)

	fc.mu.RLock()
	zone := fc.currentZone
	fc.mu.RUnlock()
	log.Printf("CPU: %d°C | GPU: %d°C | Zone: %s | Fan: %d%%",
		cpuReading.Max, gpuReading.Max, zone, target)
}

// readSensors reads both temperature sources. It returns ok=false if either read
// failed, logging each failure. It never fabricates a 0°C reading on failure.
func (fc *FanController) readSensors() (*monitor.CPUReading, *monitor.GPUReading, bool) {
	cpuReading, cpuErr := fc.cpuMon.Read()
	gpuReading, gpuErr := fc.gpuMon.Read()
	if cpuErr != nil {
		log.Printf("CPU sensor read failed: %v", cpuErr)
	}
	if gpuErr != nil {
		log.Printf("GPU sensor read failed: %v", gpuErr)
	}
	if cpuErr != nil || gpuErr != nil {
		return nil, nil, false
	}
	return cpuReading, gpuReading, true
}

// recordReadings updates the last-known readings and history under the lock.
func (fc *FanController) recordReadings(cpuReading *monitor.CPUReading, gpuReading *monitor.GPUReading) {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	fc.lastCPUReading = cpuReading
	fc.lastGPUReading = gpuReading
	now := time.Now()
	fc.cpuHistory = append(fc.cpuHistory, tempPoint{temp: cpuReading.Max, timestamp: now})
	fc.gpuHistory = append(fc.gpuHistory, tempPoint{temp: gpuReading.Max, timestamp: now})
	fc.trimHistory()
	fc.cleanExpired()
}

// handleSensorFailure records a consecutive sensor read failure. It holds the
// current fan speed (does not touch the fans) and, once the configured limit is
// reached, hands cooling back to the BMC's automatic control.
func (fc *FanController) handleSensorFailure() {
	fc.sensorFailCount++
	limit := fc.sensorFailureLimit()
	log.Printf("SENSOR FAILURE %d/%d - holding fan speed (not treating missing data as 0°C)",
		fc.sensorFailCount, limit)
	if fc.sensorFailCount >= limit {
		fc.enterFailsafe(failsafeSensor)
	}
}

// handleWriteFailure records a consecutive fan-write failure and restores auto
// mode once the configured limit is reached.
func (fc *FanController) handleWriteFailure(err error) {
	fc.writeFailCount++
	limit := fc.writeFailureLimit()
	log.Printf("Fan write failure %d/%d: %v", fc.writeFailCount, limit, err)
	if fc.writeFailCount >= limit {
		fc.enterFailsafe(failsafeWrite)
	}
}

func (fc *FanController) currentFailsafeCause() failsafeCause {
	fc.mu.RLock()
	defer fc.mu.RUnlock()
	return fc.failsafeCause
}

func (fc *FanController) isRestoreConfirmed() bool {
	fc.mu.RLock()
	defer fc.mu.RUnlock()
	return fc.restoreConfirmed
}

// enterFailsafe hands cooling back to the BMC's automatic fan control, recording
// WHY. It only attempts RestoreAutoMode on the transition from active control
// into fail-safe; a write fail-safe never downgrades to sensor, so once sticky
// it stays sticky. If the restore attempt fails, restoreConfirmed stays false
// and ensureAutoRestored retries on subsequent ticks.
func (fc *FanController) enterFailsafe(cause failsafeCause) {
	fc.mu.Lock()
	prev := fc.failsafeCause
	// Write is the stickiest cause; never let a later sensor failure overwrite it.
	if prev == failsafeWrite {
		fc.mu.Unlock()
		return
	}
	fc.failsafeCause = cause
	fc.mu.Unlock()

	if prev != failsafeNone {
		// Already handed to BMC auto for some reason; ensureAutoRestored owns the
		// (possibly still-pending) restore retry.
		return
	}
	log.Printf("FAILSAFE ACTIVATED (%s): restoring BMC automatic fan control", cause)
	fc.attemptRestore()
}

// ensureAutoRestored retries the hand-back to BMC auto mode while we are in
// fail-safe but the restore has not yet been confirmed. Restore-to-auto is
// idempotent and nothing re-enables manual mode while unconfirmed, so this
// cannot flap.
func (fc *FanController) ensureAutoRestored() {
	fc.mu.RLock()
	pending := fc.failsafeCause != failsafeNone && !fc.restoreConfirmed
	fc.mu.RUnlock()
	if !pending {
		return
	}
	log.Printf("Fail-safe: BMC auto hand-back not confirmed, retrying RestoreAutoMode")
	fc.attemptRestore()
}

// attemptRestore calls RestoreAutoMode and records whether it succeeded.
func (fc *FanController) attemptRestore() {
	if err := fc.RestoreAutoMode(); err != nil {
		log.Printf("CRITICAL: RestoreAutoMode failed; BMC hand-back NOT confirmed, will retry: %v", err)
		return
	}
	fc.mu.Lock()
	fc.restoreConfirmed = true
	fc.mu.Unlock()
	log.Printf("Fail-safe: BMC automatic fan control confirmed restored")
}

// clearFailsafe returns to normal control and resets the fail-safe state so the
// next failure starts fresh.
func (fc *FanController) clearFailsafe() {
	fc.mu.Lock()
	fc.failsafeCause = failsafeNone
	fc.restoreConfirmed = false
	// sensorFailCount / writeFailCount are only ever touched from the single
	// control-loop goroutine (here and in handleSensor/WriteFailure), so they
	// need no lock; grouped here for a coherent reset of all fail-safe state.
	fc.sensorFailCount = 0
	fc.writeFailCount = 0
	fc.mu.Unlock()
}

func (fc *FanController) sensorFailureLimit() int {
	if fc.cfg.FanControl.SensorFailureLimit > 0 {
		return fc.cfg.FanControl.SensorFailureLimit
	}
	return 3
}

func (fc *FanController) writeFailureLimit() int {
	if fc.cfg.FanControl.WriteFailureLimit > 0 {
		return fc.cfg.FanControl.WriteFailureLimit
	}
	return 3
}

// commandTimeout bounds every ipmitool invocation so a hung BMC connection can
// never freeze the control loop (or shutdown), while keeping a floor so that a
// short control interval does not make slow-but-healthy lanplus calls read as
// timeouts (which would feed the fail-safe counters).
func (fc *FanController) commandTimeout() time.Duration {
	return commandTimeout(fc.cfg.Monitoring.Interval)
}

// commandTimeout computes the external-command deadline: max(3s, min(10s,
// interval)). The 3s floor protects slow-but-healthy remote BMC calls.
func commandTimeout(intervalSeconds int) time.Duration {
	const (
		minTimeout = 3 * time.Second
		maxTimeout = 10 * time.Second
	)
	interval := time.Duration(intervalSeconds) * time.Second
	if interval > maxTimeout {
		return maxTimeout
	}
	if interval < minTimeout {
		return minTimeout
	}
	return interval
}

// ipmitool runs an ipmitool raw command against the configured BMC (local or
// remote) with a deadline.
func (fc *FanController) ipmitool(rawArgs ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), fc.commandTimeout())
	defer cancel()

	var args []string
	if fc.cfg.IDRAC.Host != "local" {
		args = append(args,
			"-I", "lanplus",
			"-H", fc.cfg.IDRAC.Host,
			"-U", fc.cfg.IDRAC.Username,
			"-P", fc.cfg.IDRAC.Password,
		)
	}
	args = append(args, rawArgs...)
	return fc.runCommand(ctx, "ipmitool", args...)
}

func (fc *FanController) calculateTarget(cpuReading *monitor.CPUReading, gpuReading *monitor.GPUReading) int {
	fc.mu.Lock()
	defer fc.mu.Unlock()

	cpuMax := cpuReading.Max
	gpuMax := gpuReading.Max

	// Safety first: a critical temperature bypasses step ramping AND any manual
	// override, driving fans straight to MaxSpeed (effectively 100%). The
	// emergency speed is a FIXED CEILING (MaxSpeed), never operator-editable zone
	// data — that way a bad zones list can never turn the "emergency" into a low
	// speed. Leaving fans pinned low during a thermal emergency is exactly what
	// this controller must never do, so critical cooling wins over everything.
	if fc.cfg.IsCritical(cpuMax, gpuMax) {
		speed := fc.cfg.FanControl.MaxSpeed
		if speed <= 0 || speed > 100 {
			speed = 100
		}
		fc.currentZone = "critical"
		fc.targetSpeed = speed
		return speed
	}

	// Check for manual override
	if fc.override != nil {
		fc.targetSpeed = fc.override.Speed
		return fc.override.Speed
	}

	// Simple threshold-based control with hysteresis
	// 1. Start with idle speed
	// 2. If CPU or GPU exceeds threshold, increase fan speed
	// 3. Only decrease after cooldown period below threshold

	baseSpeed := fc.cfg.FanControl.IdleSpeed
	if baseSpeed == 0 {
		baseSpeed = 20 // Default idle speed
	}

	// Apply workload hints as minimum floor
	hintMinSpeed := 0
	for _, hint := range fc.hints {
		if hint.MinFanSpeed > hintMinSpeed {
			hintMinSpeed = hint.MinFanSpeed
		}
	}

	// Check if we're over thresholds
	cpuThreshold := fc.cfg.FanControl.EffectiveCPUThreshold()
	gpuThreshold := fc.cfg.FanControl.EffectiveGPUThreshold()

	overThreshold := cpuMax > cpuThreshold || gpuMax > gpuThreshold

	// Determine zone name for display
	if cpuMax > 80 || gpuMax > 85 {
		fc.currentZone = "hot"
	} else if cpuMax > 70 || gpuMax > 75 {
		fc.currentZone = "warm"
	} else if overThreshold {
		fc.currentZone = "active"
	} else {
		fc.currentZone = "idle"
	}

	stepSize := fc.cfg.FanControl.StepSize
	if stepSize == 0 {
		stepSize = 10
	}

	now := time.Now()
	target := fc.currentSpeed

	if overThreshold {
		// Over threshold - increase fan speed and reset cooldown timer
		fc.lastOverThreshold = now

		// Calculate how much over threshold we are
		cpuOver := max(0, cpuMax-cpuThreshold)
		gpuOver := max(0, gpuMax-gpuThreshold)
		maxOver := max(cpuOver, gpuOver)

		// Each 5°C over threshold = one step increase
		stepsNeeded := (maxOver / 5) + 1
		neededSpeed := baseSpeed + (stepsNeeded * stepSize)

		// Ramp up immediately if needed
		if neededSpeed > target {
			target = min(target+stepSize, neededSpeed)
		}
	} else {
		// Below threshold
		cooldownDuration := time.Duration(fc.cfg.FanControl.CooldownDelay) * time.Second
		if cooldownDuration == 0 {
			cooldownDuration = 60 * time.Second
		}

		if fc.lastOverThreshold.IsZero() {
			// Never been over threshold - go directly to idle speed
			target = baseSpeed
		} else if now.Sub(fc.lastOverThreshold) > cooldownDuration {
			// Cooldown complete, can ramp down toward idle
			if target > baseSpeed {
				target = target - stepSize
				if target < baseSpeed {
					target = baseSpeed
				}
			} else {
				target = baseSpeed
			}
		}
		// Otherwise hold current speed during cooldown (target = currentSpeed, unchanged)
	}

	// Apply hint minimum (workload hint sets a floor)
	if hintMinSpeed > target {
		target = hintMinSpeed
	}

	// Clamp to configured limits
	target = max(fc.cfg.FanControl.MinSpeed, min(fc.cfg.FanControl.MaxSpeed, target))

	fc.targetSpeed = target
	return target
}

func (fc *FanController) calculateTrend(history []tempPoint) float64 {
	if len(history) < 2 {
		return 0
	}

	// Look at last 60 seconds
	cutoff := time.Now().Add(-60 * time.Second)
	var recent []tempPoint
	for _, p := range history {
		if p.timestamp.After(cutoff) {
			recent = append(recent, p)
		}
	}

	if len(recent) < 2 {
		return 0
	}

	// Simple linear regression
	first := recent[0]
	last := recent[len(recent)-1]
	duration := last.timestamp.Sub(first.timestamp).Minutes()

	if duration < 0.1 {
		return 0
	}

	return float64(last.temp-first.temp) / duration
}

func (fc *FanController) trimHistory() {
	cutoff := time.Now().Add(-time.Duration(fc.cfg.Monitoring.HistoryRetention) * time.Second)

	var newCPU []tempPoint
	for _, p := range fc.cpuHistory {
		if p.timestamp.After(cutoff) {
			newCPU = append(newCPU, p)
		}
	}
	fc.cpuHistory = newCPU

	var newGPU []tempPoint
	for _, p := range fc.gpuHistory {
		if p.timestamp.After(cutoff) {
			newGPU = append(newGPU, p)
		}
	}
	fc.gpuHistory = newGPU
}

func (fc *FanController) cleanExpired() {
	now := time.Now()

	for key, hint := range fc.hints {
		if !hint.ExpiresAt.IsZero() && hint.ExpiresAt.Before(now) {
			delete(fc.hints, key)
		}
	}

	if fc.override != nil && !fc.override.ExpiresAt.IsZero() && fc.override.ExpiresAt.Before(now) {
		fc.override = nil
	}
}

func (fc *FanController) setFanSpeed(speed int) error {
	// Convert percentage to hex (0-100 -> 0x00-0x64)
	hexSpeed := fmt.Sprintf("0x%02x", speed)

	// currentSpeed must reflect what the fans are ACTUALLY set to, so it is only
	// updated after a confirmed successful write. A failed write is surfaced via
	// lastWriteFailed in /api/status.
	if err := fc.ipmitool("raw", "0x30", "0x30", "0x02", "0xff", hexSpeed); err != nil {
		fc.mu.Lock()
		fc.lastWriteFailed = true
		fc.mu.Unlock()
		return err
	}

	fc.mu.Lock()
	fc.currentSpeed = speed
	fc.lastWriteFailed = false
	fc.mu.Unlock()
	return nil
}

func (fc *FanController) enableManualMode() error {
	return fc.ipmitool("raw", "0x30", "0x30", "0x01", "0x00")
}

func (fc *FanController) RestoreAutoMode() error {
	return fc.ipmitool("raw", "0x30", "0x30", "0x01", "0x01")
}

// AddHint registers a workload hint
func (fc *FanController) AddHint(hint *WorkloadHint) {
	fc.mu.Lock()
	defer fc.mu.Unlock()

	// Set minimum fan speed based on intensity
	switch hint.Intensity {
	case "high":
		hint.MinFanSpeed = 45 // warm zone minimum
	case "medium":
		hint.MinFanSpeed = 25 // normal zone minimum
	case "low":
		hint.MinFanSpeed = 15
	default:
		hint.MinFanSpeed = 25
	}

	hint.CreatedAt = time.Now()
	fc.hints[hint.Source] = hint

	log.Printf("Hint registered: %s from %s (min fan: %d%%)", hint.Action, hint.Source, hint.MinFanSpeed)
}

// RemoveHint removes a workload hint
func (fc *FanController) RemoveHint(source string) {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	delete(fc.hints, source)
	log.Printf("Hint removed: %s", source)
}

// SetOverride sets a manual fan speed override
func (fc *FanController) SetOverride(speed int, duration time.Duration, reason string) {
	fc.mu.Lock()
	defer fc.mu.Unlock()

	fc.override = &Override{
		Speed:     speed,
		Reason:    reason,
		CreatedAt: time.Now(),
	}

	if duration > 0 {
		fc.override.ExpiresAt = time.Now().Add(duration)
	}

	log.Printf("Override set: %d%% (%s)", speed, reason)
}

// ClearOverride removes the manual override
func (fc *FanController) ClearOverride() {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	fc.override = nil
	log.Printf("Override cleared")
}

// GetStatus returns the current controller status
func (fc *FanController) GetStatus() *Status {
	fc.mu.RLock()
	defer fc.mu.RUnlock()

	hints := make([]*WorkloadHint, 0, len(fc.hints))
	for _, h := range fc.hints {
		hints = append(hints, h)
	}

	mode := "auto"
	if fc.override != nil {
		mode = "override"
	} else if len(fc.hints) > 0 {
		mode = "hinted"
	}

	// Build threshold info for dashboard
	cpuThreshold := fc.cfg.FanControl.EffectiveCPUThreshold()
	gpuThreshold := fc.cfg.FanControl.EffectiveGPUThreshold()

	return &Status{
		Timestamp:       time.Now(),
		CPU:             fc.lastCPUReading,
		GPU:             fc.lastGPUReading,
		CurrentSpeed:    fc.currentSpeed,
		TargetSpeed:     fc.targetSpeed,
		Zone:            fc.currentZone,
		Mode:            mode,
		ActiveHints:     hints,
		Override:        fc.override,
		CPUTrend:        fc.calculateTrend(fc.cpuHistory),
		GPUTrend:        fc.calculateTrend(fc.gpuHistory),
		Zones:           fc.cfg.Zones,
		CPUThreshold:    cpuThreshold,
		GPUThreshold:    gpuThreshold,
		IdleSpeed:       fc.cfg.FanControl.IdleSpeed,
		FailsafeActive:  fc.failsafeCause != failsafeNone,
		FailsafeReason:  fc.failsafeCause.String(),
		RestorePending:  fc.failsafeCause != failsafeNone && !fc.restoreConfirmed,
		LastWriteFailed: fc.lastWriteFailed,
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
