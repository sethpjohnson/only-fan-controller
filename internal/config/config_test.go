package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestExampleConfigIsValid loads config.example.yaml verbatim (as a fresh
// install would) and confirms it passes Validate(), so the shipped example
// never silently bit-rots into a config the service refuses to start with.
func TestExampleConfigIsValid(t *testing.T) {
	cfg, err := Load(filepath.Join("..", "..", "config.example.yaml"))
	if err != nil {
		t.Fatalf("config.example.yaml failed to load/validate: %v", err)
	}
	if cfg.Storage.RetentionDays <= 0 {
		t.Fatalf("expected a positive retention_days, got %d", cfg.Storage.RetentionDays)
	}
}

// TestLegacyLoggingSectionIsIgnored confirms that removing the logging config
// struct doesn't break loading of older config files that still have a
// (now-unused) `logging:` section: yaml.v3 ignores unknown keys, so this
// should parse fine and fall back to defaults for everything else.
func TestLegacyLoggingSectionIsIgnored(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-config.yaml")
	content := `idrac:
  host: "local"
fan_control:
  critical_cpu_temp: 85
  critical_gpu_temp: 90
logging:
  level: "debug"
  file: "/var/log/foo.log"
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("failed to write temp config: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("legacy config with a logging: section should still load, got: %v", err)
	}
	if cfg.Storage.RetentionDays != 30 {
		t.Fatalf("expected default retention_days=30, got %d", cfg.Storage.RetentionDays)
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(c *Config)
		wantErr bool
	}{
		{
			name:    "default config is valid",
			mutate:  func(c *Config) {},
			wantErr: false,
		},
		{
			name:    "critical CPU temp too low is rejected",
			mutate:  func(c *Config) { c.FanControl.CriticalCPUTemp = 30 },
			wantErr: true,
		},
		{
			name:    "critical CPU temp absurdly high is rejected",
			mutate:  func(c *Config) { c.FanControl.CriticalCPUTemp = 200 },
			wantErr: true,
		},
		{
			name:    "critical temp at/below normal threshold is rejected",
			mutate:  func(c *Config) { c.FanControl.CriticalCPUTemp = c.FanControl.CPUThreshold },
			wantErr: true,
		},
		{
			name: "unset cpu_threshold still checks critical against effective default",
			mutate: func(c *Config) {
				c.FanControl.CPUThreshold = 0     // unset -> effective default 65
				c.FanControl.CriticalCPUTemp = 60 // below effective default
			},
			wantErr: true,
		},
		{
			name: "unset cpu_threshold with sane critical is valid",
			mutate: func(c *Config) {
				c.FanControl.CPUThreshold = 0     // effective default 65
				c.FanControl.CriticalCPUTemp = 70 // above effective default
			},
			wantErr: false,
		},
		{
			name:    "max speed above 100 is rejected",
			mutate:  func(c *Config) { c.FanControl.MaxSpeed = 120 },
			wantErr: true,
		},
		{
			name:    "min speed above max is rejected",
			mutate:  func(c *Config) { c.FanControl.MinSpeed = 90; c.FanControl.MaxSpeed = 80 },
			wantErr: true,
		},
		{
			name: "non-monotonic zones are rejected",
			mutate: func(c *Config) {
				c.Zones = []Zone{
					{Name: "idle", CPUMax: 45, GPUMax: 40, FanSpeed: 10},
					{Name: "backwards", CPUMax: 30, GPUMax: 30, FanSpeed: 5},
				}
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := Default()
			tt.mutate(c)
			err := c.Validate()
			if tt.wantErr && err == nil {
				t.Fatal("expected validation error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected validation error: %v", err)
			}
		})
	}
}

func TestIsCritical(t *testing.T) {
	c := Default() // CriticalCPUTemp=85, CriticalGPUTemp=90
	tests := []struct {
		cpu, gpu int
		want     bool
	}{
		{cpu: 84, gpu: 50, want: false}, // just below critical CPU
		{cpu: 85, gpu: 50, want: true},  // at critical CPU
		{cpu: 40, gpu: 90, want: true},  // at critical GPU
		{cpu: 40, gpu: 89, want: false}, // just below critical GPU
		{cpu: 81, gpu: 50, want: false}, // hot-zone band stays reachable, not critical
	}
	for _, tt := range tests {
		if got := c.IsCritical(tt.cpu, tt.gpu); got != tt.want {
			t.Fatalf("IsCritical(%d, %d) = %v, want %v", tt.cpu, tt.gpu, got, tt.want)
		}
	}
}
