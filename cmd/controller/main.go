package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

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

	// API bearer token for mutating endpoints
	if v := os.Getenv("API_TOKEN"); v != "" {
		cfg.API.Token = v
	}
}

func main() {
	os.Exit(run())
}

// runControlLoop runs the fan control loop and guarantees that a panic restores
// BMC automatic fan control before the process gives up. Any panic is reported
// on errCh so main can shut down through the same path that restores auto mode.
// The only ways to exit without restoring auto mode are SIGKILL / power loss.
func runControlLoop(loop func(), restore func() error, errCh chan<- error) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("PANIC in control loop: %v", r)
			if err := restore(); err != nil {
				log.Printf("CRITICAL: failed to restore auto fan mode after panic: %v", err)
			}
			errCh <- fmt.Errorf("control loop panic: %v", r)
		}
	}()
	loop()
}

// restoreOnce wraps a restore function so it runs at most once across all
// callers. On a control-loop panic the recover handler restores auto mode AND
// the shutdown path calls restore again; the guard makes the second call a
// no-op so the BMC is toggled exactly once.
func restoreOnce(restore func() error) func() error {
	var once sync.Once
	var firstErr error
	return func() error {
		once.Do(func() { firstErr = restore() })
		return firstErr
	}
}

// cleanupShutdownTimeout bounds how long shutdown waits for the history
// cleanup goroutine to stop. Thermal safety (restore()) always runs before
// this wait starts, so a hung Cleanup query must not stall process exit.
const cleanupShutdownTimeout = 5 * time.Second

// runHistoryCleanup prunes readings older than retention on a fixed daily
// interval until stopCh is closed. The initial cleanup (so a fresh start
// doesn't wait a full day before its first prune) is run by the caller before
// this goroutine is started; this loop only handles the recurring runs.
func runHistoryCleanup(store *storage.Store, retention time.Duration, retentionDays int, stopCh <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			logCleanupResult(store, retention, retentionDays)
		case <-stopCh:
			return
		}
	}
}

// logCleanupResult runs a single history cleanup pass and logs the outcome.
func logCleanupResult(store *storage.Store, retention time.Duration, retentionDays int) {
	deleted, err := store.Cleanup(retention)
	if err != nil {
		log.Printf("History cleanup failed: %v", err)
		return
	}
	log.Printf("History cleanup: removed %d old reading(s) (retention %d days)", deleted, retentionDays)
}

// resolveConfig loads the config, distinguishing a genuinely absent file (safe
// to fall back to defaults + env overrides) from a present-but-invalid file
// (parse or safety-validation failure). For the latter we REFUSE to start rather
// than silently fall back to Default(), which would discard the operator's real
// iDRAC host/credentials and come up pointed at `-H "" -U root -P ""` — making
// every IPMI call fail, including RestoreAutoMode itself. A process that never
// starts never enables manual mode, so the BMC keeps automatic control: that is
// the fail-safe outcome.
func resolveConfig(path string) (*config.Config, error) {
	cfg, err := config.Load(path)
	if err == nil {
		return cfg, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		log.Printf("Config file %s not found; using default configuration", path)
		return config.Default(), nil
	}
	// File exists but is unreadable, unparseable, or fails safety validation.
	return nil, err
}

// run holds the real program body so that deferred cleanup (e.g. store.Close)
// always runs, even on an abnormal exit. main() only translates the returned
// status into a process exit code.
func run() int {
	configPath := flag.String("config", "/etc/only-fan-controller/config.yaml", "Path to configuration file")
	demoMode := flag.Bool("demo", false, "Run in demo mode with simulated temperatures (no actual fan control)")
	flag.Parse()

	// Load configuration
	cfg, err := resolveConfig(*configPath)
	if err != nil {
		log.Printf("FATAL: refusing to start with an invalid config %s: %v", *configPath, err)
		log.Println("The config file exists but is invalid. NOT falling back to defaults, because that would")
		log.Println("discard your iDRAC credentials and leave the controller unable to talk to the BMC.")
		log.Println("The BMC keeps automatic fan control until you fix the config and restart.")
		return 1
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
		log.Printf("Failed to initialize storage: %v", err)
		return 1
	}
	defer store.Close()

	// Prune history once at startup, then keep pruning daily for as long as the
	// process runs. This is not on the safety-critical restore path, so its
	// goroutine is stopped (via cleanupStop/cleanupDone) before the deferred
	// store.Close() above runs, but is otherwise independent of the
	// control-loop/API shutdown below.
	retention := time.Duration(cfg.Storage.RetentionDays) * 24 * time.Hour
	logCleanupResult(store, retention, cfg.Storage.RetentionDays)
	cleanupStop := make(chan struct{})
	cleanupDone := make(chan struct{})
	go runHistoryCleanup(store, retention, cfg.Storage.RetentionDays, cleanupStop, cleanupDone)

	// errCh carries any fatal error (API server failure, control-loop panic)
	// back to the shutdown path so cleanup + auto-mode restore always run.
	errCh := make(chan error, 1)

	var fanCtrl *controller.FanController
	var restore func() error

	if *demoMode {
		// Use mock controller. Its RestoreAutoMode is log-only, so it is safe to
		// call on every exit path without touching hardware.
		mockCtrl := controller.NewMockFanController(cfg, store)
		fanCtrl = mockCtrl.FanController
		restore = restoreOnce(mockCtrl.RestoreAutoMode)
		go runControlLoop(mockCtrl.Run, restore, errCh)
	} else {
		// Initialize real monitors
		cpuMon := monitor.NewCPUMonitor(cfg)
		gpuMon := monitor.NewGPUMonitor(cfg)

		// Initialize real fan controller
		fanCtrl = controller.NewFanController(cfg, cpuMon, gpuMon, store)
		restore = restoreOnce(fanCtrl.RestoreAutoMode)
		go runControlLoop(fanCtrl.Run, restore, errCh)
	}

	// Initialize API server
	apiServer := api.NewServer(cfg, fanCtrl, store)

	// Start API server. On error, report through errCh instead of os.Exit so the
	// shutdown path (which restores BMC auto mode) still runs.
	go func() {
		if err := apiServer.Run(); err != nil {
			errCh <- fmt.Errorf("API server error: %w", err)
		}
	}()

	log.Printf("API server listening on %s:%d", cfg.API.Host, cfg.API.Port)
	log.Printf("Dashboard: http://localhost:%d/dashboard/", cfg.API.Port)

	// Wait for a shutdown signal or a fatal error.
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	exitCode := 0
	select {
	case <-sigChan:
		log.Println("Shutting down...")
	case err := <-errCh:
		log.Printf("Fatal: %v", err)
		exitCode = 1
	}

	fanCtrl.Stop()

	// Restore BMC automatic fan control on every exit path. This is the single
	// choke point that keeps the machine cooled once we leave manual mode.
	if err := restore(); err != nil {
		log.Printf("Warning: Failed to restore auto fan mode: %v", err)
	}

	// Stop the history cleanup goroutine before the deferred store.Close() runs.
	// Bounded: if a Cleanup query happens to be in flight, don't let it stall
	// exit indefinitely (thermal safety already happened above, in restore()).
	close(cleanupStop)
	select {
	case <-cleanupDone:
	case <-time.After(cleanupShutdownTimeout):
		// Abandoning it is safe: an in-flight Cleanup Exec racing the deferred
		// store.Close() is benign — database/sql won't force-interrupt a
		// checked-out connection, an interrupted DELETE rolls back via SQLite's
		// journal, and the process exits immediately after this anyway.
		log.Printf("Warning: history cleanup goroutine did not stop within %s; abandoning it", cleanupShutdownTimeout)
	}

	log.Println("Shutdown complete")
	return exitCode
}
