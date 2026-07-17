package monitor

import "testing"

func TestParseCPUTemps(t *testing.T) {
	tests := []struct {
		name    string
		output  string
		want    []int
		wantErr bool
	}{
		{
			name: "valid R730 output",
			output: `Inlet Temp       | 04h | ok  |  7.1 | 20 degrees C
Exhaust Temp     | 01h | ok  |  7.1 | 28 degrees C
Temp             | 0Eh | ok  |  3.1 | 33 degrees C
Temp             | 0Fh | ok  |  3.2 | 35 degrees C`,
			want:    []int{33, 35},
			wantErr: false,
		},
		{
			name:    "empty output is an error, never 0C",
			output:  "",
			want:    nil,
			wantErr: true,
		},
		{
			name:    "garbage output is an error",
			output:  "this is not ipmitool output at all\nrandom text",
			want:    nil,
			wantErr: true,
		},
		{
			name: "only inlet and exhaust temps yields no CPU temps",
			output: `Inlet Temp       | 04h | ok  |  7.1 | 20 degrees C
Exhaust Temp     | 01h | ok  |  7.1 | 28 degrees C`,
			want:    nil,
			wantErr: true,
		},
		{
			name: "firmware update changed labels but temps still present via fallback",
			output: `CPU1 Temp        | 0Eh | ok  |  3.1 | 41 degrees C
CPU2 Temp        | 0Fh | ok  |  3.2 | 44 degrees C`,
			want:    []int{41, 44},
			wantErr: false,
		},
		{
			name: "out-of-range readings are ignored and remaining empty is an error",
			output: `Temp             | 0Eh | ok  |  3.1 | 0 degrees C
Temp             | 0Fh | ok  |  3.2 | 999 degrees C`,
			want:    nil,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseCPUTemps(tt.output)
			if tt.wantErr && err == nil {
				t.Fatalf("expected error, got nil (temps=%v)", got)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !equalInts(got, tt.want) {
				t.Fatalf("temps = %v, want %v", got, tt.want)
			}
		})
	}
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
