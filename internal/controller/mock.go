package controller

import (
	"log"
	"time"

	"github.com/sethpjohnson/only-fan-controller/internal/config"
	"github.com/sethpjohnson/only-fan-controller/internal/monitor"
	"github.com/sethpjohnson/only-fan-controller/internal/storage"
)

// MockFanController is a fan controller that doesn't actually control fans
// Used for testing and development
type MockFanController struct {
	*FanController
}

// NewMockFanController creates a controller that simulates fan control
func NewMockFanController(cfg *config.Config, store *storage.Store) *MockFanController {
	// Create base controller with nil monitors (we'll override the control loop)
	fc := &FanController{
		cfg:        cfg,
		cpuMon:     nil,
		gpuMon:     nil,
		store:      store,
		hints:      make(map[string]*WorkloadHint),
		stopChan:   make(chan struct{}),
		cpuHistory: make([]tempPoint, 0),
		gpuHistory: make([]tempPoint, 0),
	}

	return &MockFanController{
		FanController: fc,
	}
}

func (mfc *MockFanController) Run() {
	mfc.mu.Lock()
	mfc.running = true
	mfc.mu.Unlock()

	log.Println("[MOCK] Fan controller starting in demo mode")
	log.Println("[MOCK] No actual fan control - simulated temperatures")

	ticker := time.NewTicker(time.Duration(mfc.cfg.Monitoring.Interval) * time.Second)
	defer ticker.Stop()

	// Run immediately
	mfc.mockControlLoop()

	for {
		select {
		case <-ticker.C:
			mfc.mockControlLoop()
		case <-mfc.stopChan:
			return
		}
	}
}

// Mock state
var mockCPUBase = 42.0
var mockGPUBases = []float64{38.0, 36.0}

func (mfc *MockFanController) mockControlLoop() {
	// Generate simulated temperatures
	cpuReading := mfc.generateMockCPU()
	gpuReading := mfc.generateMockGPU()

	mfc.mu.Lock()
	mfc.lastCPUReading = cpuReading
	mfc.lastGPUReading = gpuReading

	now := time.Now()
	mfc.cpuHistory = append(mfc.cpuHistory, tempPoint{temp: cpuReading.Max, timestamp: now})
	mfc.gpuHistory = append(mfc.gpuHistory, tempPoint{temp: gpuReading.Max, timestamp: now})
	mfc.trimHistory()
	mfc.cleanExpired()
	mfc.mu.Unlock()

	// Calculate what fan speed would be
	target := mfc.calculateTarget(cpuReading, gpuReading)

	// Just update internal state (no actual IPMI commands)
	mfc.mu.Lock()
	mfc.currentSpeed = target
	mfc.mu.Unlock()

	// Store reading
	mfc.store.RecordReading(cpuReading.Max, gpuReading.Max, target)

	log.Printf("[MOCK] CPU: %d°C | GPU: %d°C | Zone: %s | Fan: %d%%",
		cpuReading.Max, gpuReading.Max, mfc.currentZone, target)
}

func (mfc *MockFanController) generateMockCPU() *monitor.CPUReading {
	// Check if there are active hints (simulate load causing heat)
	mfc.mu.RLock()
	hasHints := len(mfc.hints) > 0
	mfc.mu.RUnlock()

	if hasHints {
		mockCPUBase += 0.3 // Rising
	} else {
		mockCPUBase -= 0.1 // Cooling
	}
	
	if mockCPUBase < 35 {
		mockCPUBase = 35
	}
	if mockCPUBase > 80 {
		mockCPUBase = 80
	}

	temp1 := int(mockCPUBase) + (time.Now().Second() % 3)
	temp2 := int(mockCPUBase) + ((time.Now().Second() + 1) % 3)

	return &monitor.CPUReading{
		Temps: []int{temp1, temp2},
		Max:   max(temp1, temp2),
	}
}

func (mfc *MockFanController) generateMockGPU() *monitor.GPUReading {
	mfc.mu.RLock()
	hasHints := len(mfc.hints) > 0
	mfc.mu.RUnlock()

	devices := make([]monitor.GPUDevice, 2)
	maxTemp := 0

	for i := range mockGPUBases {
		if hasHints {
			mockGPUBases[i] += 0.5 // Heat up under load
		} else {
			mockGPUBases[i] -= 0.15 // Cool down
		}
		if mockGPUBases[i] < 32 {
			mockGPUBases[i] = 32
		}
		if mockGPUBases[i] > 85 {
			mockGPUBases[i] = 85
		}

		temp := int(mockGPUBases[i]) + (time.Now().Second() % 2)
		if temp > maxTemp {
			maxTemp = temp
		}

		util := 0
		power := 45
		if hasHints {
			util = 85 + (time.Now().Second() % 15)
			power = 220 + (time.Now().Second() % 30)
		}

		devices[i] = monitor.GPUDevice{
			Index:       i,
			Name:        "Tesla P40 (Mock)",
			Temp:        temp,
			Utilization: util,
			MemoryUsed:  2048 + (time.Now().Second() * 10),
			MemoryTotal: 24576,
			PowerDraw:   power,
		}
	}

	return &monitor.GPUReading{
		Devices: devices,
		Max:     maxTemp,
	}
}

func (mfc *MockFanController) RestoreAutoMode() error {
	log.Println("[MOCK] Would restore Dell automatic fan control")
	return nil
}
