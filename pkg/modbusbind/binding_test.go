// PRMT-030 §A.5 — modbusbind must mirror the prior duplicates
// (gateway.bindingFromProtocol + cmd/cios-modbus-driver.pointDefToBinding)
// bit-for-bit. Each table case names the pre-PRMT-030 call-site
// expectation it covers.
package modbusbind

import (
	"strings"
	"testing"

	"github.com/yurimeng/cios/pkg/pointmap"
)

func TestBuildFromPointDef_RegisterTypes(t *testing.T) {
	cases := []struct {
		name    string
		proto   map[string]interface{}
		wantReg uint16
		wantTab string
		wantErr bool
	}{
		{"int register", map[string]interface{}{"register": int(30021)}, 30021, "holding", false},
		{"int64 register", map[string]interface{}{"register": int64(40001)}, 40001, "holding", false},
		{"float64 register truncates to uint16",
			map[string]interface{}{"register": float64(30021)}, 30021, "holding", false},
		{"string register parses base10",
			map[string]interface{}{"register": "30021"}, 30021, "holding", false},
		{"string register bad",
			map[string]interface{}{"register": "notanumber"}, 0, "", true},
		{"bool register unsupported",
			map[string]interface{}{"register": true}, 0, "", true},
		{"table=input accepted",
			map[string]interface{}{"register": 100, "table": "input"}, 100, "input", false},
		{"table=coils rejected",
			map[string]interface{}{"register": 100, "table": "coils"}, 0, "", true},
		{"table=42 rejected (non-string)",
			map[string]interface{}{"register": 100, "table": 42}, 0, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pd := pointmap.PointDef{Point: "p.x.y", Protocol: tc.proto}
			b, err := BuildFromPointDef(pd)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			if b.Register != tc.wantReg {
				t.Errorf("Register=%d want %d", b.Register, tc.wantReg)
			}
			if b.Table != tc.wantTab {
				t.Errorf("Table=%q want %q", b.Table, tc.wantTab)
			}
			// default Access is ro → Writable false unless Access=rw set on PointDef
			if b.Writable {
				t.Errorf("Writable=true for default (ro) PointDef")
			}
			if b.Point != "p.x.y" {
				t.Errorf("Point=%q want %q", b.Point, "p.x.y")
			}
		})
	}
}

func TestBuildFromPointDef_WritableFromAccess(t *testing.T) {
	pd := pointmap.PointDef{
		Point:    "tcs.opening",
		Access:   "rw",
		Protocol: map[string]interface{}{"register": 32, "table": "holding"},
	}
	b, err := BuildFromPointDef(pd)
	if err != nil {
		t.Fatal(err)
	}
	if !b.Writable {
		t.Fatal("Access=rw must set Writable")
	}
	pd.Access = "ro"
	b, err = BuildFromPointDef(pd)
	if err != nil {
		t.Fatal(err)
	}
	if b.Writable {
		t.Fatal("Access=ro must clear Writable")
	}
}

func TestBuildFromPointDef_MissingRegister(t *testing.T) {
	pd := pointmap.PointDef{Protocol: map[string]interface{}{"table": "holding"}}
	_, err := BuildFromPointDef(pd)
	if err == nil || !strings.Contains(err.Error(), "missing register") {
		t.Fatalf("err=%v, want 'missing register'", err)
	}
}

func TestBuildFromPointDef_NilRegister(t *testing.T) {
	// PRMT-030 §A.4: a present-but-nil register counts as missing.
	pd := pointmap.PointDef{Protocol: map[string]interface{}{"register": nil}}
	_, err := BuildFromPointDef(pd)
	if err == nil || !strings.Contains(err.Error(), "missing register") {
		t.Fatalf("err=%v, want 'missing register'", err)
	}
}

func TestBuildFromPointDef_DefaultTableIsHolding(t *testing.T) {
	pd := pointmap.PointDef{Protocol: map[string]interface{}{"register": 1}}
	b, err := BuildFromPointDef(pd)
	if err != nil {
		t.Fatal(err)
	}
	if b.Table != "holding" {
		t.Errorf("Table=%q, want holding", b.Table)
	}
}
