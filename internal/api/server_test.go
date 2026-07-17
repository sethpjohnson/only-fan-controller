package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/sethpjohnson/only-fan-controller/internal/config"
	"github.com/sethpjohnson/only-fan-controller/internal/controller"
	"github.com/sethpjohnson/only-fan-controller/internal/validate"
)

func init() { gin.SetMode(gin.TestMode) }

// newTestServer builds a Server backed by a hardware-free controller. The
// controller's SetOverride/AddHint/GetStatus paths touch no external commands,
// so nil monitors and a nil store are safe for the endpoints exercised here.
func newTestServer(t *testing.T, token string) *Server {
	t.Helper()
	cfg := config.Default()
	cfg.API.Token = token
	cfg.Dashboard.Enabled = false
	ctrl := controller.NewFanController(cfg, nil, nil, nil)
	return NewServer(cfg, ctrl, nil)
}

// TestGetConfigDoesNotLeakMQTTPassword locks in that /api/config never exposes
// the MQTT broker password (MQTTConfig.Password carries json:"-", and
// handleGetConfig builds a hand-picked map that omits the mqtt section entirely).
func TestGetConfigDoesNotLeakMQTTPassword(t *testing.T) {
	cfg := config.Default()
	cfg.Dashboard.Enabled = false
	cfg.MQTT.Enabled = true
	cfg.MQTT.Broker = "tcp://10.0.0.5:1883"
	cfg.MQTT.Password = "super-secret-broker-pw"
	ctrl := controller.NewFanController(cfg, nil, nil, nil)
	srv := NewServer(cfg, ctrl, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	rec := httptest.NewRecorder()
	srv.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/config = %d, want 200", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "super-secret-broker-pw") {
		t.Fatalf("/api/config leaked mqtt.password: %s", rec.Body.String())
	}
}

// doRequest issues a request against the server's router. A non-empty token is
// sent as a bearer header; a non-empty remoteAddr overrides the connection peer
// (httptest defaults to a non-loopback TEST-NET address).
func doRequest(s *Server, method, path, token, remoteAddr string, body []byte) *httptest.ResponseRecorder {
	var req *http.Request
	if body != nil {
		req = httptest.NewRequest(method, path, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if remoteAddr != "" {
		req.RemoteAddr = remoteAddr
	}
	w := httptest.NewRecorder()
	s.router.ServeHTTP(w, req)
	return w
}

const validOverrideBody = `{"speed":50,"duration":60,"reason":"test"}`
const validHintBody = `{"type":"gpu_load","action":"start","intensity":"high","source":"whisper"}`

// mutatingRoutes lists every route registered inside the requireAuth group.
// Parameterizing the auth-middleware tests across all of them (rather than
// just POST /api/override) means a future mutating route registered outside
// the mutate group -- and therefore missing auth -- gets caught by these
// tests instead of shipping unprotected.
var mutatingRoutes = []struct {
	name   string
	method string
	path   string
	body   []byte
}{
	{"POST /api/hint", http.MethodPost, "/api/hint", []byte(validHintBody)},
	{"DELETE /api/hint/:source", http.MethodDelete, "/api/hint/whisper", nil},
	{"POST /api/override", http.MethodPost, "/api/override", []byte(validOverrideBody)},
	{"DELETE /api/override", http.MethodDelete, "/api/override", nil},
}

func TestMutatingRequiresTokenWhenConfigured(t *testing.T) {
	for _, route := range mutatingRoutes {
		t.Run(route.name, func(t *testing.T) {
			s := newTestServer(t, "s3cret")

			cases := []struct {
				name  string
				token string
				want  int
			}{
				{"no token", "", http.StatusUnauthorized},
				{"wrong token", "nope", http.StatusUnauthorized},
				{"correct token", "s3cret", http.StatusOK},
			}
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					// Use a non-loopback peer so only the token can grant access.
					w := doRequest(s, route.method, route.path, tc.token, "203.0.113.7:5555", route.body)
					if w.Code != tc.want {
						t.Fatalf("%s %s: got %d, want %d (body: %s)", route.method, route.path, w.Code, tc.want, w.Body.String())
					}
				})
			}
		})
	}
}

func TestReadOnlyEndpointsStayOpen(t *testing.T) {
	s := newTestServer(t, "s3cret")
	for _, path := range []string{"/api/status", "/api/config"} {
		w := doRequest(s, http.MethodGet, path, "", "203.0.113.7:5555", nil)
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s should be open: got %d", path, w.Code)
		}
	}
}

func TestConfigNeverLeaksToken(t *testing.T) {
	s := newTestServer(t, "s3cret")
	w := doRequest(s, http.MethodGet, "/api/config", "", "203.0.113.7:5555", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/config: got %d", w.Code)
	}
	if bytes.Contains(w.Body.Bytes(), []byte("s3cret")) {
		t.Fatalf("/api/config leaked the api token: %s", w.Body.String())
	}
}

func TestNoTokenLoopbackOnly(t *testing.T) {
	for _, route := range mutatingRoutes {
		t.Run(route.name, func(t *testing.T) {
			s := newTestServer(t, "") // no token configured

			cases := []struct {
				name       string
				remoteAddr string
				want       int
			}{
				{"non-loopback rejected", "203.0.113.9:5000", http.StatusForbidden},
				{"ipv4 loopback allowed", "127.0.0.1:5000", http.StatusOK},
				{"ipv6 loopback allowed", "[::1]:5000", http.StatusOK},
			}
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					w := doRequest(s, route.method, route.path, "", tc.remoteAddr, route.body)
					if w.Code != tc.want {
						t.Fatalf("%s %s from %s: got %d, want %d", route.method, route.path, tc.remoteAddr, w.Code, tc.want)
					}
				})
			}
		})
	}
}

// A spoofed X-Forwarded-For must not turn a LAN request into an accepted one:
// loopback is decided from the real connection peer only.
func TestForwardedHeaderCannotSpoofLoopback(t *testing.T) {
	s := newTestServer(t, "")
	req := httptest.NewRequest(http.MethodPost, "/api/override", bytes.NewReader([]byte(validOverrideBody)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-For", "127.0.0.1")
	req.Header.Set("X-Real-IP", "127.0.0.1")
	req.RemoteAddr = "203.0.113.9:5000" // real peer is off-host
	w := httptest.NewRecorder()
	s.router.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("spoofed forwarded headers granted access: got %d, want %d", w.Code, http.StatusForbidden)
	}
}

func TestHintValidation(t *testing.T) {
	s := newTestServer(t, "") // loopback path used below

	cases := []struct {
		name string
		body string
		want int
	}{
		{"valid hint", `{"type":"gpu_load","action":"start","intensity":"high","source":"whisper"}`, http.StatusOK},
		{"xss source rejected", `{"type":"gpu_load","action":"start","intensity":"high","source":"<img src=x onerror=alert(1)>"}`, http.StatusBadRequest},
		{"bad type rejected", `{"type":"gpu load!","action":"start","source":"whisper"}`, http.StatusBadRequest},
		{"bad intensity rejected", `{"type":"gpu_load","action":"start","intensity":"EXTREME","source":"whisper"}`, http.StatusBadRequest},
		{"bad action rejected", `{"type":"gpu_load","action":"launch","source":"whisper"}`, http.StatusBadRequest},
		{"overlong source rejected", `{"type":"gpu_load","action":"start","source":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := doRequest(s, http.MethodPost, "/api/hint", "", "127.0.0.1:4000", []byte(tc.body))
			if w.Code != tc.want {
				t.Fatalf("POST /api/hint (%s): got %d, want %d (body: %s)", tc.name, w.Code, tc.want, w.Body.String())
			}
		})
	}
}

// Override reasons are human-readable free text (unlike hint source/type),
// so quotes, spaces, and punctuation must all be accepted -- the hint-client
// script sends reasons containing quotes -- while control characters and
// excessive length are still rejected server-side.
func TestOverrideReasonValidation(t *testing.T) {
	s := newTestServer(t, "") // loopback path used below

	type overrideBody struct {
		Speed    int    `json:"speed"`
		Duration int    `json:"duration"`
		Reason   string `json:"reason"`
	}

	cases := []struct {
		name   string
		reason string
		want   int
	}{
		{"normal free text", "manual override for benchmark", http.StatusOK},
		{"quotes and punctuation allowed", `user said "too loud", quiet it down!`, http.StatusOK},
		{"empty reason allowed", "", http.StatusOK},
		{"newline rejected", "line1\nline2", http.StatusBadRequest},
		{"tab rejected", "reason\twith\ttab", http.StatusBadRequest},
		{"overlong reason rejected", strings.Repeat("a", validate.MaxOverrideReasonLen+1), http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body, err := json.Marshal(overrideBody{Speed: 50, Duration: 60, Reason: tc.reason})
			if err != nil {
				t.Fatalf("marshal body: %v", err)
			}
			w := doRequest(s, http.MethodPost, "/api/override", "", "127.0.0.1:4000", body)
			if w.Code != tc.want {
				t.Fatalf("POST /api/override (%s): got %d, want %d (body: %s)", tc.name, w.Code, tc.want, w.Body.String())
			}
		})
	}
}

// An override posted through the API must be clamped to the configured band and
// carry a finite expiry (never infinite), reflected in /api/status.
func TestOverrideClampedViaAPI(t *testing.T) {
	s := newTestServer(t, "")
	s.cfg.FanControl.MinSpeed = 20
	s.cfg.FanControl.MaxSpeed = 80

	body := []byte(`{"speed":100,"duration":0,"reason":"pin high"}`)
	w := doRequest(s, http.MethodPost, "/api/override", "", "127.0.0.1:4000", body)
	if w.Code != http.StatusOK {
		t.Fatalf("POST /api/override: got %d, want 200", w.Code)
	}

	w = doRequest(s, http.MethodGet, "/api/status", "", "127.0.0.1:4000", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/status: got %d", w.Code)
	}
	var status struct {
		Override *struct {
			Speed     int    `json:"speed"`
			ExpiresAt string `json:"expires_at"`
		} `json:"override"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if status.Override == nil {
		t.Fatal("expected an active override in status")
	}
	if status.Override.Speed != 80 {
		t.Fatalf("override speed not clamped to max: got %d, want 80", status.Override.Speed)
	}
	if status.Override.ExpiresAt == "" || status.Override.ExpiresAt == "0001-01-01T00:00:00Z" {
		t.Fatalf("override expiry must be finite (not infinite), got %q", status.Override.ExpiresAt)
	}
}
