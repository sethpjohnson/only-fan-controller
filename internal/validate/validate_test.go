package validate

import "testing"

func TestHintField(t *testing.T) {
	if err := HintField("source", "plex.transcode-1"); err != nil {
		t.Fatalf("valid source rejected: %v", err)
	}
	if err := HintField("source", "bad source!"); err == nil {
		t.Fatal("source with space/bang should be rejected")
	}
	if err := HintField("type", string(make([]byte, MaxHintFieldLen+1))); err == nil {
		t.Fatal("overlong type should be rejected")
	}
}

func TestHintAction(t *testing.T) {
	if err := HintAction("start"); err != nil {
		t.Fatalf("start rejected: %v", err)
	}
	if err := HintAction("stop"); err != nil {
		t.Fatalf("stop rejected: %v", err)
	}
	if err := HintAction("pause"); err == nil {
		t.Fatal("pause should be rejected")
	}
}

func TestIntensity(t *testing.T) {
	for _, ok := range []string{"", "low", "medium", "high"} {
		if err := Intensity(ok); err != nil {
			t.Fatalf("intensity %q rejected: %v", ok, err)
		}
	}
	if err := Intensity("extreme"); err == nil {
		t.Fatal("extreme should be rejected")
	}
}

func TestOverrideSpeed(t *testing.T) {
	for _, ok := range []int{0, 50, 100} {
		if err := OverrideSpeed(ok); err != nil {
			t.Fatalf("speed %d rejected: %v", ok, err)
		}
	}
	if err := OverrideSpeed(-1); err == nil {
		t.Fatal("negative speed should be rejected")
	}
	if err := OverrideSpeed(101); err == nil {
		t.Fatal("speed > 100 should be rejected")
	}
}

func TestOverrideReason(t *testing.T) {
	if err := OverrideReason("manual burn-in test (2h)"); err != nil {
		t.Fatalf("normal prose rejected: %v", err)
	}
	if err := OverrideReason("bad\x00null"); err == nil {
		t.Fatal("control character should be rejected")
	}
	if err := OverrideReason(string(make([]byte, MaxOverrideReasonLen+1))); err == nil {
		t.Fatal("overlong reason should be rejected")
	}
}
