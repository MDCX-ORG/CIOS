package pointmap

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadUnitsOK(t *testing.T) {
	u, err := LoadUnits(filepath.Join("..", "..", "protocol"))
	if err != nil {
		t.Fatalf("LoadUnits: %v", err)
	}
	if u == nil {
		t.Fatal("u is nil")
	}
}

func TestCanConvert(t *testing.T) {
	u, err := LoadUnits(filepath.Join("..", "..", "protocol"))
	if err != nil {
		t.Fatalf("LoadUnits: %v", err)
	}
	cases := []struct {
		std, in  string
		wantFact float64
		wantOff  float64
		wantOK   bool
	}{
		{"celsius", "kelvin", 1.0, -273.15, true},
		{"lpm", "lpm", 1.0, 0.0, true},  // identity
		{"celsius", "psi", 0, 0, false}, // unknown pair
		{"kelvin", "celsius", 0, 0, false},
		{"lpm", "m3ph", 16.666667, 0.0, true},
	}
	for _, c := range cases {
		got, ok := u.CanConvert(c.std, c.in)
		if ok != c.wantOK {
			t.Errorf("CanConvert(%q,%q) ok = %v, want %v", c.std, c.in, ok, c.wantOK)
			continue
		}
		if !ok {
			continue
		}
		if got.Factor != c.wantFact {
			t.Errorf("CanConvert(%q,%q) factor = %v, want %v", c.std, c.in, got.Factor, c.wantFact)
		}
		if got.Offset != c.wantOff {
			t.Errorf("CanConvert(%q,%q) offset = %v, want %v", c.std, c.in, got.Offset, c.wantOff)
		}
	}
}

// --- L54 ext.d merging (PRMT-007) -------------------------------------------

func TestLoadUnitsExtdOK(t *testing.T) {
	// testdata/extd-units: core celsius + ext.d fragment adding us_per_cm.
	// After merge, both celsius→kelvin and us_per_cm→ms_per_cm must work.
	u, err := LoadUnits(filepath.Join("testdata", "extd-units"))
	if err != nil {
		t.Fatalf("LoadUnits: %v", err)
	}
	got, ok := u.CanConvert("celsius", "kelvin")
	if !ok || got.Factor != 1.0 || got.Offset != -273.15 {
		t.Errorf("celsius→kelvin = %+v, %v; want {1, -273.15}, true", got, ok)
	}
	got, ok = u.CanConvert("us_per_cm", "ms_per_cm")
	if !ok || got.Factor != 1000.0 || got.Offset != 0 {
		t.Errorf("us_per_cm→ms_per_cm = %+v, %v; want {1000, 0}, true", got, ok)
	}
}

func TestLoadUnitsExtdDuplicate(t *testing.T) {
	// testdata/extd-units-dup: fragment re-defines celsius — load error.
	_, err := LoadUnits(filepath.Join("testdata", "extd-units-dup"))
	if err == nil {
		t.Fatal("expected conflict error, got nil")
	}
	if !strings.Contains(err.Error(), "celsius") {
		t.Errorf("err = %v, want mention of celsius", err)
	}
}
