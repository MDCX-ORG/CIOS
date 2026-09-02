package cpath

import (
	"errors"
	"sync"
	"testing"
)

// TestGlobMatchTable drives the §5用例表 end-to-end: compile each pattern,
// run Match against the path, assert the expected verdict.
func TestGlobMatchTable(t *testing.T) {
	cases := []struct {
		pattern string
		path    string
		want    bool
	}{
		// site01.**.gpu* family
		{"site01.**.gpu*", "site01.pod000.tank003.node012.gpu0", true},
		{"site01.**.gpu*", "site01.pod000.tank003.node012.gpu0.temp", false},
		{"site01.**.gpu*", "site01.gpu0", true}, // ** matches zero segments

		// site01.chiller* (no trailing **)
		{"site01.chiller*", "site01.chiller002", true},
		{"site01.chiller*", "site01.chiller002.pump000", false},

		// explicit dot-count pattern
		{"site01.*.cdu000.fws.supply.flow", "site01.pod002.cdu000.fws.supply.flow", true},

		// single-segment "*" must NOT cross a dot
		{"site01.*", "site01.pod000.tank003", false},

		// pure "**"
		{"**", "site01", true},
		{"**", "site01.pod000.tank003.node012.gpu0.temp", true},

		// trailing "**"
		{"site01.**", "site01", true},

		// multiple "**"
		{"a.**.b.**.c", "a.x.b.y.z.c", true},
		{"a.**.b.**.c", "a.b.c", true},

		// in-segment multi-star
		{"p*d*", "pod002", true},
		{"p*d*", "pad", true},
		{"p*d*", "pdu000", true}, // 'p' + '*'="" + 'd' + '*'="u000"

		// '*' must be zero-or-more a-z0-9, so "p*d" cannot end in 'u000'
		{"p*d", "pdu000", false},
	}
	for _, tc := range cases {
		t.Run(tc.pattern+"|"+tc.path, func(t *testing.T) {
			g, err := CompileGlob(tc.pattern)
			if err != nil {
				t.Fatalf("compile %q: %v", tc.pattern, err)
			}
			if g.Pattern() != tc.pattern {
				t.Errorf("Pattern() = %q, want %q", g.Pattern(), tc.pattern)
			}
			if got := g.Match(tc.path); got != tc.want {
				t.Errorf("Match(%q) on %q = %v, want %v", tc.path, tc.pattern, got, tc.want)
			}
		})
	}
}

// TestGlobInvalidPatterns covers §5's 8 illegal patterns. All must fail
// at compile time with errors.Is(err, ErrGlobSyntax) == true.
func TestGlobInvalidPatterns(t *testing.T) {
	bad := []string{
		"a**b", // '*' run inside a non-solo segment
		"***",  // triple star, not a solo "**"
		"a..b", // empty segment
		".a",   // leading dot
		"a.",   // trailing dot
		"",     // empty
		"A.*",  // uppercase forbidden
		"a.*?", // '?' is not in the alphabet
	}
	for _, p := range bad {
		t.Run(p, func(t *testing.T) {
			_, err := CompileGlob(p)
			if err == nil {
				t.Fatalf("CompileGlob(%q) succeeded, want error", p)
			}
			if !errors.Is(err, ErrGlobSyntax) {
				t.Fatalf("CompileGlob(%q) err = %v, want errors.Is(.., ErrGlobSyntax)", p, err)
			}
		})
	}
}

// TestGlobInvalidPaths: Match must return false (never panic, never
// error) when given a malformed path.
func TestGlobInvalidPaths(t *testing.T) {
	// Use a generous "match anything but must be queried" pattern.
	g, err := CompileGlob("**")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	bad := []string{
		"Foo.bar", // uppercase
		"a..b",    // empty segment
		"",        // empty
		".a",      // leading dot
		"a.",      // trailing dot
	}
	for _, p := range bad {
		t.Run(p, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("Match(%q) panicked: %v", p, r)
				}
			}()
			if g.Match(p) {
				t.Errorf("Match(%q) = true, want false", p)
			}
		})
	}
}

// TestGlobRace runs Match from many goroutines against a shared compiled
// Glob. With `go test -race`, any data race on g.segs / g.src / the regexp
// pointers will be reported. The Glob contract says reads are safe.
func TestGlobRace(t *testing.T) {
	g, err := CompileGlob("site01.**.gpu*")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	paths := []string{
		"site01.pod000.tank003.node012.gpu0",
		"site01.gpu0",
		"site01.pod000.cdu000.fws.supply.flow",
		"site01",
		"site01.pod000.tank003.node012.gpu0.temp",
	}
	const n = 10
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(seed int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				_ = g.Match(paths[(seed+j)%len(paths)])
			}
		}(i)
	}
	wg.Wait()
}

// TestGlobLiteralFastPath: a pattern with no '*' is a pure equality check.
// This guards the fast path: even if a future refactor changes the
// per-segment regex, literals must still match by equality.
func TestGlobLiteralFastPath(t *testing.T) {
	g, err := CompileGlob("site01.pod002.cdu000.fws.supply.flow")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if !g.Match("site01.pod002.cdu000.fws.supply.flow") {
		t.Error("literal exact: want true")
	}
	if g.Match("site01.pod002.cdu000.fws.return.flow") {
		t.Error("literal different: want false")
	}
	if g.Match("site01") {
		t.Error("literal shorter: want false")
	}
}
