package main

import (
	"flag"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/sethpjohnson/only-fan-controller/internal/api"
	"github.com/sethpjohnson/only-fan-controller/internal/config"
	"github.com/sethpjohnson/only-fan-controller/internal/controller"
	"github.com/sethpjohnson/only-fan-controller/internal/monitor"
	"github.com/sethpjohnson/only-fan-controller/internal/storage"
)

// applyEnvOverrides applies environment variable overrides to the configuration
func applyEnvOverrides(cfg *config.Config) {
	// iDRAC settings
	if v := os.Getenv("IDRAC_HOST"); v != "" {
		cfg.IDRAC.Host = v
	}
	if v := os.Getenv("IDRAC_USERNAME"); v != "" {
		cfg.IDRAC.Username = v
	}
	if v := os.Getenv("IDRAC_PASSWORD"); v != "" {
		cfg.IDRAC.Password = v
	}

	// GPU settings
	if v := os.Getenv("GPU_ENABLED"); v != "" {
		cfg.GPU.Enabled = strings.ToLower(v) == "true" || v == "1"
	}

	// Fan control settings
	if v := os.Getenv("FAN_MIN_SPEED"); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			cfg.FanControl.MinSpeed = i
		}
	}
	if v := os.Getenv("FAN_MAX_SPEED"); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			cfg.FanControl.MaxSpeed = i
		}
	}
	if v := os.Getenv("FAN_IDLE_SPEED"); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			cfg.FanControl.IdleSpeed = i
		}
	}
	if v := os.Getenv("FAN_CPU_THRESHOLD"); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			cfg.FanControl.CPUThreshold = i
		}
	}
	if v := os.Getenv("FAN_GPU_THRESHOLD"); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			cfg.FanControl.GPUThreshold = i
		}
	}
	if v := os.Getenv("FAN_STEP_SIZE"); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			cfg.FanControl.StepSize = i
		}
	}
	if v := os.Getenv("FAN_COOLDOWN_DELAY"); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			cfg.FanControl.CooldownDelay = i
		}
	}

	// Monitoring interval
	if v := os.Getenv("CHECK_INTERVAL"); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			cfg.Monitoring.Interval = i
		}
	}

	// API port
	if v := os.Getenv("API_PORT"); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			cfg.API.Port = i
		}
	}
}

// FanControllerInterface allows both real and mock controllers
type FanControllerInterface interface {
	Run()
	Stop()
	RestoreAutoMode() error
	GetStatus() *controller.Status
	AddHint(hint *controller.WorkloadHint)
	RemoveHint(source string)
	SetOverride(speed int, duration interface{}, reason string)
	ClearOverride()
}

func main() {
	configPath := flag.String("config", "/etc/only-fan-controller/config.yaml", "Path to configuration file")
	demoMode := flag.Bool("demo", false, "Run in demo mode with simulated temperatures (no actual fan control)")
	flag.Parse()

	// Load configuration
	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Printf("Warning: Could not load config from %s: %v", *configPath, err)
		log.Println("Using default configuration")
		cfg = config.Default()
	}

	// Override with environment variables
	applyEnvOverrides(cfg)

	if *demoMode {
		log.Println("===========================================")
		log.Println("  DEMO MODE - No actual hardware control")
		log.Println("===========================================")
	}

	log.Printf("Only Fan Controller starting...")
	log.Printf("iDRAC host: %s", cfg.IDRAC.Host)
	log.Printf("GPU monitoring: %v", cfg.GPU.Enabled)
	log.Printf("API port: %d", cfg.API.Port)

	// Initialize storage for history
	store, err := storage.New(cfg.Storage.Path)
	if err != nil {
		log.Fatalf("Failed to initialize storage: %v", err)
	}
	defer store.Close()

	var fanCtrl *controller.FanController

	if *demoMode {
		// Use mock controller
		mockCtrl := controller.NewMockFanController(cfg, store)
		fanCtrl = mockCtrl.FanController

		// Start mock control loop
		go mockCtrl.Run()
	} else {
		// Initialize real monitors
		cpuMon := monitor.NewCPUMonitor(cfg)
		gpuMon := monitor.NewGPUMonitor(cfg)

		// Initialize real fan controller
		fanCtrl = controller.NewFanController(cfg, cpuMon, gpuMon, store)

		// Start the control loop
		go fanCtrl.Run()
	}

	// Initialize API server
	apiServer := api.NewServer(cfg, fanCtrl, store)

	// Start API server
	go func() {
		if err := apiServer.Run(); err != nil {
			log.Fatalf("API server error: %v", err)
		}
	}()

	log.Printf("API server listening on %s:%d", cfg.API.Host, cfg.API.Port)
	log.Printf("Dashboard: http://localhost:%d/dashboard/", cfg.API.Port)

	// Wait for shutdown signal
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	log.Println("Shutting down...")
	fanCtrl.Stop()

	// Restore Dell automatic fan control on exit (only in real mode)
	if !*demoMode {
		if err := fanCtrl.RestoreAutoMode(); err != nil {
			log.Printf("Warning: Failed to restore auto fan mode: %v", err)
		}
	}

	log.Println("Shutdown complete")
}
