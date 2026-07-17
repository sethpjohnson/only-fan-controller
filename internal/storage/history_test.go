package storage

import (
	"testing"
	"time"
)

// lastReadingID returns the id of the most recently inserted row. Safe here
// because each test uses its own private :memory: DB with no concurrent
// writers, so it reliably identifies whichever RecordReading call just ran.
func lastReadingID(t *testing.T, s *Store) int64 {
	t.Helper()
	var id int64
	if err := s.db.QueryRow("SELECT max(id) FROM readings").Scan(&id); err != nil {
		t.Fatalf("failed to read last reading id: %v", err)
	}
	return id
}

// ageReading rewrites a row's timestamp to `hours` in the past using SQLite's
// own datetime('now', ...), which produces the exact same plain-UTC-text
// format ("YYYY-MM-DD HH:MM:SS", no offset, no fractional seconds) that the
// timestamp column's DATETIME DEFAULT CURRENT_TIMESTAMP writes via
// RecordReading. This lets tests age a real, production-written row without
// reimplementing timestamp formatting (and without depending on the host's
// local timezone, since SQLite's 'now' is always UTC).
func ageReading(t *testing.T, s *Store, id int64, hours int) {
	t.Helper()
	_, err := s.db.Exec(
		"UPDATE readings SET timestamp = datetime('now', printf('-%d hours', ?)) WHERE id = ?",
		hours, id,
	)
	if err != nil {
		t.Fatalf("failed to age reading id=%d: %v", id, err)
	}
}

// TestCleanupRemovesOnlyOldReadings guards against a real bug: Cleanup used to
// bind a raw time.Time as the DELETE parameter. go-sqlite3 serializes a
// time.Time using the HOST's local timezone plus a numeric offset and
// fractional seconds (e.g. "2026-07-16 12:02:29.5-04:00"), but the readings
// table stores plain UTC text via CURRENT_TIMESTAMP (e.g.
// "2026-07-17 16:02:29"). SQLite compares timestamps as plain strings, so
// that mismatch made retention drift by the host's UTC offset - on a
// non-UTC host a row could survive (or be deleted) hours earlier/later than
// intended.
//
// Rows are seeded through the REAL write path (RecordReading, which relies on
// CURRENT_TIMESTAMP) and then aged via ageReading (SQLite's own datetime('now',
// ...), same text format) rather than via a hand-formatted timestamp string -
// only that exercises the same write/compare path production uses. The seeded
// rows sit just inside/outside a 24h retention window (23h old vs. 25h old):
// that boundary fails under the old buggy code on any non-UTC host, and passes
// on the fixed code regardless of the host's timezone.
func TestCleanupRemovesOnlyOldReadings(t *testing.T) {
	s, err := New(":memory:")
	if err != nil {
		t.Fatalf("failed to open in-memory store: %v", err)
	}
	defer s.Close()

	if err := s.RecordReading(50, 0, 20); err != nil {
		t.Fatalf("RecordReading failed: %v", err)
	}
	tooOld := lastReadingID(t, s)
	ageReading(t, s, tooOld, 25) // 25h old: must be deleted under 24h retention

	if err := s.RecordReading(51, 0, 20); err != nil {
		t.Fatalf("RecordReading failed: %v", err)
	}
	stillFresh := lastReadingID(t, s)
	ageReading(t, s, stillFresh, 23) // 23h old: must survive 24h retention

	if err := s.RecordReading(52, 0, 20); err != nil {
		t.Fatalf("RecordReading failed: %v", err)
	}
	// Left at "now" (unaged): must survive.

	deleted, err := s.Cleanup(24 * time.Hour)
	if err != nil {
		t.Fatalf("Cleanup returned error: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("expected 1 row deleted (the 25h-old reading), got %d", deleted)
	}

	remaining, err := s.GetHistory(365 * 24 * time.Hour)
	if err != nil {
		t.Fatalf("GetHistory returned error: %v", err)
	}
	if len(remaining) != 2 {
		t.Fatalf("expected 2 rows remaining (23h-old + now), got %d", len(remaining))
	}
	for _, p := range remaining {
		if p.CPUTemp == 50 {
			t.Fatalf("the 25h-old reading (cpu_temp=50) should have been deleted, but survived")
		}
	}
}

func TestCleanupNoOldReadingsDeletesNothing(t *testing.T) {
	s, err := New(":memory:")
	if err != nil {
		t.Fatalf("failed to open in-memory store: %v", err)
	}
	defer s.Close()

	if err := s.RecordReading(40, 30, 20); err != nil {
		t.Fatalf("RecordReading failed: %v", err)
	}

	deleted, err := s.Cleanup(30 * 24 * time.Hour)
	if err != nil {
		t.Fatalf("Cleanup returned error: %v", err)
	}
	if deleted != 0 {
		t.Fatalf("expected 0 rows deleted, got %d", deleted)
	}
}
