package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestResolveConfigMissingFileFallsBackToDefaults(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist.yaml")
	cfg, err := resolveConfig(missing)
	if err != nil {
		t.Fatalf("missing config file should fall back to defaults, got error: %v", err)
	}
	if cfg == nil {
		t.Fatal("expected default config, got nil")
	}
}

func TestResolveConfigInvalidFileRefusesToStart(t *testing.T) {
	// Valid YAML with REAL iDRAC credentials, but a safety-validation failure
	// (critical_cpu_temp below the customized cpu_threshold). resolveConfig must
	// NOT silently fall back to Default() (which would discard these credentials)
	// — it must return an error so the process refuses to start.
	path := filepath.Join(t.TempDir(), "config.yaml")
	content := `idrac:
  host: "192.168.1.50"
  username: "operator"
  password: "s3cret"
fan_control:
  cpu_threshold: 90
  critical_cpu_temp: 85
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("failed to write temp config: %v", err)
	}

	cfg, err := resolveConfig(path)
	if err == nil {
		t.Fatalf("expected refusal on invalid config, got cfg=%+v", cfg)
	}
	if cfg != nil {
		t.Fatalf("invalid config must not yield a usable (fallback) config, got %+v", cfg)
	}
}

func TestRunControlLoopRestoresAutoModeOnPanic(t *testing.T) {
	restored := make(chan struct{}, 1)
	errCh := make(chan error, 1)

	run := func() { panic("boom in control loop") }
	restore := func() error {
		restored <- struct{}{}
		return nil
	}

	runControlLoop(run, restore, errCh)

	select {
	case <-restored:
	default:
		t.Fatal("RestoreAutoMode was not called when the control loop panicked")
	}

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("expected a non-nil error on the error channel after panic")
		}
	case <-time.After(time.Second):
		t.Fatal("no error reported on the error channel after panic")
	}
}

func TestRunControlLoopReportsRestoreFailureButStillExits(t *testing.T) {
	errCh := make(chan error, 1)
	run := func() { panic("boom") }
	restore := func() error { return errors.New("ipmitool unreachable") }

	// Must not itself panic even if restore fails.
	runControlLoop(run, restore, errCh)

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("expected an error after panic")
		}
	default:
		t.Fatal("expected an error to be reported after panic")
	}
}

func TestRestoreOnceFiresUnderlyingExactlyOnce(t *testing.T) {
	calls := 0
	restore := restoreOnce(func() error {
		calls++
		return nil
	})

	// Simulate the panic path (recover restores) followed by the shutdown path
	// (run() restores again). The underlying BMC toggle must happen once.
	_ = restore()
	_ = restore()
	_ = restore()

	if calls != 1 {
		t.Fatalf("underlying restore called %d times, want 1", calls)
	}
}

func TestRunControlLoopNormalReturnDoesNotReportError(t *testing.T) {
	errCh := make(chan error, 1)
	run := func() {} // returns normally
	restore := func() error {
		t.Fatal("restore must not be called on a normal return")
		return nil
	}

	runControlLoop(run, restore, errCh)

	select {
	case err := <-errCh:
		t.Fatalf("unexpected error on normal return: %v", err)
	default:
	}
}
