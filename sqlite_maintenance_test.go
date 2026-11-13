package main

import (
	"os"
	"strings"
	"testing"
)

// Tests for runSQLiteMaintenance (db.go): PRAGMA optimize always, VACUUM only
// when the freelist grows past its 1000-page threshold. The function reports
// problems only through logs, so every assertion here is behavioral: the
// freelist count, the database file size, and the database's continued
// usability.

// freelistPages reads PRAGMA freelist_count through the writer pool, the same
// way runSQLiteMaintenance does.
func freelistPages(t *testing.T) int64 {
	t.Helper()
	var n int64
	if err := db.QueryRow("PRAGMA freelist_count").Scan(&n); err != nil {
		t.Fatalf("freelist_count: %v", err)
	}
	return n
}

// mainDBSize is the size of the database file itself (not the -wal/-shm
// sidecars).
func mainDBSize(t *testing.T) int64 {
	t.Helper()
	fi, err := os.Stat(cfg.DBPath)
	if err != nil {
		t.Fatalf("stat %s: %v", cfg.DBPath, err)
	}
	return fi.Size()
}

// Baseline: on a healthy database the maintenance pass must be a safe no-op —
// the freelist is far below the VACUUM threshold — and the database stays
// fully usable afterwards.
func TestSQLiteMaintenanceOnHealthyDatabase(t *testing.T) {
	withTestDB(t)

	runSQLiteMaintenance()

	var n int
	if err := dbRead.QueryRow("SELECT COUNT(*) FROM users").Scan(&n); err != nil {
		t.Fatalf("read after maintenance: %v", err)
	}
	if _, err := db.Exec("INSERT INTO settings(key, value) VALUES ('maintenance-probe', '1')"); err != nil {
		t.Fatalf("write after maintenance: %v", err)
	}
	if _, err := db.Exec("DELETE FROM settings WHERE key = 'maintenance-probe'"); err != nil {
		t.Fatalf("delete after maintenance: %v", err)
	}
	if free := freelistPages(t); free > 1000 {
		t.Errorf("freelist = %d after maintenance, want at or below the 1000-page vacuum threshold", free)
	}
}

// Push the freelist above the 1000-page threshold with a bulk insert/delete
// cycle and assert the maintenance pass reclaims the pages: the freelist
// drops to zero and the database file shrinks. Nothing else in the test
// issues a VACUUM, so the dropped freelist is the observable proof the
// maintenance pass ran it.
func TestSQLiteMaintenanceVacuumsAfterBulkDelete(t *testing.T) {
	withTestDB(t)

	// 1500 rows of 5 KiB ≈ 7.5 MiB ≈ ~1900 freed pages at the default
	// 4 KiB page size, comfortably above the 1000-page threshold.
	const rows = 1500
	payload := strings.Repeat("x", 5120)
	if _, err := db.Exec("CREATE TABLE maintenance_free_pages(payload TEXT NOT NULL)"); err != nil {
		t.Fatalf("create probe table: %v", err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	stmt, err := tx.Prepare("INSERT INTO maintenance_free_pages(payload) VALUES (?)")
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	for i := 0; i < rows; i++ {
		if _, err := stmt.Exec(payload); err != nil {
			_ = tx.Rollback()
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	if err := stmt.Close(); err != nil {
		t.Fatalf("close statement: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	if _, err := db.Exec("DELETE FROM maintenance_free_pages"); err != nil {
		t.Fatalf("bulk delete: %v", err)
	}
	// Move any WAL data into the main file so the size measured below is
	// where the freed pages actually sit.
	if _, err := db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}

	if free := freelistPages(t); free <= 1000 {
		t.Fatalf("freelist = %d after the bulk delete, want above the 1000-page threshold", free)
	}
	sizeBefore := mainDBSize(t)

	runSQLiteMaintenance()

	// VACUUM rewrites the file and returns the freed pages to the OS.
	if _, err := db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if free := freelistPages(t); free != 0 {
		t.Errorf("freelist = %d after maintenance, want 0 (VACUUM must have reclaimed the pages)", free)
	}
	// VACUUM in WAL mode leaves the rewritten content in the -wal file; a
	// checkpoint materializes it in the main file so the shrink is observable
	// there.
	if _, err := db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		t.Fatalf("post-VACUUM checkpoint: %v", err)
	}
	if sizeAfter := mainDBSize(t); sizeAfter >= sizeBefore {
		t.Errorf("database file did not shrink: %d -> %d bytes", sizeBefore, sizeAfter)
	}

	// The database is still usable after the VACUUM.
	var n int
	if err := dbRead.QueryRow("SELECT COUNT(*) FROM maintenance_free_pages").Scan(&n); err != nil {
		t.Fatalf("read after VACUUM: %v", err)
	}
	if n != 0 {
		t.Errorf("probe table rows = %d after VACUUM, want 0", n)
	}
	if _, err := db.Exec("INSERT INTO maintenance_free_pages(payload) VALUES ('still-usable')"); err != nil {
		t.Fatalf("write after VACUUM: %v", err)
	}
}
