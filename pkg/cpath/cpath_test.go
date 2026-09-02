package cpath

import (
	"errors"
	"path/filepath"
	"testing"
)

// loadTestDict loads the real protocol/ dictionary from the repo root.
func loadTestDict(t *testing.T) *Dict {
	t.Helper()
	repo := filepath.Join("..", "..")
	d, err := LoadDict(filepath.Join(repo, "protocol"))
	if err != nil {
		t.Fatalf("LoadDict: %v", err)
	}
	return d
}

// -----------------------------------------------------------------------------
// §5 valid ParseAssetPath
// -----------------------------------------------------------------------------

func TestParseAssetPathValid(t *testing.T) {
	d := loadTestDict(t)
	cases := []string{
		"site01.pod000.tank003.node012.gpu0",
		"site01.pod002.rack005.pdu000",
		"site01.pod000.ups000.bess000",
		"site01.pod003.busbar000.tou005",
		"site01.chiller002.drycooler000.fan003",
		"site01.switchgear000.breaker004",
		"site01.bess000.battery002.cell117",
	}
	for _, s := range cases {
		t.Run(s, func(t *testing.T) {
			ap, err := d.ParseAssetPath(s)
			if err != nil {
				t.Fatalf("ParseAssetPath(%q): %v", s, err)
			}
			if got := ap.String(); got != s {
				t.Errorf("String() = %q, want %q", got, s)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// §5 valid ParsePoint
// -----------------------------------------------------------------------------

func TestParsePointValid(t *testing.T) {
	d := loadTestDict(t)
	cases := []struct {
		in      string
		loop    string
		loopIdx int
		side    string
		phase   string
		quant   string
		nodes   int
	}{
		{"sgp01.pod002.cdu000.fws.supply.flow", "fws", -1, "supply", "", "flow", 2},
		{"site01.pod002.cdu000.fws.return.temp", "fws", -1, "return", "", "temp", 2},
		{"site01.pod002.rack000.tcs.supply.press", "tcs", -1, "supply", "", "press", 2},
		{"site01.pod003.busbar000.tou005.l1.amp", "", -1, "", "l1", "amp", 3},
		{"site01.pod003.busbar000.tou005.amp", "", -1, "", "", "amp", 3},
		{"site01.pod003.busbar000.tou005.status", "", -1, "", "", "status", 3},
		{"site01.chiller000.pump002.rpm", "", -1, "", "", "rpm", 2},
		{"site01.switchgear000.meter000.energy", "", -1, "", "", "energy", 2},
		{"site01.bess000.battery002.cell117.resistance", "", -1, "", "", "resistance", 3},
		{"site01.pod000.tank003.leak", "", -1, "", "", "leak", 2},
		{"site01.pod000.cdu000.fws1.supply.temp", "fws", 1, "supply", "", "temp", 2},
		{"site01.pod002.cdu000.deltat", "", -1, "", "", "deltat", 2},
		{"site01.pue", "", -1, "", "", "pue", 0},
		{"site01.itload", "", -1, "", "", "itload", 0},
		// R5: side without leading loop is allowed (state machine accepts
		// any non-decreasing category order with any category omitted).
		{"site01.pod002.cdu000.supply.temp", "", -1, "supply", "", "temp", 2},
		{"site01.pod002.cdu000.tcs.return.temp", "tcs", -1, "return", "", "temp", 2},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			p, err := d.ParsePoint(c.in)
			if err != nil {
				t.Fatalf("ParsePoint(%q): %v", c.in, err)
			}
			if p.Loop != c.loop {
				t.Errorf("Loop = %q, want %q", p.Loop, c.loop)
			}
			if p.LoopIndex != c.loopIdx {
				t.Errorf("LoopIndex = %d, want %d", p.LoopIndex, c.loopIdx)
			}
			if p.Side != c.side {
				t.Errorf("Side = %q, want %q", p.Side, c.side)
			}
			if p.Phase != c.phase {
				t.Errorf("Phase = %q, want %q", p.Phase, c.phase)
			}
			if p.Quantity != c.quant {
				t.Errorf("Quantity = %q, want %q", p.Quantity, c.quant)
			}
			if len(p.Asset.Nodes) != c.nodes {
				t.Errorf("Nodes = %d, want %d", len(p.Asset.Nodes), c.nodes)
			}
			if got := p.String(); got != c.in {
				t.Errorf("String() = %q, want %q", got, c.in)
			}
		})
	}
}

// 2-bis explicit field assertions
func TestParsePointFieldsExplicit(t *testing.T) {
	d := loadTestDict(t)
	p, err := d.ParsePoint("site01.pod002.cdu000.fws.supply.flow")
	if err != nil {
		t.Fatalf("ParsePoint: %v", err)
	}
	if p.Asset.Site != "site01" {
		t.Errorf("Site = %q, want site01", p.Asset.Site)
	}
	if len(p.Asset.Nodes) != 2 {
		t.Fatalf("Nodes len = %d, want 2", len(p.Asset.Nodes))
	}
	if p.Asset.Nodes[0].Type != "pod" || p.Asset.Nodes[0].Index != "002" {
		t.Errorf("Nodes[0] = %+v, want {pod,002}", p.Asset.Nodes[0])
	}
	if p.Asset.Nodes[1].Type != "cdu" || p.Asset.Nodes[1].Index != "000" {
		t.Errorf("Nodes[1] = %+v, want {cdu,000}", p.Asset.Nodes[1])
	}
	if p.Loop != "fws" || p.LoopIndex != -1 {
		t.Errorf("Loop/Index = %q/%d, want fws/-1", p.Loop, p.LoopIndex)
	}
	if p.Side != "supply" || p.Phase != "" {
		t.Errorf("Side/Phase = %q/%q, want supply/empty", p.Side, p.Phase)
	}
	if p.Quantity != "flow" {
		t.Errorf("Quantity = %q, want flow", p.Quantity)
	}
}

// -----------------------------------------------------------------------------
// §5 invalid ParsePoint / ParseAssetPath
// -----------------------------------------------------------------------------

func TestParseInvalid(t *testing.T) {
	d := loadTestDict(t)
	cases := []struct {
		in     string
		target error
	}{
		// ErrSyntax
		{"site1.pod000.status", ErrSyntax},
		{"site00.pod000.status", ErrSyntax},
		{"Site01.pod000.status", ErrSyntax},
		{"site01..pod000.status", ErrSyntax},
		// ErrBadIndex
		{"site01.pod00.status", ErrBadIndex},
		{"site01.pod0000.status", ErrBadIndex},
		{"site01.pod000.tank003.node012.gpu00", ErrBadIndex},
		// ErrBadParent
		{"site01.tank003.status", ErrBadParent},
		{"site01.pod000.fan003.status", ErrBadParent},
		// ErrUnknownSegment
		{"site01.pod000.xyz003.status", ErrUnknownSegment},
		{"site01.pod000.cdu000.supply.fws.temp", ErrUnknownSegment},
		{"site01.pod000.cdu000.fws.supply.return.temp", ErrUnknownSegment},
		// R5: state-machine rejection of inversion and duplicate.
		{"site01.pod002.cdu000.fws.l1.supply.temp", ErrUnknownSegment},
		{"site01.pod002.cdu000.l1.supply.amp", ErrUnknownSegment},
		{"site01.pod002.cdu000.fws.fws.temp", ErrUnknownSegment},
		// ErrUnknownQuantity
		{"site01.pod000.cdu000.fws.supply", ErrUnknownQuantity},
		// ErrBadHost
		{"site01.chiller000.pue", ErrBadHost},
		{"site01.pod002.cdu000.cop", ErrBadHost},
		// L48: site-level address with a non-derived quantity → ErrBadHost.
		{"site01.temp", ErrBadHost},
		{"site01.status", ErrBadHost},
		// L47: water-loop temp without a side is a hard error.
		{"site01.pod002.cdu000.fws.temp", ErrMissingSide},
		{"site01.pod000.cdu000.tcs1.temp", ErrMissingSide},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			_, err := d.ParsePoint(c.in)
			if err == nil {
				t.Fatalf("ParsePoint(%q): expected error, got nil", c.in)
			}
			if !errors.Is(err, c.target) {
				t.Errorf("ParsePoint(%q): err = %v, want errors.Is(_, %v)", c.in, err, c.target)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// §5 LintPoint
// -----------------------------------------------------------------------------

func TestLintPoint(t *testing.T) {
	d := loadTestDict(t)

	warn := d.LintPoint(mustPoint(t, d, "site01.pod002.cdu000.fws.flow"))
	if len(warn) != 1 {
		t.Errorf("expected 1 warning for fws.flow, got %d: %v", len(warn), warn)
	}
	// R4: lint message must be the literal string from §4.5 with no quotes.
	const want = "water-loop quantity flow should carry side (supply/return)"
	if warn[0] != want {
		t.Errorf("lint message = %q, want %q", warn[0], want)
	}

	// L47: temp is now a hard error, not a lint warning. Build the Point
	// directly (ParsePoint rejects it) to exercise LintPoint's contract.
	tempNoSide := Point{Quantity: "temp", Loop: "fws", Side: ""}
	if got := d.LintPoint(tempNoSide); len(got) != 0 {
		t.Errorf("expected no lint warnings for fws.temp (L47 makes it an error), got %v", got)
	}

	// L47: press is still lintable.
	pressWarn := d.LintPoint(mustPoint(t, d, "site01.pod002.rack000.tcs.press"))
	if len(pressWarn) != 1 {
		t.Errorf("expected 1 warning for tcs.press, got %d: %v", len(pressWarn), pressWarn)
	}

	if got := d.LintPoint(mustPoint(t, d, "site01.pod002.cdu000.fws.supply.flow")); len(got) != 0 {
		t.Errorf("expected no warnings for fws.supply.flow, got %v", got)
	}
	if got := d.LintPoint(mustPoint(t, d, "site01.pod003.busbar000.tou005.amp")); len(got) != 0 {
		t.Errorf("expected no warnings for tou005.amp, got %v", got)
	}
}

func mustPoint(t *testing.T, d *Dict, s string) Point {
	t.Helper()
	p, err := d.ParsePoint(s)
	if err != nil {
		t.Fatalf("ParsePoint(%q): %v", s, err)
	}
	return p
}
