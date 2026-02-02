package monitor

import (
	"bytes"
	"encoding/csv"
	"log"
	"os/exec"
	"strconv"
	"strings"

	"github.com/sethpjohnson/only-fan-controller/internal/config"
)

type GPUMonitor struct {
	cfg *config.Config
}

type GPUDevice struct {
	Index       int    `json:"index"`
	Name        string `json:"name"`
	Temp        int    `json:"temp"`
	Utilization int    `json:"utilization"`
	MemoryUsed  int    `json:"memory_used"`   // MB
	MemoryTotal int    `json:"memory_total"`  // MB
	PowerDraw   int    `json:"power_draw"`    // Watts
}

type GPUReading struct {
	Devices []GPUDevice
	Max     int
}

func NewGPUMonitor(cfg *config.Config) *GPUMonitor {
	return &GPUMonitor{cfg: cfg}
}

// Read gets current GPU temperatures and stats via nvidia-smi
func (m *GPUMonitor) Read() (*GPUReading, error) {
	if !m.cfg.GPU.Enabled {
		return &GPUReading{}, nil
	}

	// Query nvidia-smi for key metrics in CSV format
	cmd := exec.Command(m.cfg.GPU.NvidiaSmiPath,
		"--query-gpu=index,name,temperature.gpu,utilization.gpu,memory.used,memory.total,power.draw",
		"--format=csv,noheader,nounits",
	)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		log.Printf("nvidia-smi error: %v, stderr: %s", err, stderr.String())
		return nil, err
	}

	devices := parseGPUOutput(stdout.String())
	
	reading := &GPUReading{
		Devices: devices,
		Max:     maxGPUTemp(devices),
	}

	return reading, nil
}

// parseGPUOutput parses nvidia-smi CSV output
func parseGPUOutput(output string) []GPUDevice {
	var devices []GPUDevice

	reader := csv.NewReader(strings.NewReader(output))
	records, err := reader.ReadAll()
	if err != nil {
		log.Printf("Failed to parse nvidia-smi output: %v", err)
		return devices
	}

	for _, record := range records {
		if len(record) < 7 {
			continue
		}

		// Clean up whitespace from CSV fields
		for i := range record {
			record[i] = strings.TrimSpace(record[i])
		}

		device := GPUDevice{
			Index:       parseInt(record[0]),
			Name:        record[1],
			Temp:        parseInt(record[2]),
			Utilization: parseInt(record[3]),
			MemoryUsed:  parseInt(record[4]),
			MemoryTotal: parseInt(record[5]),
			PowerDraw:   parseInt(record[6]),
		}

		devices = append(devices, device)
	}

	return devices
}

func parseInt(s string) int {
	// Handle "N/A" or empty values
	s = strings.TrimSpace(s)
	if s == "" || s == "[N/A]" || s == "N/A" {
		return 0
	}
	
	// Remove any decimal part (e.g., "45.00" -> "45")
	if idx := strings.Index(s, "."); idx != -1 {
		s = s[:idx]
	}
	
	val, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return val
}

func maxGPUTemp(devices []GPUDevice) int {
	max := 0
	for _, d := range devices {
		if d.Temp > max {
			max = d.Temp
		}
	}
	return max
}

// IsAvailable checks if nvidia-smi is available
func (m *GPUMonitor) IsAvailable() bool {
	cmd := exec.Command(m.cfg.GPU.NvidiaSmiPath, "--version")
	return cmd.Run() == nil
}
