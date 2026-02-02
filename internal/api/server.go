package api

import (
	"embed"
	"fmt"
	"io/fs"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/sethpjohnson/only-fan-controller/internal/config"
	"github.com/sethpjohnson/only-fan-controller/internal/controller"
	"github.com/sethpjohnson/only-fan-controller/internal/storage"
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

	s.setupRoutes()
	return s
}

func (s *Server) setupRoutes() {
	// API routes
	api := s.router.Group("/api")
	{
		api.GET("/status", s.handleStatus)
		api.GET("/history", s.handleHistory)
		api.POST("/hint", s.handleHint)
		api.DELETE("/hint/:source", s.handleRemoveHint)
		api.POST("/override", s.handleOverride)
		api.DELETE("/override", s.handleClearOverride)
		api.GET("/config", s.handleGetConfig)
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

	if req.Speed < 0 || req.Speed > 100 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "speed must be 0-100"})
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
		"idrac_host":    s.cfg.IDRAC.Host,
		"gpu_enabled":   s.cfg.GPU.Enabled,
		"interval":      s.cfg.Monitoring.Interval,
		"zones":         s.cfg.Zones,
		"fan_control":   s.cfg.FanControl,
		"api_port":      s.cfg.API.Port,
	})
}
