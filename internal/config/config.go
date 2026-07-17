package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type Config struct {
	IDRAC      IDRACConfig      `yaml:"idrac"`
	Monitoring MonitoringConfig `yaml:"monitoring"`
	GPU        GPUConfig        `yaml:"gpu"`
	Zones      []Zone           `yaml:"zones"`
	FanControl FanControlConfig `yaml:"fan_control"`
	API        APIConfig        `yaml:"api"`
	Dashboard  DashboardConfig  `yaml:"dashboard"`
	Storage    StorageConfig    `yaml:"storage"`
	Logging    LoggingConfig    `yaml:"logging"`
}

type IDRACConfig struct {
	Host     string `yaml:"host"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

type MonitoringConfig struct {
	Interval         int `yaml:"interval"`          // Seconds between checks
	HistoryRetention int `yaml:"history_retention"` // Seconds to keep history
}

type GPUConfig struct {
	Enabled       bool   `yaml:"enabled"`
	NvidiaSmiPath string `yaml:"nvidia_smi_path"`
}

type Zone struct {
	Name     string `yaml:"name" json:"name"`
	CPUMax   int    `yaml:"cpu_max" json:"cpu_max"`
	GPUMax   int    `yaml:"gpu_max" json:"gpu_max"`
	FanSpeed int    `yaml:"fan_speed" json:"fan_speed"`
}

type FanControlConfig struct {
	MinSpeed      int `yaml:"min_speed" json:"min_speed"`
	MaxSpeed      int `yaml:"max_speed" json:"max_speed"`
	IdleSpeed     int `yaml:"idle_speed" json:"idle_speed"`         // Base fan speed when idle
	CPUThreshold  int `yaml:"cpu_threshold" json:"cpu_threshold"`   // CPU temp that triggers fan increase
	GPUThreshold  int `yaml:"gpu_threshold" json:"gpu_threshold"`   // GPU temp that triggers fan increase
	StepSize      int `yaml:"step_size" json:"step_size"`           // Fan speed increment per threshold breach
	CooldownDelay int `yaml:"cooldown_delay" json:"cooldown_delay"` // Seconds below threshold before ramping down
	// Critical temperatures trigger the emergency ramp (fans jump straight to
	// MaxSpeed, bypassing step ramping and any manual override). These are the
	// explicit, validated trigger thresholds — the emergency SPEED is always
	// MaxSpeed and is never taken from operator-editable zone data.
	CriticalCPUTemp int `yaml:"critical_cpu_temp" json:"critical_cpu_temp"` // CPU temp (>=) that forces the emergency ramp
	CriticalGPUTemp int `yaml:"critical_gpu_temp" json:"critical_gpu_temp"` // GPU temp (>=) that forces the emergency ramp
	// Fail-safe thresholds: after this many consecutive failures the controller
	// hands cooling back to the BMC's automatic fan control.
	SensorFailureLimit int `yaml:"sensor_failure_limit" json:"sensor_failure_limit"` // Consecutive sensor read failures before restoring auto mode
	WriteFailureLimit  int `yaml:"write_failure_limit" json:"write_failure_limit"`   // Consecutive fan-write failures before restoring auto mode
	// Legacy fields (still supported)
	RampUpStep   int  `yaml:"ramp_up_step" json:"ramp_up_step"`
	RampDownStep int  `yaml:"ramp_down_step" json:"ramp_down_step"`
	ConstantIdle bool `yaml:"constant_idle" json:"constant_idle"`
}

type APIConfig struct {
	Host string `yaml:"host"`
	Port int    `yaml:"port"`
}

type DashboardConfig struct {
	Enabled bool `yaml:"enabled"`
	Port    int  `yaml:"port"`
}

type StorageConfig struct {
	Path string `yaml:"path"`
}

type LoggingConfig struct {
	Level string `yaml:"level"`
	File  string `yaml:"file"`
}

// Default normal-ramp thresholds, applied when the corresponding config field is
// left at its zero value. Kept here so config validation and the controller
// agree on the effective values.
const (
	defaultCPUThreshold = 65
	defaultGPUThreshold = 60
)

// EffectiveCPUThreshold returns the CPU ramp threshold actually in force,
// substituting the default when unset.
func (fc FanControlConfig) EffectiveCPUThreshold() int {
	if fc.CPUThreshold > 0 {
		return fc.CPUThreshold
	}
	return defaultCPUThreshold
}

// EffectiveGPUThreshold returns the GPU ramp threshold actually in force,
// substituting the default when unset.
func (fc FanControlConfig) EffectiveGPUThreshold() int {
	if fc.GPUThreshold > 0 {
		return fc.GPUThreshold
	}
	return defaultGPUThreshold
}

// Load reads configuration from a YAML file
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	cfg := Default()
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, err
	}

	// Reject unsafe operator config so the caller can fall back to safe defaults.
	// This is critical for the emergency-ramp path: a malformed thresholds/zones
	// list must never be trusted to decide when to pin fans.
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}

// Validate checks that the safety-critical parts of the config are sane. It is
// intentionally strict about the fields that drive the emergency ramp and the
// fail-safe thresholds; an invalid config is rejected by Load so main falls back
// to the (always-valid) defaults rather than trusting bad operator input.
func (c *Config) Validate() error {
	fc := c.FanControl
	if fc.MinSpeed < 0 || fc.MaxSpeed > 100 || fc.MinSpeed > fc.MaxSpeed {
		return fmt.Errorf("invalid fan speed bounds: min=%d max=%d (require 0<=min<=max<=100)", fc.MinSpeed, fc.MaxSpeed)
	}
	// Critical thresholds must be explicit and physically plausible. They are the
	// sole trigger for the emergency ramp, so we do not accept 0 ("always
	// critical") or absurd values.
	if fc.CriticalCPUTemp < 40 || fc.CriticalCPUTemp > 120 {
		return fmt.Errorf("invalid critical_cpu_temp: %d (require 40..120)", fc.CriticalCPUTemp)
	}
	if fc.CriticalGPUTemp < 40 || fc.CriticalGPUTemp > 120 {
		return fmt.Errorf("invalid critical_gpu_temp: %d (require 40..120)", fc.CriticalGPUTemp)
	}
	// The critical trigger must sit above the normal ramp thresholds, otherwise
	// the step-ramp band is unreachable. Compare against the EFFECTIVE thresholds
	// (post-default) so an unset cpu_threshold: 0 does not silently skip the check
	// — and so only genuinely conflicting customizations trip validation.
	if cpuT := fc.EffectiveCPUThreshold(); fc.CriticalCPUTemp <= cpuT {
		return fmt.Errorf("critical_cpu_temp (%d) must exceed effective cpu_threshold (%d)", fc.CriticalCPUTemp, cpuT)
	}
	if gpuT := fc.EffectiveGPUThreshold(); fc.CriticalGPUTemp <= gpuT {
		return fmt.Errorf("critical_gpu_temp (%d) must exceed effective gpu_threshold (%d)", fc.CriticalGPUTemp, gpuT)
	}
	// Zones are informational (dashboard display), but if provided they must be
	// monotonic non-decreasing so the display stays coherent.
	for i := 1; i < len(c.Zones); i++ {
		prev, cur := c.Zones[i-1], c.Zones[i]
		if cur.CPUMax < prev.CPUMax || cur.GPUMax < prev.GPUMax || cur.FanSpeed < prev.FanSpeed {
			return fmt.Errorf("zones must be monotonic non-decreasing; zone %q breaks ordering", cur.Name)
		}
	}
	return nil
}

// Default returns a configuration with sensible defaults
func Default() *Config {
	return &Config{
		IDRAC: IDRACConfig{
			Host:     "",     // Must be set via config or IDRAC_HOST env var
			Username: "root", // Dell iDRAC default
			Password: "",     // Must be set via config or IDRAC_PASSWORD env var
		},
		Monitoring: MonitoringConfig{
			Interval:         10,
			HistoryRetention: 3600,
		},
		GPU: GPUConfig{
			Enabled:       true,
			NvidiaSmiPath: "/usr/bin/nvidia-smi",
		},
		Zones: []Zone{
			{Name: "idle", CPUMax: 45, GPUMax: 40, FanSpeed: 10},
			{Name: "normal", CPUMax: 60, GPUMax: 70, FanSpeed: 25},
			{Name: "warm", CPUMax: 70, GPUMax: 80, FanSpeed: 45},
			{Name: "hot", CPUMax: 80, GPUMax: 85, FanSpeed: 70},
			{Name: "critical", CPUMax: 999, GPUMax: 999, FanSpeed: 100},
		},
		FanControl: FanControlConfig{
			MinSpeed:           5,
			MaxSpeed:           100,
			IdleSpeed:          20, // Base fan speed when idle (quiet)
			CPUThreshold:       65, // Bump fans if CPU exceeds this
			GPUThreshold:       60, // Bump fans if GPU exceeds this
			StepSize:           10, // Increase fan by 10% per threshold breach
			CooldownDelay:      60, // Wait 60s below threshold before ramping down
			CriticalCPUTemp:    85, // Emergency ramp at/above this CPU temp
			CriticalGPUTemp:    90, // Emergency ramp at/above this GPU temp
			SensorFailureLimit: 3,  // Restore BMC auto mode after 3 consecutive sensor read failures
			WriteFailureLimit:  3,  // Restore BMC auto mode after 3 consecutive fan-write failures
			RampUpStep:         10,
			RampDownStep:       5,
			ConstantIdle:       true,
		},
		API: APIConfig{
			Host: "0.0.0.0",
			Port: 8086,
		},
		Dashboard: DashboardConfig{
			Enabled: true,
			Port:    8086,
		},
		Storage: StorageConfig{
			Path: "/var/lib/only-fan-controller/history.db",
		},
		Logging: LoggingConfig{
			Level: "info",
			File:  "/var/log/only-fan-controller.log",
		},
	}
}

// IsCritical reports whether the given temperatures have reached the explicit,
// validated critical thresholds. It deliberately does NOT consult the (operator-
// editable) Zones list: the trigger must not be steerable into pinning fans at a
// low speed. The emergency SPEED is decided separately (always MaxSpeed).
func (c *Config) IsCritical(cpuTemp, gpuTemp int) bool {
	return cpuTemp >= c.FanControl.CriticalCPUTemp || gpuTemp >= c.FanControl.CriticalGPUTemp
}
