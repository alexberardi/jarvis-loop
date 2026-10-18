package store

import (
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
