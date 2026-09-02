// Package core — crn_test.go: unit tests for the spec-004 §6bis CRN
// parser + bijection. The dot-path grammar in pkg/cpath is exercised
// elsewhere; here we only assert the crn-side grammar and the crn ↔
// dot path bijection (PRMT-190 §5 MUSTs).
package core

import (
	"errors"
	"testing"
)

// crnValidCases are drawn directly from the §6bis examples in the
// PRMT body and from the PRMT-189 site-org-linkage seed data.
var crnValidCases = []struct {
	in       string
	wantTid  string
	wantOid  string
	wantSid  string
	wantTail []string
	wantDot  string
}{
	{
		// PRMT-190 §2-bis example, verbatim.
		in:       "crn:tenant/acme/org/emea/site/fra01/pod002",
		wantTid:  "acme",
		wantOid:  "emea",
		wantSid:  "fra01",
		wantTail: []string{"pod002"},
		wantDot:  "fra01.pod002",
	},
	{
		in:       "crn:tenant/acme/org/default/site/site01/chiller*",
		wantTid:  "acme",
		wantOid:  "default",
		wantSid:  "site01",
		wantTail: []string{"chiller*"},
		wantDot:  "site01.chiller*",
	},
	{
		in:       "crn:tenant/acme/org/emea/site/fra01/pod002.cdu000.fan000.rpm",
		wantTid:  "acme",
		wantOid:  "emea",
		wantSid:  "fra01",
		wantTail: []string{"pod002", "cdu000", "fan000", "rpm"},
		wantDot:  "fra01.pod002.cdu000.fan000.rpm",
	},
	{
		// Site-only (no tail) — should be a valid site-level crn
		// whose dot path is just the site slug.
		in:       "crn:tenant/acme/org/emea/site/fra01",
		wantTid:  "acme",
		wantOid:  "emea",
		wantSid:  "fra01",
		wantTail: nil,
		wantDot:  "fra01",
	},
	{
		// Glob segments in oid/tid (L50).
		in:       "crn:tenant/*/org/**/site/site01/**",
		wantTid:  "*",
		wantOid:  "**",
		wantSid:  "site01",
		wantTail: []string{"**"},
		wantDot:  "site01.**",
	},
	{
		// Single-seg wildcard in tail.
		in:       "crn:tenant/acme/org/emea/site/fra01/*",
		wantTid:  "acme",
		wantOid:  "emea",
		wantSid:  "fra01",
		wantTail: []string{"*"},
		wantDot:  "fra01.*",
	},
}

// TestParseCRN_Valid verifies the happy path: every §6bis-shaped
// string in the table parses to the expected (tid, oid, sid, tail).
func TestParseCRN_Valid(t *testing.T) {
	for _, tc := range crnValidCases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := ParseCRN(tc.in)
			if err != nil {
				t.Fatalf("ParseCRN(%q) err = %v", tc.in, err)
			}
			if got.tid != tc.wantTid {
				t.Errorf("tid = %q, want %q", got.tid, tc.wantTid)
			}
			if got.oid != tc.wantOid {
				t.Errorf("oid = %q, want %q", got.oid, tc.wantOid)
			}
			if got.sid != tc.wantSid {
				t.Errorf("sid = %q, want %q", got.sid, tc.wantSid)
			}
			if !equalSegs(got.tail, tc.wantTail) {
				t.Errorf("tail = %v, want %v", got.tail, tc.wantTail)
			}
		})
	}
}

// TestParseCRN_DotPathAndString_RoundTrip pins the crn ↔ dot path
// bijection (PRMT-190 §5 MUST #1): the dot path renders exactly as
// the site-tail form, and String() re-canonicalizes back to a crn
// whose ParseCRN → dotPath matches the original.
func TestParseCRN_DotPathAndString_RoundTrip(t *testing.T) {
	for _, tc := range crnValidCases {
		t.Run(tc.in, func(t *testing.T) {
			c, err := ParseCRN(tc.in)
			if err != nil {
				t.Fatalf("ParseCRN: %v", err)
			}
			if got := c.dotPath(); got != tc.wantDot {
				t.Errorf("dotPath() = %q, want %q", got, tc.wantDot)
			}
			round, err := ParseCRN(c.String())
			if err != nil {
				t.Fatalf("ParseCRN(String()): %v", err)
			}
			if round.dotPath() != c.dotPath() {
				t.Errorf("round-trip dotPath mismatch: %q → %q → %q",
					tc.in, c.String(), round.dotPath())
			}
		})
	}
}

// TestParseCRN_Invalid checks every malformed-input branch. The
// PRMT pins "returns an error (not a panic) on any malformed input"
// — these cases collectively guarantee that.
func TestParseCRN_Invalid(t *testing.T) {
	cases := []string{
		"",                                     // empty
		"site01.pod002",                        // legacy dot path, not crn:
		"crn:",                                 // scheme only
		"crn:tenant",                           // scheme + literal, no body
		"crn:tenant/acme",                      // too few segments
		"crn:tenant/acme/org/emea",             // still too few
		"crn:tenant/acme/org/emea/site",        // missing sid
		"crn:tenant/Acme/org/emea/site/fra01",  // uppercase tid
		"crn:tenant/-acme/org/emea/site/fra01", // leading hyphen tid
		"crn:tenant/acme/org/emea/site/FRA01",  // uppercase sid
		"crn:tenant/acme/org/emea/site/fra01.pod002", // dot in sid
		"crn:tenant/acme/org/emea/site/fra01/pod**",  // illegal "**" mid-segment
		"crn:site/acme/org/emea/site/fra01",          // wrong first label
		"crn:tenant/acme/team/emea/site/fra01",       // wrong third label
		"crn:tenant/acme/org/emea/host/fra01",        // wrong fourth label
	}
	for _, in := range cases {
		t.Run(in, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("ParseCRN(%q) panicked: %v", in, r)
				}
			}()
			c, err := ParseCRN(in)
			if err == nil {
				t.Fatalf("ParseCRN(%q) succeeded with %+v, want error", in, c)
			}
			if !errors.Is(err, ErrCRNSyntax) {
				t.Errorf("ParseCRN(%q) err = %v, want wrapping ErrCRNSyntax", in, err)
			}
		})
	}
}

// TestLegacyToCRN pins the §4.1 transition semantics: legacy
// dot-glob is split into (sid, tail) preserving the dot path
// verbatim, oid is "default", and tid is whatever the caller passes
// (empty for "not yet tenant-bound").
func TestLegacyToCRN(t *testing.T) {
	cases := []struct {
		dot        string
		token      string
		wantTid    string
		wantSid    string
		wantTail   []string
		wantDotOut string
	}{
		{
			dot:        "site01.chiller*",
			token:      "acme",
			wantTid:    "acme",
			wantSid:    "site01",
			wantTail:   []string{"chiller*"},
			wantDotOut: "site01.chiller*",
		},
		{
			dot:        "site01",
			token:      "acme",
			wantTid:    "acme",
			wantSid:    "site01",
			wantTail:   nil,
			wantDotOut: "site01",
		},
		{
			dot:        "site01.**",
			token:      "acme",
			wantTid:    "acme",
			wantSid:    "site01",
			wantTail:   []string{"**"},
			wantDotOut: "site01.**",
		},
		{
			dot:        "site01.pod002.**",
			token:      "", // ops-realm — no tenant attached
			wantTid:    "",
			wantSid:    "site01",
			wantTail:   []string{"pod002", "**"},
			wantDotOut: "site01.pod002.**",
		},
	}
	for _, tc := range cases {
		t.Run(tc.dot+"_tok="+tc.token, func(t *testing.T) {
			c := legacyToCRN(tc.dot, tc.token)
			if c.tid != tc.wantTid {
				t.Errorf("tid = %q, want %q", c.tid, tc.wantTid)
			}
			if c.oid != "default" {
				t.Errorf("oid = %q, want %q", c.oid, "default")
			}
			if c.sid != tc.wantSid {
				t.Errorf("sid = %q, want %q", c.sid, tc.wantSid)
			}
			if !equalSegs(c.tail, tc.wantTail) {
				t.Errorf("tail = %v, want %v", c.tail, tc.wantTail)
			}
			if got := c.dotPath(); got != tc.wantDotOut {
				t.Errorf("dotPath() = %q, want %q", got, tc.wantDotOut)
			}
		})
	}
}

// equalSegs is a small nil-safe slice equality helper used by the
// parse-round-trip tests; stdlib's reflect.DeepEqual treats nil and
// []string{} as different which would make the table-driven tests
// flaky on the site-only (no tail) case.
func equalSegs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
