package cpath

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadDictOK(t *testing.T) {
	// tools/speccheck lives two levels up from pkg/cpath.
	repo := filepath.Join("..", "..")
	d, err := LoadDict(filepath.Join(repo, "protocol"))
	if err != nil {
		t.Fatalf("LoadDict: %v", err)
	}
	if _, ok := d.Types["pod"]; !ok {
		t.Errorf("expected pod in types")
	}
	if _, ok := d.Quantities["flow"]; !ok {
		t.Errorf("expected flow in quantities")
	}
	if _, ok := d.Derived["pue"]; !ok {
		t.Errorf("expected pue in derived")
	}
	if !d.Loops["fws"] || !d.Loops["tcs"] {
		t.Errorf("expected fws/tcs loops, got %v", d.Loops)
	}
	if !d.Sides["supply"] || !d.Sides["return"] {
		t.Errorf("expected supply/return sides, got %v", d.Sides)
	}
	if !d.Phases["l1"] || !d.Phases["n"] {
		t.Errorf("expected l1/n phases, got %v", d.Phases)
	}
	// Enum map is populated for enum-typed quantities and nil otherwise.
	if e := d.Quantities["status"].Enum; e == nil || e[0] != "ok" || e[5] != "offline" || len(e) != 6 {
		t.Errorf("status.Enum = %v, want 6-entry map ending 0:ok 5:offline", e)
	}
	if e := d.Quantities["leak"].Enum; e == nil || e[0] != "dry" || e[1] != "leak" {
		t.Errorf("leak.Enum = %v, want 0:dry 1:leak", e)
	}
	if e := d.Quantities["flow"].Enum; e != nil {
		t.Errorf("flow.Enum = %v, want nil (non-enum quantity)", e)
	}
	if e := d.Derived["pue"].Enum; e != nil {
		t.Errorf("pue.Enum (derived) = %v, want nil", e)
	}
}

func TestLoadDictMissingDir(t *testing.T) {
	if _, err := LoadDict("does-not-exist"); err == nil {
		t.Errorf("expected error for missing dir, got nil")
	}
}

// --- L54 ext.d merging (PRMT-007) -------------------------------------------

func TestLoadDictExtdOK(t *testing.T) {
	// testdata/extd-ok: core + one additive fragment; both newtype and
	// extq must appear in the merged Dict.
	d, err := LoadDict(filepath.Join("testdata", "extd-ok"))
	if err != nil {
		t.Fatalf("LoadDict: %v", err)
	}
	if _, ok := d.Types["newtype"]; !ok {
		t.Errorf("expected newtype in merged types")
	}
	if _, ok := d.Types["pod"]; !ok {
		t.Errorf("expected pod (core) to still be present")
	}
	if _, ok := d.Quantities["extq"]; !ok {
		t.Errorf("expected extq in merged quantities")
	}
}

func TestLoadDictExtdDuplicateCore(t *testing.T) {
	// testdata/extd-dup-core: fragment re-defines core "pod" — load error
	// names the offending entry.
	_, err := LoadDict(filepath.Join("testdata", "extd-dup-core"))
	if err == nil {
		t.Fatal("expected conflict error, got nil")
	}
	if !strings.Contains(err.Error(), "pod") {
		t.Errorf("err = %v, want mention of pod", err)
	}
}

func TestLoadDictExtdDuplicateFrag(t *testing.T) {
	// testdata/extd-dup-frag: two fragments define "fragsame" — load error
	// names the second-offending entry (b.yaml, sorted after a.yaml).
	_, err := LoadDict(filepath.Join("testdata", "extd-dup-frag"))
	if err == nil {
		t.Fatal("expected inter-fragment conflict, got nil")
	}
	if !strings.Contains(err.Error(), "fragsame") {
		t.Errorf("err = %v, want mention of fragsame", err)
	}
}

func TestLoadDictExtdCrossSection(t *testing.T) {
	// testdata/extd-cross: fragment defines "temp" as a type while
	// quantities.yaml already has "temp" as a quantity. LoadDict does
	// not enforce cross-section disjointness (speccheck does) — so this
	// must LOAD successfully. The disjointness check belongs in
	// tools/speccheck, not in cpath.
	d, err := LoadDict(filepath.Join("testdata", "extd-cross"))
	if err != nil {
		t.Fatalf("LoadDict: %v (cross-section disjointness is speccheck's job)", err)
	}
	if _, ok := d.Types["temp"]; !ok {
		t.Errorf("expected temp in merged types")
	}
	if _, ok := d.Quantities["temp"]; !ok {
		t.Errorf("expected temp in merged quantities (core)")
	}
}
