package mqtt

import "testing"

func TestNormalizeBrokerURL(t *testing.T) {
	tests := []struct {
		name     string
		in       string
		want     string
		wantNote bool // true if a transformation note is expected
	}{
		{"bare IPv4 defaults scheme and port", "192.168.1.10", "tcp://192.168.1.10:1883", true},
		{"host:port assumes tcp scheme", "192.168.1.10:1883", "tcp://192.168.1.10:1883", true},
		{"bare hostname defaults scheme and port", "broker.example.com", "tcp://broker.example.com:1883", true},
		{"http browser paste becomes tcp", "http://homeassistant.local:8123", "tcp://homeassistant.local:8123", true},
		{"https browser paste becomes tcp and defaults port", "https://broker.example.com", "tcp://broker.example.com:1883", true},
		{"canonical tcp is unchanged", "tcp://127.0.0.1:1883", "tcp://127.0.0.1:1883", false},
		{"canonical ssl is unchanged", "ssl://broker.example.com:8883", "ssl://broker.example.com:8883", false},
		{"canonical ws is unchanged", "ws://broker.example.com:9001", "ws://broker.example.com:9001", false},
		{"mqtt scheme becomes tcp", "mqtt://broker.example.com:1883", "tcp://broker.example.com:1883", true},
		{"mqtts scheme becomes ssl", "mqtts://broker.example.com:8883", "ssl://broker.example.com:8883", true},
		{"mqtts without port defaults to 8883", "mqtts://broker.example.com", "ssl://broker.example.com:8883", true},
		{"tls scheme becomes ssl", "tls://broker.example.com:8883", "ssl://broker.example.com:8883", true},
		{"ssl without port defaults to 8883", "ssl://broker.example.com", "ssl://broker.example.com:8883", true},
		{"uppercase scheme is lowercased", "TCP://broker.example.com:1883", "tcp://broker.example.com:1883", true},
		{"surrounding whitespace is trimmed", "  tcp://broker.example.com:1883  ", "tcp://broker.example.com:1883", true},
		{"trailing slash is stripped", "tcp://broker.example.com:1883/", "tcp://broker.example.com:1883", true},
		{"IPv6 literal with port is preserved", "[::1]:1883", "tcp://[::1]:1883", true},
		{"IPv6 literal without port defaults port", "[::1]", "tcp://[::1]:1883", true},
		{"bare IPv6 literal gets brackets", "fe80::1", "tcp://[fe80::1]:1883", true},
		{"canonical IPv6 tcp is unchanged", "tcp://[::1]:1883", "tcp://[::1]:1883", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, note := normalizeBrokerURL(tt.in)
			if got != tt.want {
				t.Errorf("normalizeBrokerURL(%q) = %q, want %q", tt.in, got, tt.want)
			}
			if (note != "") != tt.wantNote {
				t.Errorf("normalizeBrokerURL(%q) note = %q, wantNote=%v", tt.in, note, tt.wantNote)
			}
		})
	}
}

// TestNormalizeBrokerURLDefensive covers the never-crash contract: unparseable
// or empty input is returned unchanged with an explanatory note.
func TestNormalizeBrokerURLDefensive(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{"empty string", ""},
		{"whitespace only", "   "},
		{"unrecognized scheme", "carrier-pigeon://broker.example.com"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, note := normalizeBrokerURL(tt.in)
			if got != tt.in {
				t.Errorf("normalizeBrokerURL(%q) = %q, want input returned unchanged", tt.in, got)
			}
			if note == "" {
				t.Errorf("normalizeBrokerURL(%q) expected a note explaining it was left unchanged", tt.in)
			}
		})
	}
}
