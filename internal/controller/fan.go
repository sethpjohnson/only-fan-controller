package controller

import (
	"bytes"
	"fmt"
	"log"
	"os/exec"
	"sync"
	"time"

	"github.com/sethpjohnson/only-fan-controller/internal/config"
	"github.com/sethpjohnson/only-fan-controller/internal/monitor"
	"github.com/sethpjohnson/only-fan-controller/internal/storage"
)

type FanController struct {
	cfg     *config.Config
	cpuMon  *monitor.CPUMonitor
	gpuMon  *monitor.GPUMonitor
	store   *storage.Store
	
	// State
	mu              sync.RWMutex
	currentSpeed    int
	targetSpeed     int
	currentZone     string
	lastCPUReading  *monitor.CPUReading
	lastGPUReading  *monitor.GPUReading
	hints           map[string]*WorkloadHint
	override        *Override
	running         bool
	stopChan        chan struct{}
	
	// History for trend analysis
	cpuHistory      []tempPoint
	gpuHistory      []tempPoint
	
	// Hysteresis tracking
	lastOverThreshold time.Time
}

type tempPoint struct {
	temp      int
	timestamp time.Time
}

type WorkloadHint struct {
	Type       string    `json:"type"`
	Action     string    `json:"action"`
	Intensity  string    `json:"intensity"`
	Source     string    `json:"source"`
	MinFanSpeed int      `json:"min_fan_speed"`
	ExpiresAt  time.Time `json:"expires_at"`
	CreatedAt  time.Time `json:"created_at"`
}

type Override struct {
	Speed     int       `json:"speed"`
	Reason    string    `json:"reason"`
	ExpiresAt time.Time `json:"expires_at"`
	CreatedAt time.Time `json:"created_at"`
}

type Status struct {
	Timestamp     time.Time             `json:"timestamp"`
	CPU           *monitor.CPUReading   `json:"cpu"`
	GPU           *monitor.GPUReading   `json:"gpu"`
	CurrentSpeed  int                   `json:"current_speed"`
	TargetSpeed   int                   `json:"target_speed"`
	Zone          string                `json:"zone"`
	Mode          string                `json:"mode"`
	ActiveHints   []*WorkloadHint       `json:"active_hints"`
	Override      *Override             `json:"override,omitempty"`
	CPUTrend      float64               `json:"cpu_trend"`
	GPUTrend      float64               `json:"gpu_trend"`
	Zones         []config.Zone         `json:"zones"`
	CPUThreshold  int                   `json:"cpu_threshold"`
	GPUThreshold  int                   `json:"gpu_threshold"`
	IdleSpeed     int                   `json:"idle_speed"`
}

func NewFanController(cfg *config.Config, cpuMon *monitor.CPUMonitor, gpuMon *monitor.GPUMonitor, store *storage.Store) *FanController {
	return &FanController{
		cfg:        cfg,
		cpuMon:     cpuMon,
		gpuMon:     gpuMon,
		store:      store,
		hints:      make(map[string]*WorkloadHint),
		stopChan:   make(chan struct{}),
		cpuHistory: make([]tempPoint, 0),
		gpuHistory: make([]tempPoint, 0),
	}
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
	// Read temperatures
	cpuReading, err := fc.cpuMon.Read()
	if err != nil {
		log.Printf("CPU read error: %v", err)
		cpuReading = &monitor.CPUReading{Temps: []int{}, Max: 0}
	}

	gpuReading, err := fc.gpuMon.Read()
	if err != nil {
		log.Printf("GPU read error: %v", err)
		gpuReading = &monitor.GPUReading{Devices: []monitor.GPUDevice{}, Max: 0}
	}

	fc.mu.Lock()
	fc.lastCPUReading = cpuReading
	fc.lastGPUReading = gpuReading
	
	// Update history
	now := time.Now()
	fc.cpuHistory = append(fc.cpuHistory, tempPoint{temp: cpuReading.Max, timestamp: now})
	fc.gpuHistory = append(fc.gpuHistory, tempPoint{temp: gpuReading.Max, timestamp: now})
	
	// Trim old history
	fc.trimHistory()
	
	// Clean expired hints and overrides
	fc.cleanExpired()
	fc.mu.Unlock()

	// Calculate target fan speed
	target := fc.calculateTarget(cpuReading, gpuReading)

	// Apply fan speed
	if err := fc.setFanSpeed(target); err != nil {
		log.Printf("Failed to set fan speed: %v", err)
	}

	// Store reading
	fc.store.RecordReading(cpuReading.Max, gpuReading.Max, target)

	log.Printf("CPU: %d°C | GPU: %d°C | Zone: %s | Fan: %d%%", 
		cpuReading.Max, gpuReading.Max, fc.currentZone, target)
}

func (fc *FanController) calculateTarget(cpuReading *monitor.CPUReading, gpuReading *monitor.GPUReading) int {
	fc.mu.Lock()
	defer fc.mu.Unlock()

	cpuMax := cpuReading.Max
	gpuMax := gpuReading.Max

	// Check for manual override
	if fc.override != nil {
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
	cpuThreshold := fc.cfg.FanControl.CPUThreshold
	gpuThreshold := fc.cfg.FanControl.GPUThreshold
	if cpuThreshold == 0 {
		cpuThreshold = 65 // Default CPU threshold
	}
	if gpuThreshold == 0 {
		gpuThreshold = 60 // Default GPU threshold
	}

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
	fc.mu.Lock()
	fc.currentSpeed = speed
	fc.mu.Unlock()

	// Convert percentage to hex (0-100 -> 0x00-0x64)
	hexSpeed := fmt.Sprintf("0x%02x", speed)

	var cmd *exec.Cmd
	if fc.cfg.IDRAC.Host == "local" {
		cmd = exec.Command("ipmitool", "raw", "0x30", "0x30", "0x02", "0xff", hexSpeed)
	} else {
		cmd = exec.Command("ipmitool",
			"-I", "lanplus",
			"-H", fc.cfg.IDRAC.Host,
			"-U", fc.cfg.IDRAC.Username,
			"-P", fc.cfg.IDRAC.Password,
			"raw", "0x30", "0x30", "0x02", "0xff", hexSpeed,
		)
	}

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ipmitool error: %v, stderr: %s", err, stderr.String())
	}

	return nil
}

func (fc *FanController) enableManualMode() error {
	var cmd *exec.Cmd
	if fc.cfg.IDRAC.Host == "local" {
		cmd = exec.Command("ipmitool", "raw", "0x30", "0x30", "0x01", "0x00")
	} else {
		cmd = exec.Command("ipmitool",
			"-I", "lanplus",
			"-H", fc.cfg.IDRAC.Host,
			"-U", fc.cfg.IDRAC.Username,
			"-P", fc.cfg.IDRAC.Password,
			"raw", "0x30", "0x30", "0x01", "0x00",
		)
	}
	return cmd.Run()
}

func (fc *FanController) RestoreAutoMode() error {
	var cmd *exec.Cmd
	if fc.cfg.IDRAC.Host == "local" {
		cmd = exec.Command("ipmitool", "raw", "0x30", "0x30", "0x01", "0x01")
	} else {
		cmd = exec.Command("ipmitool",
			"-I", "lanplus",
			"-H", fc.cfg.IDRAC.Host,
			"-U", fc.cfg.IDRAC.Username,
			"-P", fc.cfg.IDRAC.Password,
			"raw", "0x30", "0x30", "0x01", "0x01",
		)
	}
	return cmd.Run()
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
	cpuThreshold := fc.cfg.FanControl.CPUThreshold
	gpuThreshold := fc.cfg.FanControl.GPUThreshold
	if cpuThreshold == 0 {
		cpuThreshold = 65
	}
	if gpuThreshold == 0 {
		gpuThreshold = 60
	}

	return &Status{
		Timestamp:    time.Now(),
		CPU:          fc.lastCPUReading,
		GPU:          fc.lastGPUReading,
		CurrentSpeed: fc.currentSpeed,
		TargetSpeed:  fc.targetSpeed,
		Zone:         fc.currentZone,
		Mode:         mode,
		ActiveHints:  hints,
		Override:     fc.override,
		CPUTrend:     fc.calculateTrend(fc.cpuHistory),
		GPUTrend:     fc.calculateTrend(fc.gpuHistory),
		Zones:        fc.cfg.Zones,
		CPUThreshold: cpuThreshold,
		GPUThreshold: gpuThreshold,
		IdleSpeed:    fc.cfg.FanControl.IdleSpeed,
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
