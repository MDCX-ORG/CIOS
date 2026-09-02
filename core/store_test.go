package core

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// newStore is a tiny helper: a temp file + a fresh store.
func newStore(t *testing.T) (Store, string) {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "store.json")
	st, err := NewFileStore(p)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	return st, p
}

func sampleAsset(path, typ string) Asset {
	return Asset{Path: path, Spec: map[string]any{"type": typ}}
}

func TestStore_PutCreateAndUpdate(t *testing.T) {
	st, _ := newStore(t)
	a, err := st.PutAsset(context.Background(), sampleAsset("site01.pod000.cdu000", "cdu"), 0)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if a.ResourceVersion != 1 {
		t.Errorf("version = %d, want 1", a.ResourceVersion)
	}
	// Update bumps version.
	a2, err := st.PutAsset(context.Background(), sampleAsset("site01.pod000.cdu000", "cdu"), 0)
	if err != nil {
		t.Fatalf("put2: %v", err)
	}
	if a2.ResourceVersion != 2 {
		t.Errorf("version = %d, want 2", a2.ResourceVersion)
	}
}

func TestStore_OptimisticLock(t *testing.T) {
	st, _ := newStore(t)
	_, _ = st.PutAsset(context.Background(), sampleAsset("site01.pod000.cdu000", "cdu"), 0)
	// Right version → ok.
	if _, err := st.PutAsset(context.Background(), sampleAsset("site01.pod000.cdu000", "cdu"), 1); err != nil {
		t.Errorf("expectVersion=1 should pass: %v", err)
	}
	// Stale version → conflict.
	if _, err := st.PutAsset(context.Background(), sampleAsset("site01.pod000.cdu000", "cdu"), 1); !errors.Is(err, ErrVersionConflict) {
		t.Errorf("stale expectVersion: want ErrVersionConflict, got %v", err)
	}
}

func TestStore_AtomicWriteAndReload(t *testing.T) {
	st, p := newStore(t)
	if _, err := st.PutAsset(context.Background(), sampleAsset("site01.pod000.cdu000", "cdu"), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PutAsset(context.Background(), sampleAsset("site01.pod000.cdu001", "cdu"), 0); err != nil {
		t.Fatal(err)
	}
	// Reload from disk in a separate Store handle.
	st2, err := NewFileStore(p)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	all, _ := st2.ListAssets(context.Background())
	if len(all) != 2 {
		t.Fatalf("reloaded len = %d, want 2", len(all))
	}
	if all[0].Path != "site01.pod000.cdu000" || all[1].Path != "site01.pod000.cdu001" {
		t.Errorf("reload order: %+v", all)
	}
}

func TestStore_DeleteAndCascade(t *testing.T) {
	st, _ := newStore(t)
	for _, p := range []string{
		"site01.pod000", "site01.pod000.cdu000", "site01.pod000.cdu001",
		"site01.pod001",
	} {
		typ := "cdu"
		if p == "site01.pod000" || p == "site01.pod001" {
			typ = "pod"
		}
		if _, err := st.PutAsset(context.Background(), sampleAsset(p, typ), 0); err != nil {
			t.Fatal(err)
		}
	}
	// Delete leaf → 1.
	n, err := st.DeleteAsset(context.Background(), "site01.pod001", false)
	if err != nil || n != 1 {
		t.Errorf("leaf delete: n=%d err=%v", n, err)
	}
	// Delete pod without cascade → ErrHasChildren.
	_, err = st.DeleteAsset(context.Background(), "site01.pod000", false)
	if !errors.Is(err, ErrHasChildren) {
		t.Errorf("want ErrHasChildren, got %v", err)
	}
	// Delete pod with cascade → 3 (pod + 2 cdus).
	n, err = st.DeleteAsset(context.Background(), "site01.pod000", true)
	if err != nil || n != 3 {
		t.Errorf("cascade delete: n=%d err=%v", n, err)
	}
	all, _ := st.ListAssets(context.Background())
	if len(all) != 0 {
		t.Errorf("post-delete len = %d, want 0", len(all))
	}
}

func TestStore_DeleteMissing(t *testing.T) {
	st, _ := newStore(t)
	n, err := st.DeleteAsset(context.Background(), "site01.nope", false)
	if err != nil {
		t.Errorf("missing delete should be no-op err=nil, got %v", err)
	}
	if n != 0 {
		t.Errorf("missing delete n = %d, want 0", n)
	}
}

func TestStore_SeedAlarmsIdempotent(t *testing.T) {
	st, _ := newStore(t)
	a := Alarm{ID: "A1", Path: "site01.pod000.cdu000", Severity: "critical", State: "firing", Summary: "x", Since: time.Unix(1700000000, 0).UTC()}
	_ = st.SeedAlarms(context.Background(), []Alarm{a})
	// Re-seed with updated summary but same ID → summary updates,
	// Since stays put.
	a.Summary = "y"
	_ = st.SeedAlarms(context.Background(), []Alarm{a})
	got, _ := st.ListAlarms(context.Background())
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	if got[0].Summary != "y" {
		t.Errorf("summary = %q, want y", got[0].Summary)
	}
	if !got[0].Since.Equal(time.Unix(1700000000, 0).UTC()) {
		t.Errorf("Since drifted on re-seed: %v", got[0].Since)
	}
}

func TestStore_AlarmsSortedBySeverity(t *testing.T) {
	st, _ := newStore(t)
	_ = st.SeedAlarms(context.Background(), []Alarm{
		{ID: "I", Severity: "info", Since: time.Unix(100, 0).UTC()},
		{ID: "C", Severity: "critical", Since: time.Unix(100, 0).UTC()},
		{ID: "M", Severity: "major", Since: time.Unix(100, 0).UTC()},
	})
	got, _ := st.ListAlarms(context.Background())
	if len(got) != 3 {
		t.Fatalf("len=%d", len(got))
	}
	if got[0].ID != "C" || got[1].ID != "M" || got[2].ID != "I" {
		t.Errorf("order: %+v", got)
	}
}

func TestStore_ListAssetsSorted(t *testing.T) {
	st, _ := newStore(t)
	for _, p := range []string{
		"site01.pod010", "site01.pod002", "site01.pod001",
	} {
		_, _ = st.PutAsset(context.Background(), sampleAsset(p, "pod"), 0)
	}
	got, _ := st.ListAssets(context.Background())
	if len(got) != 3 {
		t.Fatalf("len=%d", len(got))
	}
	// Three-digit zero padding makes string sort = numeric sort.
	if got[0].Path != "site01.pod001" || got[1].Path != "site01.pod002" || got[2].Path != "site01.pod010" {
		t.Errorf("order: %+v", got)
	}
}
