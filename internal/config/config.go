package config

import (
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
	Interval         int `yaml:"interval"`           // Seconds between checks
	HistoryRetention int `yaml:"history_retention"`  // Seconds to keep history
}

type GPUConfig struct {
	Enabled      bool   `yaml:"enabled"`
	NvidiaSmiPath string `yaml:"nvidia_smi_path"`
}

type Zone struct {
	Name     string `yaml:"name" json:"name"`
	CPUMax   int    `yaml:"cpu_max" json:"cpu_max"`
	GPUMax   int    `yaml:"gpu_max" json:"gpu_max"`
	FanSpeed int    `yaml:"fan_speed" json:"fan_speed"`
}

type FanControlConfig struct {
	MinSpeed       int  `yaml:"min_speed" json:"min_speed"`
	MaxSpeed       int  `yaml:"max_speed" json:"max_speed"`
	IdleSpeed      int  `yaml:"idle_speed" json:"idle_speed"`             // Base fan speed when idle
	CPUThreshold   int  `yaml:"cpu_threshold" json:"cpu_threshold"`       // CPU temp that triggers fan increase
	GPUThreshold   int  `yaml:"gpu_threshold" json:"gpu_threshold"`       // GPU temp that triggers fan increase
	StepSize       int  `yaml:"step_size" json:"step_size"`               // Fan speed increment per threshold breach
	CooldownDelay  int  `yaml:"cooldown_delay" json:"cooldown_delay"`     // Seconds below threshold before ramping down
	// Legacy fields (still supported)
	RampUpStep     int  `yaml:"ramp_up_step" json:"ramp_up_step"`
	RampDownStep   int  `yaml:"ramp_down_step" json:"ramp_down_step"`
	ConstantIdle   bool `yaml:"constant_idle" json:"constant_idle"`
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

	return cfg, nil
}

// Default returns a configuration with sensible defaults
func Default() *Config {
	return &Config{
		IDRAC: IDRACConfig{
			Host:     "",      // Must be set via config or IDRAC_HOST env var
			Username: "root",  // Dell iDRAC default
			Password: "",      // Must be set via config or IDRAC_PASSWORD env var
		},
		Monitoring: MonitoringConfig{
			Interval:         10,
			HistoryRetention: 3600,
		},
		GPU: GPUConfig{
			Enabled:      true,
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
			MinSpeed:      5,
			MaxSpeed:      100,
			IdleSpeed:     20,          // Base fan speed when idle (quiet)
			CPUThreshold:  65,          // Bump fans if CPU exceeds this
			GPUThreshold:  60,          // Bump fans if GPU exceeds this
			StepSize:      10,          // Increase fan by 10% per threshold breach
			CooldownDelay: 60,          // Wait 60s below threshold before ramping down
			RampUpStep:    10,
			RampDownStep:  5,
			ConstantIdle:  true,
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

// GetZone returns the appropriate zone for given temperatures
func (c *Config) GetZone(cpuTemp, gpuTemp int) *Zone {
	for i := range c.Zones {
		zone := &c.Zones[i]
		if cpuTemp <= zone.CPUMax && gpuTemp <= zone.GPUMax {
			return zone
		}
	}
	// Return last zone (critical) if nothing matches
	return &c.Zones[len(c.Zones)-1]
}
