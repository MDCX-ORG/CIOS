package alarm

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// readAlarmStoreSource returns the alarm store.go file as a string
// so the suppression-shape test can grep for SQL fragments without
// re-typing the SQL — a low-cost regression net for the §2 / §4
// invariants when the file is hand-edited.
func readAlarmStoreSource(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(wd, "store.go")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestEventID_StableAndDistinct verifies the dedup-key-to-PK mapping
// is deterministic and collision-free across rule/asset variations
// that a real deployment would hit.
func TestEventID_StableAndDistinct(t *testing.T) {
	a := eventID("cdu-fws-deltat-low", "sgp01.pod002.cdu000")
	b := eventID("cdu-fws-deltat-low", "sgp01.pod002.cdu000")
	if a != b {
		t.Fatalf("same input gave different IDs: %s vs %s", a, b)
	}
	if len(a) != 16 {
		t.Fatalf("len(id)=%d, want 16", len(a))
	}
	// Different asset → different id.
	if eventID("cdu-fws-deltat-low", "sgp01.pod002.cdu001") == a {
		t.Fatal("different asset collided")
	}
	// Different rule → different id.
	if eventID("cdu-status-fault", "sgp01.pod002.cdu000") == a {
		t.Fatal("different rule collided")
	}
}

// TestStore_Upsert_NoPG skips when no DSN is set in the test env.
// A real PG round-trip would belong in pg_store_integration_test.go
// (mirroring the core/pg_store_test.go pattern), but this package
// has no such fixture and PRMT-020 §5 explicitly allows t.Skip.
func TestStore_Upsert_NoPG(t *testing.T) {
	t.Setenv("CIOS_ALARM_TEST_DSN", "")
	if testing.Short() {
		t.Skip("short mode: skipping PG round-trip")
	}
	// No DSN: assert constructor rejects empty DSN (no network).
	if _, err := NewStore(t.Context(), ""); err == nil {
		t.Fatal("empty DSN should error")
	}
}

// TestOpenTicket_NoPG verifies the NoPG guard: a Store with a nil
// pool (the path exercised when cios-alarm runs without -pg-dsn,
// which is rare but supported) must return nil, not panic. PRMT-034
// §4.1 says "镜像 Upsert's NoPG guard".
func TestOpenTicket_NoPG(t *testing.T) {
	var s *Store // nil receiver; pool is nil
	ev := Event{RuleName: "cdu-fws-deltat-low", AssetPath: "sgp01.pod002.cdu000"}
	if err := s.OpenTicket(t.Context(), ev); err != nil {
		t.Fatalf("nil-store OpenTicket: %v", err)
	}
	// Non-nil Store, nil pool: same NoPG behaviour.
	s2 := &Store{}
	if err := s2.OpenTicket(t.Context(), ev); err != nil {
		t.Fatalf("nil-pool OpenTicket: %v", err)
	}
}

// TestNewTicketID_Shape verifies the id generator produces ids
// matching core.newTicketID's shape: "tk_" + 16 chars in [A-Z2-7]
// (RFC 4648 base32 alphabet, no padding). PRMT-034 §4.1 / L69.
func TestNewTicketID_Shape(t *testing.T) {
	re := regexp.MustCompile(`^tk_[A-Z2-7]{16}$`)
	for i := 0; i < 32; i++ {
		id, err := newTicketID()
		if err != nil {
			t.Fatalf("newTicketID: %v", err)
		}
		if !re.MatchString(id) {
			t.Fatalf("id %q does not match %s", id, re)
		}
	}
	// Two calls produce different ids (sanity).
	a, _ := newTicketID()
	b, _ := newTicketID()
	if a == b {
		t.Fatal("newTicketID returned identical ids")
	}
}

// TestOpenTicket_SuppressionShape locks the maintenance-window
// probe SQL shape that PRMT-096 §2 / §4 mandates. We cannot
// exercise the live round-trip here (no PG fixture in this
// package — see TestStore_Upsert_NoPG), so we cover the SQL
// fragment and the suppression log message shape so a reviewer
// can grep both invariants from the code without booting PG.
//
// The probe must:
//   - query maintenance_windows
//   - bound [starts_at, ends_at) around now()
//   - match asset_path equality OR "."-prefixed ancestor (LIKE
//     asset_path || '.%')
//   - return the matching id (so the suppression log can name it)
//   - ORDER BY starts_at ASC, id ASC LIMIT 1 for determinism
func TestOpenTicket_SuppressionShape(t *testing.T) {
	src := readAlarmStoreSource(t)
	for _, frag := range []string{
		"SELECT id FROM maintenance_windows",
		"starts_at <= $2",
		"ends_at   >  $2",
		"asset_path = $1",
		"asset_path LIKE $1 || '.%'",
		"ORDER BY starts_at ASC, id ASC",
		"LIMIT 1",
	} {
		if !strings.Contains(src, frag) {
			t.Errorf("OpenTicket suppression SQL missing fragment: %q", frag)
		}
	}
	// And the suppression log line shape per PRMT-096 §2.
	want := "cios-alarm: suppressed auto-ticket for %s (maintenance window %s)"
	if !strings.Contains(src, want) {
		t.Errorf("OpenTicket suppression log missing shape: %q", want)
	}
}
