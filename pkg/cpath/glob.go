package cpath

import (
	"errors"
	"regexp"
	"strings"
)

// Glob is a compiled CIOS path pattern. The zero value is not usable;
// construct one with CompileGlob.
//
// A Glob is immutable after construction and safe for concurrent reads.
type Glob struct {
	src   string        // original pattern string
	segs  []compiledSeg // compiled segments (literal-with-stars or globstar)
	isAll bool          // true iff segs == [{globstar}] (i.e. pattern is "**")
}

type compiledSeg struct {
	regex    *regexp.Regexp // nil for globstar segments
	globstar bool
}

// ErrGlobSyntax is returned by CompileGlob for any malformed pattern.
var ErrGlobSyntax = errors.New("cpath: bad glob pattern")

// CompileGlob compiles a CIOS path pattern.
//
// Pattern grammar (per spec-001 §2, spec-006 §5.3):
//
//   - The pattern is dot-separated into segments. The whole string must
//     match ^[a-z0-9.*]+$ with no empty segments (no leading/trailing dot,
//     no consecutive dots).
//   - Inside a segment, "*" matches zero or more [a-z0-9] characters.
//     A segment may contain several "*"s (e.g. "p*d*").
//   - "**" must occupy a whole segment by itself and matches zero or more
//     whole segments. It may appear multiple times.
//   - Any "*" not part of a sole "**" segment (e.g. "a**b", "***") is an
//     error.
//   - Patterns without any "*" are valid and match by string equality.
//
// CompileGlob is the only place regexps are compiled; Match performs no
// compilation and is safe to call concurrently on the same Glob.
func CompileGlob(pattern string) (Glob, error) {
	if pattern == "" {
		return Glob{}, ErrGlobSyntax
	}
	if !globPatternRe.MatchString(pattern) {
		return Glob{}, ErrGlobSyntax
	}
	if strings.Contains(pattern, "..") || pattern[0] == '.' || pattern[len(pattern)-1] == '.' {
		return Glob{}, ErrGlobSyntax
	}
	rawSegs := strings.Split(pattern, ".")
	segs := make([]compiledSeg, len(rawSegs))
	for i, s := range rawSegs {
		if s == "**" {
			segs[i] = compiledSeg{globstar: true}
			continue
		}
		if strings.Contains(s, "*") {
			// Verify every '*' is part of a valid in-segment run.
			// A "**" inside a non-solo segment is illegal (e.g. "a**b", "***").
			// Walk the string and reject any occurrence of two consecutive
			// '*' characters within this segment.
			if strings.Contains(s, "**") {
				return Glob{}, ErrGlobSyntax
			}
		}
		re, err := regexp.Compile(globSegmentToRegexp(s))
		if err != nil {
			// Should be unreachable given the ^[a-z0-9.*]+$ precheck.
			return Glob{}, ErrGlobSyntax
		}
		segs[i] = compiledSeg{regex: re}
	}
	g := Glob{src: pattern, segs: segs, isAll: len(segs) == 1 && segs[0].globstar}
	return g, nil
}

// Pattern returns the original pattern string passed to CompileGlob.
func (g Glob) Pattern() string { return g.src }

// Match reports whether path is fully matched by the compiled pattern,
// segment by segment. "**" segments match zero or more whole path segments.
//
// path is validated with a permissive check (lowest-common-denominator
// ^[a-z0-9.]+$ with no empty segments); an invalid path yields false
// without an error and without panicking.
func (g Glob) Match(path string) bool {
	if !globPathRe.MatchString(path) {
		return false
	}
	if strings.Contains(path, "..") || path[0] == '.' || path[len(path)-1] == '.' {
		return false
	}
	segs := strings.Split(path, ".")

	// Fast path: pure-literal pattern (no '*' at all) is a plain equality
	// check. This avoids the DP table for the common RBAC literal case.
	if !containsStar(g.src) {
		return g.src == path
	}

	// Fast path: pattern is the sole "**" — matches any non-empty path.
	if g.isAll {
		return true
	}

	// DP: dp[i][j] = pattern segments [0..i) match path segments [0..j).
	pLen := len(g.segs)
	sLen := len(segs)
	dp := make([][]bool, pLen+1)
	for i := range dp {
		dp[i] = make([]bool, sLen+1)
	}
	dp[0][0] = true

	for i := 0; i < pLen; i++ {
		seg := g.segs[i]
		if seg.globstar {
			// "**" segment: any number of whole path segments.
			// Once dp[i][j] is true, every dp[i+1][k] for k >= j is true
			// (zero, one, two, ... segments consumed). The first k for
			// which dp[i][j] && dp[i+1][j] is true is also the smallest
			// continuation point for the NEXT pattern segment.
			// Find the leftmost j with dp[i][j] == true; if none, skip.
			any := false
			leftmost := sLen + 1
			for j := 0; j <= sLen; j++ {
				if dp[i][j] {
					any = true
					if j < leftmost {
						leftmost = j
					}
				}
			}
			if any {
				// Zero-segment path: dp[i+1][leftmost] = true.
				dp[i+1][leftmost] = true
				// Also propagate to every later column (eating 1, 2, ...
				// segments) so the next pattern segment can match at any
				// of them. This is what makes "a.**.b.**.c" match
				// "a.x.b.y.z.c" — the second "**" needs to be able to
				// start from j=2 (after "b") and consume both "y" and "z".
				for k := leftmost; k <= sLen; k++ {
					dp[i+1][k] = true
				}
			}
			continue
		}
		for j := 0; j < sLen; j++ {
			if !dp[i][j] {
				continue
			}
			if seg.regex.MatchString(segs[j]) {
				dp[i+1][j+1] = true
			}
		}
	}
	return dp[pLen][sLen]
}

// --- internal helpers ---

// globPatternRe restricts the pattern alphabet to the documented set.
// It is intentionally NOT the path alphabet (which also forbids '*').
var globPatternRe = regexp.MustCompile(`^[a-z0-9.*]+$`)

// globPathRe is the path alphabet (per spec-001 §2): lowercase alnum and
// dots only. Validated upfront so Match can fail fast on garbage input
// without entering the DP.
var globPathRe = regexp.MustCompile(`^[a-z0-9.]+$`)

// containsStar reports whether s contains any '*' rune. Used to pick
// the fast literal path in Match.
func containsStar(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == '*' {
			return true
		}
	}
	return false
}

// globSegmentToRegexp converts an in-segment glob (containing only
// [a-z0-9] and '*') into a regexp matching that segment. Literal runs
// are passed through QuoteMeta; every '*' becomes [a-z0-9]*. The result
// is always anchored to the full segment.
func globSegmentToRegexp(seg string) string {
	var b strings.Builder
	b.Grow(len(seg) + 4)
	b.WriteByte('^')
	for i := 0; i < len(seg); i++ {
		c := seg[i]
		if c == '*' {
			b.WriteString("[a-z0-9]*")
			continue
		}
		// Non-star chars in this segment are guaranteed to be [a-z0-9]
		// by the precheck; QuoteMeta is a safe no-op on them.
		b.WriteString(regexp.QuoteMeta(string(c)))
	}
	b.WriteByte('$')
	return b.String()
}
