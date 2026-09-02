package cpath

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Node is one typed step in an asset path.
type Node struct {
	Type  string
	Index string // raw literal, e.g. "002" or "0"
}

// AssetPath identifies a physical asset slot.
type AssetPath struct {
	Site  string
	Nodes []Node
}

// Point identifies a telemetry point: asset + optional location + quantity.
type Point struct {
	Asset     AssetPath
	Loop      string
	LoopIndex int // -1 when no index
	Side      string
	Phase     string
	Quantity  string
}

// Sentinel errors. Wrap with fmt.Errorf("...%q...: %w", seg, Err) so callers
// can match with errors.Is and still read the offending segment.
var (
	ErrSyntax          = errors.New("cpath: syntax error")
	ErrBadIndex        = errors.New("cpath: bad index")
	ErrBadParent       = errors.New("cpath: illegal parent")
	ErrUnknownSegment  = errors.New("cpath: unknown segment")
	ErrUnknownQuantity = errors.New("cpath: unknown quantity")
	ErrBadHost         = errors.New("cpath: derived host violation")
	ErrMissingSide     = errors.New("cpath: water-loop temp requires side") // L47
)

// --- pre-compiled patterns -------------------------------------------------

var (
	reOverall   = regexp.MustCompile(`^[a-z0-9.]+$`)
	reSite      = regexp.MustCompile(`^[a-z]{2,8}[0-9]{2}$`)
	reNode      = regexp.MustCompile(`^([a-z]+)([0-9]+)$`)
	reLoopName  = regexp.MustCompile(`^([a-z]+)([0-9])?$`)
	reChipIndex = regexp.MustCompile(`^(0|[1-9][0-9]{0,2})$`)
)

// ParseAssetPath parses and validates an asset path (no location, no quantity).
func (d *Dict) ParseAssetPath(s string) (AssetPath, error) {
	if !reOverall.MatchString(s) || strings.Contains(s, "..") {
		return AssetPath{}, fmt.Errorf("%w: %q", ErrSyntax, s)
	}
	parts := strings.Split(s, ".")
	if len(parts) < 2 || parts[0] == "" || parts[len(parts)-1] == "" {
		return AssetPath{}, fmt.Errorf("%w: %q", ErrSyntax, s)
	}
	site := parts[0]
	if !reSite.MatchString(site) || strings.HasSuffix(site, "00") {
		return AssetPath{}, fmt.Errorf("%w: site %q", ErrSyntax, site)
	}

	var nodes []Node
	prevType := "site"
	for _, seg := range parts[1:] {
		m := reNode.FindStringSubmatch(seg)
		if m == nil {
			// Per §4.4 R3: a non-node segment is rejected as unknown unless
			// it is a bare type name (alpha-only segment that is itself a
			// registered type), which is a syntax error.
			if _, isType := d.Types[seg]; isType {
				return AssetPath{}, fmt.Errorf("%w: bare type %q without index", ErrSyntax, seg)
			}
			return AssetPath{}, fmt.Errorf("%w: %q", ErrUnknownSegment, seg)
		}
		typeName, idx := m[1], m[2]
		def, ok := d.Types[typeName]
		if !ok {
			return AssetPath{}, fmt.Errorf("%w: %q", ErrUnknownSegment, seg)
		}
		if err := checkIndex(def, idx, seg); err != nil {
			return AssetPath{}, err
		}
		if !containsString(def.Parents, prevType) {
			return AssetPath{}, fmt.Errorf("%w: %q cannot follow %q",
				ErrBadParent, typeName, prevType)
		}
		nodes = append(nodes, Node{Type: typeName, Index: idx})
		prevType = typeName
	}
	return AssetPath{Site: site, Nodes: nodes}, nil
}

// ParsePoint parses and validates a full point address.
func (d *Dict) ParsePoint(s string) (Point, error) {
	if !reOverall.MatchString(s) || strings.Contains(s, "..") {
		return Point{}, fmt.Errorf("%w: %q", ErrSyntax, s)
	}
	parts := strings.Split(s, ".")
	if len(parts) < 2 || parts[0] == "" || parts[len(parts)-1] == "" {
		return Point{}, fmt.Errorf("%w: %q", ErrSyntax, s)
	}
	site := parts[0]
	if !reSite.MatchString(site) || strings.HasSuffix(site, "00") {
		return Point{}, fmt.Errorf("%w: site %q", ErrSyntax, site)
	}

	// --- node pass: consume typed nodes from segment 1 onward (including
	// a possible last segment) until a non-node segment appears.
	asset := AssetPath{Site: site}
	i := 1
	prevType := "site"
	for ; i < len(parts); i++ {
		seg := parts[i]
		m := reNode.FindStringSubmatch(seg)
		if m == nil {
			// Either a known type without an index (e.g. trailing "pod.") or
			// a real location/quantity. The prompt §4.4 disallows "pure
			// alpha segment that is itself a type" → ErrSyntax.
			if _, isType := d.Types[seg]; isType {
				return Point{}, fmt.Errorf("%w: bare type %q without index", ErrSyntax, seg)
			}
			break
		}
		typeName, idx := m[1], m[2]
		def, ok := d.Types[typeName]
		if !ok {
			break // hand off to location/quantity pass
		}
		if err := checkIndex(def, idx, seg); err != nil {
			return Point{}, err
		}
		if !containsString(def.Parents, prevType) {
			return Point{}, fmt.Errorf("%w: %q cannot follow %q",
				ErrBadParent, typeName, prevType)
		}
		asset.Nodes = append(asset.Nodes, Node{Type: typeName, Index: idx})
		prevType = typeName
	}

	// --- location pass: stage state machine (Round 1 revision).
	// stage ∈ {0,1,2,3} where 0/1/2 are "next acceptable category" and 3
	// means location pass is done. A segment is classified into one of
	// loop=0, side=1, phase=2. If its category cat is < stage the segment
	// is an inversion/duplicate → ErrUnknownSegment; otherwise it is
	// consumed and stage = cat+1. This accepts any combination of the
	// three categories in non-decreasing order (any of them may be
	// omitted) and rejects both inversion and repetition.
	var loop, side, phase string
	loopIndex := -1
	stage := 0
	for ; i < len(parts)-1; i++ {
		seg := parts[i]
		cat := -1
		// loop candidate: name+optional digit, name must be in Loops.
		if lm := reLoopName.FindStringSubmatch(seg); lm != nil {
			if d.Loops[lm[1]] {
				cat = 0
				if lm[2] != "" {
					n, _ := strconv.Atoi(lm[2])
					loopIndex = n
				}
				loop = lm[1]
			}
		}
		if cat < 0 && d.Sides[seg] {
			cat = 1
			side = seg
		}
		if cat < 0 && d.Phases[seg] {
			cat = 2
			phase = seg
		}
		if cat < 0 {
			return Point{}, fmt.Errorf("%w: %q", ErrUnknownSegment, seg)
		}
		if cat < stage {
			return Point{}, fmt.Errorf("%w: %q", ErrUnknownSegment, seg)
		}
		stage = cat + 1
	}

	// --- quantity pass: last segment must be a known quantity. If the
	// node pass already consumed the last segment, there is no quantity.
	if i >= len(parts) {
		return Point{}, fmt.Errorf("%w: missing quantity in %q", ErrSyntax, s)
	}
	qSeg := parts[len(parts)-1]
	// L48: site-level address (no nodes) requires a derived quantity.
	if len(asset.Nodes) == 0 {
		if _, isDerived := d.Derived[qSeg]; !isDerived {
			return Point{}, fmt.Errorf("%w: site-level address requires derived quantity, got %q",
				ErrBadHost, qSeg)
		}
	}
	// L47: water-loop temp without a side is an error (not a warning).
	if loop != "" && side == "" && qSeg == "temp" {
		return Point{}, fmt.Errorf("%w: %q", ErrMissingSide, s)
	}
	if _, ok := d.Quantities[qSeg]; !ok {
		if def, okD := d.Derived[qSeg]; okD {
			// Derived host check. L48 above rejects site-level
			// (len(asset.Nodes)==0) addresses outright unless qSeg is
			// derived, so by the time we reach this branch the quantity
			// is both derived AND has at least one node. Belt-and-braces
			// guard kept in case asset.Nodes is empty for any reason.
			if len(asset.Nodes) == 0 {
				if !containsString(def.Host, "site") {
					return Point{}, fmt.Errorf("%w: %q requires site-level host",
						ErrBadHost, qSeg)
				}
			} else {
				last := asset.Nodes[len(asset.Nodes)-1].Type
				if !containsString(def.Host, last) {
					return Point{}, fmt.Errorf("%w: %q not allowed on %q",
						ErrBadHost, qSeg, last)
				}
			}
		} else {
			return Point{}, fmt.Errorf("%w: %q", ErrUnknownQuantity, qSeg)
		}
	}

	return Point{
		Asset:     asset,
		Loop:      loop,
		LoopIndex: loopIndex,
		Side:      side,
		Phase:     phase,
		Quantity:  qSeg,
	}, nil
}

// LintPoint returns non-blocking warnings. Only one rule per spec-002 §1:
// water-loop quantities without a side are reported as a warning. L47
// upgraded temp to a hard error in ParsePoint, so the lint rule no longer
// fires on temp; only flow/press remain as lintable quantities.
func (d *Dict) LintPoint(p Point) []string {
	if p.Loop == "" || p.Side != "" {
		return nil
	}
	switch p.Quantity {
	case "flow", "press":
		return []string{
			fmt.Sprintf("water-loop quantity %s should carry side (supply/return)", p.Quantity),
		}
	}
	return nil
}

// String renders the asset path back to its canonical dotted form.
func (p AssetPath) String() string {
	var b strings.Builder
	b.WriteString(p.Site)
	for _, n := range p.Nodes {
		b.WriteByte('.')
		b.WriteString(n.Type)
		b.WriteString(n.Index)
	}
	return b.String()
}

// String renders the point back to its canonical dotted form.
func (p Point) String() string {
	var b strings.Builder
	b.WriteString(p.Asset.String())
	if p.Loop != "" {
		b.WriteByte('.')
		b.WriteString(p.Loop)
		if p.LoopIndex >= 0 {
			b.WriteString(strconv.Itoa(p.LoopIndex))
		}
	}
	if p.Side != "" {
		b.WriteByte('.')
		b.WriteString(p.Side)
	}
	if p.Phase != "" {
		b.WriteByte('.')
		b.WriteString(p.Phase)
	}
	b.WriteByte('.')
	b.WriteString(p.Quantity)
	return b.String()
}

// --- helpers ---------------------------------------------------------------

func checkIndex(def TypeDef, idx, seg string) error {
	switch def.Level {
	case LevelDevice:
		if len(idx) != 3 {
			return fmt.Errorf("%w: %q (device index must be 3 digits)", ErrBadIndex, seg)
		}
	case LevelChip:
		if !reChipIndex.MatchString(idx) {
			return fmt.Errorf("%w: %q (chip index has leading zero or >3 digits)", ErrBadIndex, seg)
		}
	default:
		// Unknown level → treat as bad index to surface drift early.
		return fmt.Errorf("%w: %q has unknown level", ErrBadIndex, seg)
	}
	return nil
}

func containsString(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
