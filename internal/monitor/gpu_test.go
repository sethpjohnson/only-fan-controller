package monitor

import "testing"

func TestParseGPUOutput(t *testing.T) {
	tests := []struct {
		name      string
		output    string
		wantTemps []int // temps of parsed devices, in order
		wantErr   bool
	}{
		{
			name:      "valid two-GPU CSV",
			output:    "0, Tesla P40, 45, 80, 2048, 24576, 220\n1, Tesla P40, 47, 85, 4096, 24576, 230\n",
			wantTemps: []int{45, 47},
			wantErr:   false,
		},
		{
			name:      "empty output is an error, never 0C",
			output:    "",
			wantTemps: nil,
			wantErr:   true,
		},
		{
			name:      "whitespace-only output is an error",
			output:    "   \n\n",
			wantTemps: nil,
			wantErr:   true,
		},
		{
			name:      "garbage / non-CSV output is an error",
			output:    "nvidia-smi: command not found",
			wantTemps: nil,
			wantErr:   true,
		},
		{
			name:      "partial rows (too few fields) are skipped; all-skipped is an error",
			output:    "0, Tesla P40, 45\n1, Tesla P40\n",
			wantTemps: nil,
			wantErr:   true,
		},
		{
			name:      "mixed valid and partial rows keeps the valid one",
			output:    "0, Tesla P40, 45\n1, Tesla P40, 50, 80, 4096, 24576, 230\n",
			wantTemps: []int{50},
			wantErr:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			devices, err := parseGPUOutput(tt.output)
			if tt.wantErr && err == nil {
				t.Fatalf("expected error, got nil (devices=%+v)", devices)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(devices) != len(tt.wantTemps) {
				t.Fatalf("device count = %d, want %d", len(devices), len(tt.wantTemps))
			}
			for i, want := range tt.wantTemps {
				if devices[i].Temp != want {
					t.Fatalf("device[%d].Temp = %d, want %d", i, devices[i].Temp, want)
				}
			}
		})
	}
}
