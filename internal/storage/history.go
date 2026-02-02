package storage

import (
	"database/sql"
	"os"
	"path/filepath"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

type Store struct {
	db *sql.DB
}

type HistoryPoint struct {
	Timestamp time.Time `json:"timestamp"`
	CPUTemp   int       `json:"cpu_temp"`
	GPUTemp   int       `json:"gpu_temp"`
	FanSpeed  int       `json:"fan_speed"`
}

func New(dbPath string) (*Store, error) {
	// Ensure directory exists
	dir := filepath.Dir(dbPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}

	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return nil, err
	}

	// Create tables
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS readings (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			timestamp DATETIME DEFAULT CURRENT_TIMESTAMP,
			cpu_temp INTEGER,
			gpu_temp INTEGER,
			fan_speed INTEGER
		);
		
		CREATE INDEX IF NOT EXISTS idx_readings_timestamp ON readings(timestamp);
	`)
	if err != nil {
		db.Close()
		return nil, err
	}

	return &Store{db: db}, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

// RecordReading stores a temperature/fan reading
func (s *Store) RecordReading(cpuTemp, gpuTemp, fanSpeed int) error {
	_, err := s.db.Exec(
		"INSERT INTO readings (cpu_temp, gpu_temp, fan_speed) VALUES (?, ?, ?)",
		cpuTemp, gpuTemp, fanSpeed,
	)
	return err
}

// GetHistory retrieves readings for the specified duration
func (s *Store) GetHistory(duration time.Duration) ([]HistoryPoint, error) {
	cutoff := time.Now().Add(-duration)
	
	rows, err := s.db.Query(`
		SELECT timestamp, cpu_temp, gpu_temp, fan_speed 
		FROM readings 
		WHERE timestamp > datetime(?)
		ORDER BY timestamp ASC
	`, cutoff.UTC().Format("2006-01-02 15:04:05"))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var history []HistoryPoint
	for rows.Next() {
		var p HistoryPoint
		var ts string
		if err := rows.Scan(&ts, &p.CPUTemp, &p.GPUTemp, &p.FanSpeed); err != nil {
			continue
		}
		// Try multiple timestamp formats
		for _, format := range []string{
			"2006-01-02 15:04:05",
			"2006-01-02T15:04:05Z",
			time.RFC3339,
		} {
			if t, err := time.Parse(format, ts); err == nil {
				p.Timestamp = t
				break
			}
		}
		history = append(history, p)
	}

	return history, nil
}

// Cleanup removes old readings beyond retention period
func (s *Store) Cleanup(retention time.Duration) error {
	cutoff := time.Now().Add(-retention)
	_, err := s.db.Exec("DELETE FROM readings WHERE timestamp < ?", cutoff)
	return err
}
