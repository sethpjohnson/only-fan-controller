package controller

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/sethpjohnson/only-fan-controller/internal/config"
	"github.com/sethpjohnson/only-fan-controller/internal/monitor"
	"github.com/sethpjohnson/only-fan-controller/internal/storage"
)

// testConfig returns a fresh default config for deterministic tests.
func testConfig() *config.Config {
	return config.Default()
}

func cpuGpu(cpuMax, gpuMax int) (*monitor.CPUReading, *monitor.GPUReading) {
	return &monitor.CPUReading{Temps: []int{cpuMax}, Max: cpuMax},
		&monitor.GPUReading{Max: gpuMax}
}

func TestCalculateTarget(t *testing.T) {
	tests := []struct {
		name string
		// setup mutates the controller before the call.
		setup func(fc *FanController)
		cpu   int
		gpu   int
		want  int
	}{
		{
			name:  "idle below all thresholds goes to idle speed",
			setup: func(fc *FanController) { fc.currentSpeed = 0 },
			cpu:   40, gpu: 35,
			want: 20, // IdleSpeed
		},
		{
			name: "threshold crossing ramps up by one step",
			setup: func(fc *FanController) {
				fc.currentSpeed = 20
			},
			cpu: 75, gpu: 35,
			want: 30, // 20 + one StepSize
		},
		{
			name: "hysteresis holds speed during cooldown",
			setup: func(fc *FanController) {
				fc.currentSpeed = 40
				fc.lastOverThreshold = time.Now()
			},
			cpu: 50, gpu: 35,
			want: 40, // unchanged during cooldown
		},
		{
			name: "ramps down one step after cooldown elapses",
			setup: func(fc *FanController) {
				fc.currentSpeed = 40
				fc.lastOverThreshold = time.Now().Add(-2 * time.Minute)
			},
			cpu: 50, gpu: 35,
			want: 30, // 40 - one StepSize
		},
		{
			name: "manual override takes precedence over normal logic",
			setup: func(fc *FanController) {
				fc.currentSpeed = 20
				fc.override = &Override{Speed: 55}
			},
			cpu: 50, gpu: 35,
			want: 55,
		},
		{
			name: "critical temp overrides even a low manual override",
			setup: func(fc *FanController) {
				fc.currentSpeed = 10
				fc.override = &Override{Speed: 20}
			},
			cpu: 95, gpu: 35,
			want: 100, // critical zone speed
		},
		{
			name: "critical temp fast-ramps straight to 100 from idle",
			setup: func(fc *FanController) {
				fc.currentSpeed = 10
			},
			cpu: 95, gpu: 35,
			want: 100,
		},
		{
			name: "critical GPU temp also fast-ramps",
			setup: func(fc *FanController) {
				fc.currentSpeed = 10
			},
			cpu: 40, gpu: 95,
			want: 100,
		},
		{
			name: "target clamped up to configured min speed",
			setup: func(fc *FanController) {
				fc.currentSpeed = 0
				fc.cfg.FanControl.MinSpeed = 30
			},
			cpu: 40, gpu: 35,
			want: 30, // idle speed 20 clamped up to min 30
		},
		{
			name: "target clamped down to configured max speed",
			setup: func(fc *FanController) {
				fc.currentSpeed = 0
				fc.cfg.FanControl.IdleSpeed = 90
				fc.cfg.FanControl.MaxSpeed = 80
			},
			cpu: 40, gpu: 35,
			want: 80, // idle speed 90 clamped down to max 80
		},
		{
			name: "workload hint sets a fan floor",
			setup: func(fc *FanController) {
				fc.currentSpeed = 0
				fc.hints["test"] = &WorkloadHint{Source: "test", MinFanSpeed: 45}
			},
			cpu: 40, gpu: 35,
			want: 45,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fc := NewFanController(testConfig(), nil, nil, nil)
			if tt.setup != nil {
				tt.setup(fc)
			}
			cpuR, gpuR := cpuGpu(tt.cpu, tt.gpu)
			got := fc.calculateTarget(cpuR, gpuR)
			if got != tt.want {
				t.Fatalf("calculateTarget(cpu=%d, gpu=%d) = %d, want %d", tt.cpu, tt.gpu, got, tt.want)
			}
		})
	}
}

// TestCriticalRampIgnoresOperatorZoneData proves the emergency ramp trusts the
// fixed MaxSpeed ceiling, never operator-editable zone fan speeds. Even a
// malicious/misconfigured Zones list whose top zone asks for a LOW speed must
// not weaken the emergency ramp, and critical still overrides a low manual
// override.
func TestCriticalRampIgnoresOperatorZoneData(t *testing.T) {
	cfg := config.Default()
	// Hostile zone list: "critical" zone requests a dangerously low 15%.
	cfg.Zones = []config.Zone{
		{Name: "idle", CPUMax: 45, GPUMax: 40, FanSpeed: 10},
		{Name: "critical", CPUMax: 999, GPUMax: 999, FanSpeed: 15},
	}
	cfg.FanControl.MaxSpeed = 100
	cfg.FanControl.CriticalCPUTemp = 85
	cfg.FanControl.CriticalGPUTemp = 90

	fc := NewFanController(cfg, nil, nil, nil)
	fc.override = &Override{Speed: 20} // low manual override
	fc.currentSpeed = 10

	cpuR, gpuR := cpuGpu(95, 35) // CPU critical
	if got := fc.calculateTarget(cpuR, gpuR); got != 100 {
		t.Fatalf("critical ramp used operator zone data / override instead of MaxSpeed: got %d, want 100", got)
	}
}

// --- fail-safe wiring tests ---

// cmdRecorder captures the ipmitool invocations made through runCommand.
type cmdRecorder struct {
	cmds         [][]string
	failOnFanSet bool
	failRestore  bool // simulate an unreachable BMC: RestoreAutoMode (0x01 0x01) fails
}

func (r *cmdRecorder) run(_ context.Context, name string, args ...string) error {
	call := append([]string{name}, args...)
	r.cmds = append(r.cmds, call)
	if r.failOnFanSet && containsArg(args, "0x02") {
		return errors.New("simulated ipmitool fan-set failure")
	}
	if r.failRestore && endsWith(call, "0x01", "0x01") {
		return errors.New("simulated unreachable BMC on RestoreAutoMode")
	}
	return nil
}

func containsArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

// restoreCount counts how many times BMC auto mode was restored (raw ... 0x01 0x01).
func (r *cmdRecorder) restoreCount() int {
	n := 0
	for _, c := range r.cmds {
		if endsWith(c, "0x01", "0x01") {
			n++
		}
	}
	return n
}

func (r *cmdRecorder) fanSetCount() int {
	n := 0
	for _, c := range r.cmds {
		if containsArg(c, "0x02") {
			n++
		}
	}
	return n
}

// manualModeCount counts how many times manual fan mode was (re-)enabled
// (raw ... 0x01 0x00). Together with restoreCount this exposes BMC flapping.
func (r *cmdRecorder) manualModeCount() int {
	n := 0
	for _, c := range r.cmds {
		if endsWith(c, "0x01", "0x00") {
			n++
		}
	}
	return n
}

func endsWith(s []string, a, b string) bool {
	if len(s) < 2 {
		return false
	}
	return s[len(s)-2] == a && s[len(s)-1] == b
}

type failingCPU struct{}

func (failingCPU) Read() (*monitor.CPUReading, error) {
	return nil, errors.New("simulated CPU sensor loss")
}

// flakyCPU fails its first failFor reads, then succeeds — used to test recovery.
type flakyCPU struct {
	failFor int
	calls   int
	max     int
}

func (f *flakyCPU) Read() (*monitor.CPUReading, error) {
	f.calls++
	if f.calls <= f.failFor {
		return nil, errors.New("simulated transient CPU sensor loss")
	}
	return &monitor.CPUReading{Temps: []int{f.max}, Max: f.max}, nil
}

type staticCPU struct{ max int }

func (s staticCPU) Read() (*monitor.CPUReading, error) {
	return &monitor.CPUReading{Temps: []int{s.max}, Max: s.max}, nil
}

type staticGPU struct{ max int }

func (s staticGPU) Read() (*monitor.GPUReading, error) {
	return &monitor.GPUReading{Max: s.max}, nil
}

func newTestStore(t *testing.T) *storage.Store {
	t.Helper()
	store, err := storage.New(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func TestControlLoopHoldsSpeedOnSensorFailure(t *testing.T) {
	rec := &cmdRecorder{}
	fc := NewFanController(testConfig(), failingCPU{}, staticGPU{max: 40}, newTestStore(t))
	fc.runCommand = rec.run
	fc.currentSpeed = 40

	fc.controlLoop()

	if fc.currentSpeed != 40 {
		t.Fatalf("currentSpeed changed on sensor failure: got %d, want 40", fc.currentSpeed)
	}
	if writes := rec.fanSetCount(); writes != 0 {
		t.Fatalf("fan speed was written on sensor failure (%d writes); must hold instead", writes)
	}
}

func TestSensorFailureThresholdRestoresAutoMode(t *testing.T) {
	rec := &cmdRecorder{}
	cfg := testConfig()
	cfg.FanControl.SensorFailureLimit = 3
	fc := NewFanController(cfg, failingCPU{}, staticGPU{max: 40}, newTestStore(t))
	fc.runCommand = rec.run
	fc.currentSpeed = 40

	fc.controlLoop()
	fc.controlLoop()
	if got := rec.restoreCount(); got != 0 {
		t.Fatalf("restored auto mode too early after 2 failures: %d restores", got)
	}
	fc.controlLoop() // third failure crosses the threshold
	if got := rec.restoreCount(); got != 1 {
		t.Fatalf("expected exactly one auto-mode restore after %d failures, got %d", cfg.FanControl.SensorFailureLimit, got)
	}

	// Keep failing well past the threshold: must NOT re-restore or flap.
	for i := 0; i < 6; i++ {
		fc.controlLoop()
	}
	if got := rec.restoreCount(); got != 1 {
		t.Fatalf("sensor fail-safe re-restored auto mode (oscillation): %d restores, want 1", got)
	}
	if got := rec.manualModeCount(); got != 0 {
		t.Fatalf("sensor fail-safe flapped back to manual while sensors still failing: %d manual toggles", got)
	}
}

// TestWriteFailureStickyNoOscillation reproduces the reviewer's scenario:
// healthy sensors + persistently failing fan writes. The fail-safe must engage
// exactly once and then stay in BMC auto — no manual/auto flapping.
func TestWriteFailureStickyNoOscillation(t *testing.T) {
	rec := &cmdRecorder{failOnFanSet: true}
	cfg := testConfig()
	cfg.FanControl.WriteFailureLimit = 3
	fc := NewFanController(cfg, staticCPU{max: 50}, staticGPU{max: 40}, newTestStore(t))
	fc.runCommand = rec.run

	fc.controlLoop()
	fc.controlLoop()
	if got := rec.restoreCount(); got != 0 {
		t.Fatalf("restored auto mode too early after 2 write failures: %d restores", got)
	}
	fc.controlLoop() // third write failure crosses the threshold
	if got := rec.restoreCount(); got != 1 {
		t.Fatalf("expected exactly one auto-mode restore after %d write failures, got %d", cfg.FanControl.WriteFailureLimit, got)
	}

	fanSetsAtTrip := rec.fanSetCount()

	// Run many more ticks with writes still failing. The reviewer's bug flapped
	// once per tick; the sticky write fail-safe must converge.
	for i := 0; i < 10; i++ {
		fc.controlLoop()
	}
	if got := rec.restoreCount(); got != 1 {
		t.Fatalf("write fail-safe oscillated: %d auto-mode restores after threshold, want 1", got)
	}
	if got := rec.manualModeCount(); got != 0 {
		t.Fatalf("write fail-safe flapped back to manual: %d manual toggles, want 0", got)
	}
	if got := rec.fanSetCount(); got != fanSetsAtTrip {
		t.Fatalf("write fail-safe kept attempting fan writes after going sticky: %d attempts, want %d", got, fanSetsAtTrip)
	}
	if fc.currentFailsafeCause() != failsafeWrite {
		t.Fatalf("expected sticky write fail-safe, got cause %v", fc.currentFailsafeCause())
	}
	// Readings must still be refreshed for /api/status visibility while sticky.
	if fc.lastCPUReading == nil || fc.lastCPUReading.Max != 50 {
		t.Fatalf("readings not refreshed during write fail-safe: %+v", fc.lastCPUReading)
	}
}

// TestSensorFailsafeRecoversAndReclaimsManual proves the recoverable domain:
// once sensors read successfully again, manual control is reclaimed exactly once.
func TestSensorFailsafeRecoversAndReclaimsManual(t *testing.T) {
	rec := &cmdRecorder{}
	cfg := testConfig()
	cfg.FanControl.SensorFailureLimit = 3
	cpu := &flakyCPU{failFor: 3, max: 50}
	fc := NewFanController(cfg, cpu, staticGPU{max: 40}, newTestStore(t))
	fc.runCommand = rec.run
	fc.currentSpeed = 30

	// 3 failing reads -> enter sensor fail-safe.
	fc.controlLoop()
	fc.controlLoop()
	fc.controlLoop()
	if fc.currentFailsafeCause() != failsafeSensor {
		t.Fatalf("expected sensor fail-safe after 3 failures, got %v", fc.currentFailsafeCause())
	}
	if got := rec.restoreCount(); got != 1 {
		t.Fatalf("expected one auto restore, got %d", got)
	}

	// 4th read succeeds -> reclaim manual and resume control.
	fc.controlLoop()
	if fc.currentFailsafeCause() != failsafeNone {
		t.Fatalf("fail-safe not cleared after sensor recovery: %v", fc.currentFailsafeCause())
	}
	if got := rec.manualModeCount(); got != 1 {
		t.Fatalf("expected exactly one manual-mode reclaim on recovery, got %d", got)
	}
	if got := rec.fanSetCount(); got != 1 {
		t.Fatalf("expected one fan write after resuming control, got %d", got)
	}
	// Counters reset for a fresh future failure window.
	if fc.sensorFailCount != 0 {
		t.Fatalf("sensorFailCount not reset after recovery: %d", fc.sensorFailCount)
	}
}

// TestFailsafeRetriesRestoreUntilConfirmed reproduces an unreachable BMC: the
// very condition that trips fail-safe also fails RestoreAutoMode. The controller
// must keep retrying every tick (not give up after one attempt) and report the
// restore as pending; once the BMC returns, restore succeeds once and retries
// stop.
func TestFailsafeRetriesRestoreUntilConfirmed(t *testing.T) {
	rec := &cmdRecorder{failRestore: true}
	cfg := testConfig()
	cfg.FanControl.SensorFailureLimit = 3
	fc := NewFanController(cfg, failingCPU{}, staticGPU{max: 40}, newTestStore(t))
	fc.runCommand = rec.run
	fc.currentSpeed = 40

	// Cross the sensor threshold -> first (failed) restore attempt.
	fc.controlLoop()
	fc.controlLoop()
	fc.controlLoop()
	if got := rec.restoreCount(); got != 1 {
		t.Fatalf("expected 1 restore attempt at trip, got %d", got)
	}
	if fc.isRestoreConfirmed() {
		t.Fatal("restore should not be confirmed while BMC is unreachable")
	}

	// Keep failing for N more ticks: each tick must retry the restore.
	const extra = 5
	for i := 0; i < extra; i++ {
		fc.controlLoop()
	}
	if got := rec.restoreCount(); got != 1+extra {
		t.Fatalf("expected %d restore attempts (retrying every tick), got %d", 1+extra, got)
	}
	if got := fc.GetStatus(); !got.RestorePending || !got.FailsafeActive {
		t.Fatalf("status should show failsafe active + restore pending, got %+v", got)
	}

	// BMC comes back: the next tick's retry succeeds, and retries then stop.
	rec.failRestore = false
	fc.controlLoop()
	attemptsAfterRecovery := rec.restoreCount()
	if !fc.isRestoreConfirmed() {
		t.Fatal("restore should be confirmed once the BMC is reachable again")
	}
	fc.controlLoop()
	fc.controlLoop()
	if got := rec.restoreCount(); got != attemptsAfterRecovery {
		t.Fatalf("restore retries did not stop after confirmation: %d -> %d", attemptsAfterRecovery, got)
	}
	if fc.GetStatus().RestorePending {
		t.Fatal("restore_pending should be false after confirmation")
	}
}

func TestSetFanSpeedOnlyUpdatesCurrentSpeedOnSuccess(t *testing.T) {
	rec := &cmdRecorder{failOnFanSet: true}
	fc := NewFanController(testConfig(), nil, nil, nil)
	fc.runCommand = rec.run
	fc.currentSpeed = 25

	if err := fc.setFanSpeed(80); err == nil {
		t.Fatal("expected setFanSpeed to return the simulated failure")
	}
	if fc.currentSpeed != 25 {
		t.Fatalf("currentSpeed updated despite failed write: got %d, want 25", fc.currentSpeed)
	}
	if !fc.lastWriteFailed {
		t.Fatal("lastWriteFailed should be true after a failed write")
	}

	rec.failOnFanSet = false
	if err := fc.setFanSpeed(80); err != nil {
		t.Fatalf("unexpected error on successful write: %v", err)
	}
	if fc.currentSpeed != 80 {
		t.Fatalf("currentSpeed not updated after successful write: got %d, want 80", fc.currentSpeed)
	}
	if fc.lastWriteFailed {
		t.Fatal("lastWriteFailed should be cleared after a successful write")
	}
}

// --- manual-override clamping (Tier 2 C3) ---

func TestSetOverrideClampsSpeedAndCapsDuration(t *testing.T) {
	cfg := config.Default()
	cfg.FanControl.MinSpeed = 20
	cfg.FanControl.MaxSpeed = 80
	fc := NewFanController(cfg, nil, nil, nil)

	// Above max clamps down; an infinite (0) duration is capped, not held forever.
	fc.SetOverride(100, 0, "too high")
	if fc.override.Speed != 80 {
		t.Fatalf("override speed above max not clamped: got %d, want 80", fc.override.Speed)
	}
	if fc.override.ExpiresAt.IsZero() {
		t.Fatal("override with duration 0 must be capped to a finite expiry, not infinite")
	}
	if d := time.Until(fc.override.ExpiresAt); d > maxOverrideDuration+time.Minute {
		t.Fatalf("infinite override not capped to %s: expires in %s", maxOverrideDuration, d)
	}

	// Below min clamps up.
	fc.SetOverride(1, time.Hour, "too low")
	if fc.override.Speed != 20 {
		t.Fatalf("override speed below min not clamped: got %d, want 20", fc.override.Speed)
	}

	// A duration beyond the cap is capped.
	fc.SetOverride(50, 48*time.Hour, "too long")
	if d := time.Until(fc.override.ExpiresAt); d > maxOverrideDuration+time.Minute {
		t.Fatalf("override duration not capped to %s: expires in %s", maxOverrideDuration, d)
	}
}

// calculateTarget must clamp the override even if an override was set directly
// (defense in depth), while temperatures are non-critical.
func TestCalculateTargetClampsOverride(t *testing.T) {
	cfg := config.Default()
	cfg.FanControl.MinSpeed = 20
	cfg.FanControl.MaxSpeed = 80
	fc := NewFanController(cfg, nil, nil, nil)

	cpuR, gpuR := cpuGpu(30, 30) // well below any threshold

	fc.override = &Override{Speed: 100} // set directly, bypassing SetOverride's clamp
	if got := fc.calculateTarget(cpuR, gpuR); got != 80 {
		t.Fatalf("override not clamped to max in calculateTarget: got %d, want 80", got)
	}

	fc.override = &Override{Speed: 1}
	if got := fc.calculateTarget(cpuR, gpuR); got != 20 {
		t.Fatalf("override not clamped to min in calculateTarget: got %d, want 20", got)
	}
}

// A critical temperature must still ramp to MaxSpeed even when a (clamped) low
// manual override is active — the Tier 1 emergency behavior must not regress.
func TestCriticalOverridesClampedOverride(t *testing.T) {
	cfg := config.Default()
	cfg.FanControl.MinSpeed = 20
	cfg.FanControl.MaxSpeed = 100
	cfg.FanControl.CriticalCPUTemp = 85
	fc := NewFanController(cfg, nil, nil, nil)

	fc.SetOverride(20, time.Hour, "low manual") // clamped, low
	cpuR, gpuR := cpuGpu(95, 30)                // CPU past critical
	if got := fc.calculateTarget(cpuR, gpuR); got != 100 {
		t.Fatalf("critical ramp did not override clamped override: got %d, want 100", got)
	}
}
