package monitor

import (
	"math"
	"math/rand"
	"time"
)

// MockCPUMonitor simulates CPU temperatures for testing
type MockCPUMonitor struct {
	baseTemp   float64
	lastUpdate time.Time
	trend      float64
}

func NewMockCPUMonitor() *MockCPUMonitor {
	return &MockCPUMonitor{
		baseTemp:   40.0,
		lastUpdate: time.Now(),
		trend:      0,
	}
}

func (m *MockCPUMonitor) Read() (*CPUReading, error) {
	now := time.Now()
	elapsed := now.Sub(m.lastUpdate).Seconds()
	m.lastUpdate = now

	// Slowly drift temperature with some randomness
	m.trend += (rand.Float64() - 0.5) * 0.1
	m.trend = math.Max(-0.5, math.Min(0.5, m.trend)) // Clamp trend
	
	m.baseTemp += m.trend * elapsed
	m.baseTemp = math.Max(30, math.Min(75, m.baseTemp)) // Clamp temp

	// Generate 2 CPU temps with slight variation
	temp1 := int(m.baseTemp + rand.Float64()*3)
	temp2 := int(m.baseTemp + rand.Float64()*3)

	return &CPUReading{
		Temps: []int{temp1, temp2},
		Max:   maxInt([]int{temp1, temp2}),
	}, nil
}

// MockGPUMonitor simulates GPU temperatures for testing
type MockGPUMonitor struct {
	baseTemps  []float64
	lastUpdate time.Time
	loadActive bool
}

func NewMockGPUMonitor(numGPUs int) *MockGPUMonitor {
	temps := make([]float64, numGPUs)
	for i := range temps {
		temps[i] = 35.0 + rand.Float64()*5
	}
	return &MockGPUMonitor{
		baseTemps:  temps,
		lastUpdate: time.Now(),
	}
}

func (m *MockGPUMonitor) Read() (*GPUReading, error) {
	now := time.Now()
	elapsed := now.Sub(m.lastUpdate).Seconds()
	m.lastUpdate = now

	devices := make([]GPUDevice, len(m.baseTemps))
	maxTemp := 0

	for i := range m.baseTemps {
		// If load is active, temps rise; otherwise they fall
		if m.loadActive {
			m.baseTemps[i] += 0.5 * elapsed
		} else {
			m.baseTemps[i] -= 0.2 * elapsed
		}
		m.baseTemps[i] = math.Max(30, math.Min(85, m.baseTemps[i]))

		temp := int(m.baseTemps[i] + rand.Float64()*2)
		if temp > maxTemp {
			maxTemp = temp
		}

		util := 0
		power := 50
		if m.loadActive {
			util = 80 + rand.Intn(20)
			power = 200 + rand.Intn(50)
		}

		devices[i] = GPUDevice{
			Index:       i,
			Name:        "Mock Tesla P40",
			Temp:        temp,
			Utilization: util,
			MemoryUsed:  1024 + rand.Intn(2048),
			MemoryTotal: 24576,
			PowerDraw:   power,
		}
	}

	return &GPUReading{
		Devices: devices,
		Max:     maxTemp,
	}, nil
}

func (m *MockGPUMonitor) SetLoad(active bool) {
	m.loadActive = active
}

func (m *MockGPUMonitor) IsAvailable() bool {
	return true
}
