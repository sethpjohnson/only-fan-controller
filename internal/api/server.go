package api

import (
	"crypto/subtle"
	"embed"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/sethpjohnson/only-fan-controller/internal/config"
	"github.com/sethpjohnson/only-fan-controller/internal/controller"
	"github.com/sethpjohnson/only-fan-controller/internal/storage"
	"github.com/sethpjohnson/only-fan-controller/internal/validate"
)

//go:embed static/*
var staticFiles embed.FS

type Server struct {
	cfg    *config.Config
	ctrl   *controller.FanController
	store  *storage.Store
	router *gin.Engine
}

type HintRequest struct {
	Type             string `json:"type" binding:"required"`
	Action           string `json:"action" binding:"required"`
	Intensity        string `json:"intensity"`
	DurationEstimate int    `json:"duration_estimate"` // seconds
	Source           string `json:"source" binding:"required"`
}

type OverrideRequest struct {
	Speed    int    `json:"speed" binding:"required"`
	Duration int    `json:"duration"` // seconds, 0 = indefinite
	Reason   string `json:"reason"`
}

func NewServer(cfg *config.Config, ctrl *controller.FanController, store *storage.Store) *Server {
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	router.Use(gin.Recovery())

	s := &Server{
		cfg:    cfg,
		ctrl:   ctrl,
		store:  store,
		router: router,
	}

	if cfg.API.Token == "" {
		log.Println("WARNING: no api.token configured (env API_TOKEN); mutating endpoints " +
			"(override/hint) are restricted to loopback only. Set a token to control fans from other LAN hosts.")
	}

	s.setupRoutes()
	return s
}

func (s *Server) setupRoutes() {
	// API routes
	api := s.router.Group("/api")
	{
		// Read-only endpoints stay open: they expose no control surface.
		api.GET("/status", s.handleStatus)
		api.GET("/history", s.handleHistory)
		api.GET("/config", s.handleGetConfig)

		// Mutating endpoints are gated by requireAuth (bearer token, or loopback
		// when no token is configured).
		mutate := api.Group("", s.requireAuth())
		{
			mutate.POST("/hint", s.handleHint)
			mutate.DELETE("/hint/:source", s.handleRemoveHint)
			mutate.POST("/override", s.handleOverride)
			mutate.DELETE("/override", s.handleClearOverride)
		}
	}

	// Dashboard static files
	if s.cfg.Dashboard.Enabled {
		staticFS, err := fs.Sub(staticFiles, "static")
		if err == nil {
			s.router.StaticFS("/dashboard", http.FS(staticFS))
			s.router.GET("/", func(c *gin.Context) {
				c.Redirect(http.StatusMovedPermanently, "/dashboard/")
			})
		}
	}
}

func (s *Server) Run() error {
	addr := fmt.Sprintf("%s:%d", s.cfg.API.Host, s.cfg.API.Port)
	return s.router.Run(addr)
}

// requireAuth guards the mutating endpoints.
//
//   - When a token is configured, the request must carry a matching
//     "Authorization: Bearer <token>" header (compared in constant time). Any
//     other case is 401.
//   - When no token is configured, only requests whose connection peer is a
//     loopback address are accepted (403 otherwise). This preserves single-user,
//     same-host convenience without silently exposing fan control to the LAN.
//
// Loopback is decided from the real connection peer (c.Request.RemoteAddr), NOT
// from X-Forwarded-For / X-Real-IP: those are client-supplied and trivially
// spoofable, so trusting them would defeat the check.
func (s *Server) requireAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		token := s.cfg.API.Token

		if token == "" {
			if !isLoopbackAddr(c.Request.RemoteAddr) {
				c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
					"error": "mutating endpoints require a loopback connection or a configured api.token",
				})
				return
			}
			c.Next()
			return
		}

		if !bearerTokenMatches(c.GetHeader("Authorization"), token) {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"error": "missing or invalid bearer token",
			})
			return
		}
		c.Next()
	}
}

// isLoopbackAddr reports whether a net.Conn RemoteAddr string ("host:port")
// refers to a loopback address.
func isLoopbackAddr(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		// RemoteAddr is normally host:port; fall back to treating the whole
		// string as a bare host so an unexpected format fails closed via ParseIP.
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// bearerTokenMatches reports whether an Authorization header carries a bearer
// token equal to expected. The comparison is constant-time to avoid leaking the
// token via response timing.
func bearerTokenMatches(header, expected string) bool {
	got, ok := strings.CutPrefix(header, "Bearer ")
	if !ok {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(expected)) == 1
}

// validateHintRequest enforces length, charset, and closed-set bounds on the
// client-controlled hint fields before they are stored or echoed back, using the
// shared validate package so the HTTP and MQTT surfaces agree.
func validateHintRequest(req *HintRequest) error {
	if err := validate.HintField("source", req.Source); err != nil {
		return err
	}
	if err := validate.HintField("type", req.Type); err != nil {
		return err
	}
	if err := validate.HintAction(req.Action); err != nil {
		return err
	}
	if err := validate.Intensity(req.Intensity); err != nil {
		return err
	}
	if req.DurationEstimate < 0 {
		return fmt.Errorf("duration_estimate must not be negative")
	}
	return nil
}

// GET /api/status
func (s *Server) handleStatus(c *gin.Context) {
	status := s.ctrl.GetStatus()
	c.JSON(http.StatusOK, status)
}

// GET /api/history?duration=3600
func (s *Server) handleHistory(c *gin.Context) {
	durationStr := c.DefaultQuery("duration", "3600")
	durationSec, err := strconv.Atoi(durationStr)
	if err != nil {
		durationSec = 3600
	}

	history, err := s.store.GetHistory(time.Duration(durationSec) * time.Second)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"duration": durationSec,
		"count":    len(history),
		"data":     history,
	})
}

// POST /api/hint
func (s *Server) handleHint(c *gin.Context) {
	var req HintRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if err := validateHintRequest(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if req.Action == "stop" {
		s.ctrl.RemoveHint(req.Source)
		c.JSON(http.StatusOK, gin.H{"status": "hint removed", "source": req.Source})
		return
	}

	hint := &controller.WorkloadHint{
		Type:      req.Type,
		Action:    req.Action,
		Intensity: req.Intensity,
		Source:    req.Source,
	}

	if req.DurationEstimate > 0 {
		hint.ExpiresAt = time.Now().Add(time.Duration(req.DurationEstimate) * time.Second)
	}

	s.ctrl.AddHint(hint)
	c.JSON(http.StatusOK, gin.H{"status": "hint registered", "hint": hint})
}

// DELETE /api/hint/:source
func (s *Server) handleRemoveHint(c *gin.Context) {
	source := c.Param("source")
	s.ctrl.RemoveHint(source)
	c.JSON(http.StatusOK, gin.H{"status": "hint removed", "source": source})
}

// POST /api/override
func (s *Server) handleOverride(c *gin.Context) {
	var req OverrideRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if err := validate.OverrideSpeed(req.Speed); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if err := validate.OverrideReason(req.Reason); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	duration := time.Duration(req.Duration) * time.Second
	s.ctrl.SetOverride(req.Speed, duration, req.Reason)

	c.JSON(http.StatusOK, gin.H{
		"status":   "override set",
		"speed":    req.Speed,
		"duration": req.Duration,
	})
}

// DELETE /api/override
func (s *Server) handleClearOverride(c *gin.Context) {
	s.ctrl.ClearOverride()
	c.JSON(http.StatusOK, gin.H{"status": "override cleared"})
}

// GET /api/config
func (s *Server) handleGetConfig(c *gin.Context) {
	// Return sanitized config (no passwords)
	c.JSON(http.StatusOK, gin.H{
		"idrac_host":  s.cfg.IDRAC.Host,
		"gpu_enabled": s.cfg.GPU.Enabled,
		"interval":    s.cfg.Monitoring.Interval,
		"zones":       s.cfg.Zones,
		"fan_control": s.cfg.FanControl,
		"api_port":    s.cfg.API.Port,
	})
}
