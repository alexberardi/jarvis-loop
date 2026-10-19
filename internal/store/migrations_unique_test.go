package store

import (
	"github.com/alexberardi/jarvis-loop/internal/clock"
	"strconv"
	"strings"
	"testing"
)

// Two branches that each add the next migration number merge cleanly and break main: the second
// file is skipped as already applied (2026-10-18, #205 and #213 both added 0022).
func TestMigrationVersionsUnique(t *testing.T) {
	ents, err := migrations.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[int]string{}
	for _, e := range ents {
		v, err := strconv.Atoi(strings.SplitN(e.Name(), "_", 2)[0])
		if err != nil {
			t.Fatalf("%s: name must start with a number", e.Name())
		}
		if prev, ok := seen[v]; ok {
			t.Errorf("migrations %s and %s share version %d", prev, e.Name(), v)
		}
		seen[v] = e.Name()
	}
}

// 0025_proofs (#194) and 0025_usage (#199) merged with one number; usage is 0026 now. A database
// that recorded 25 for the usage migration (a loopd built between the two merges in the other
// order) gets the proof columns, and a 0026 that already ran is not run twice.
func TestMigration25RecordedForUsage(t *testing.T) {
	dir := t.TempDir()
	clk := clock.NewFake(t0)
	s, err := Open(dir, clk)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`ALTER TABLE items DROP COLUMN proof`,
		`ALTER TABLE items DROP COLUMN proof_asked`,
		`DELETE FROM schema_migrations WHERE version >= 26`,
	} {
		if _, err := s.w.Exec(q); err != nil {
			t.Fatal(q, err)
		}
	}
	s.Close()
	s, err = Open(dir, clk)
	if err != nil {
		t.Fatalf("reopen after 25 = usage: %v", err)
	}
	defer s.Close()
	var n int
	if err := s.w.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('items') WHERE name IN ('proof', 'proof_asked')`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("proof columns after reopen: %d, %v", n, err)
	}
	if err := s.w.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version = 26`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("version 26 recorded %d times, %v", n, err)
	}
}
