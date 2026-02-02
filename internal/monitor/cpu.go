package monitor

import (
	"bytes"
	"log"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"github.com/sethpjohnson/only-fan-controller/internal/config"
)

type CPUMonitor struct {
	cfg *config.Config
}

type CPUReading struct {
	Temps []int
	Max   int
}

func NewCPUMonitor(cfg *config.Config) *CPUMonitor {
	return &CPUMonitor{cfg: cfg}
}

// Read gets current CPU temperatures via IPMI
func (m *CPUMonitor) Read() (*CPUReading, error) {
	var cmd *exec.Cmd

	if m.cfg.IDRAC.Host == "local" {
		// Local IPMI access
		cmd = exec.Command("ipmitool", "sdr", "type", "temperature")
	} else {
		// Remote IPMI access
		cmd = exec.Command("ipmitool",
			"-I", "lanplus",
			"-H", m.cfg.IDRAC.Host,
			"-U", m.cfg.IDRAC.Username,
			"-P", m.cfg.IDRAC.Password,
			"sdr", "type", "temperature",
		)
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		log.Printf("ipmitool error: %v, stderr: %s", err, stderr.String())
		return nil, err
	}

	temps := parseCPUTemps(stdout.String())
	
	reading := &CPUReading{
		Temps: temps,
		Max:   maxInt(temps),
	}

	return reading, nil
}

// parseCPUTemps extracts CPU temperatures from ipmitool output
// Example output from R730:
//   Inlet Temp       | 04h | ok  |  7.1 | 20 degrees C
//   Exhaust Temp     | 01h | ok  |  7.1 | 28 degrees C
//   Temp             | 0Eh | ok  |  3.1 | 33 degrees C  <- CPU 1
//   Temp             | 0Fh | ok  |  3.2 | 35 degrees C  <- CPU 2
func parseCPUTemps(output string) []int {
	var temps []int
	
	lines := strings.Split(output, "\n")
	for _, line := range lines {
		// Skip inlet/exhaust temps, focus on CPU temps
		lower := strings.ToLower(line)
		if strings.Contains(lower, "inlet") || strings.Contains(lower, "exhaust") {
			continue
		}
		
		// Only process lines that start with "Temp" (CPU temps on Dell servers)
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "Temp") {
			continue
		}
		
		// Extract temperature value: look for "NN degrees"
		re := regexp.MustCompile(`(\d+)\s*degrees`)
		matches := re.FindStringSubmatch(line)
		if len(matches) >= 2 {
			if temp, err := strconv.Atoi(matches[1]); err == nil && temp > 0 && temp < 120 {
				temps = append(temps, temp)
			}
		}
	}

	// Fallback: try to find any temperature reading if regex didn't match
	if len(temps) == 0 {
		re2 := regexp.MustCompile(`(\d+)\s*degrees`)
		for _, line := range lines {
			matches := re2.FindStringSubmatch(line)
			if len(matches) >= 2 {
				if temp, err := strconv.Atoi(matches[1]); err == nil && temp > 0 && temp < 120 {
					temps = append(temps, temp)
				}
			}
		}
	}

	return temps
}

func maxInt(vals []int) int {
	if len(vals) == 0 {
		return 0
	}
	max := vals[0]
	for _, v := range vals[1:] {
		if v > max {
			max = v
		}
	}
	return max
}
