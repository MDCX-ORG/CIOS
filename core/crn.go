// Package core — crn.go: the RBAC-layer CRN wrapper over the spec-001
// §2 dot-path grammar (spec-004 §6bis). The dot-path grammar itself
// is unchanged; crn lives ONLY in core RBAC (PRMT-190 R2), never in
// pkg/cpath.
//
// CRN is the canonical RBAC resource name used during the v1.0→§6bis
// dual-grammar transition. A v1.0 dot-glob scope (e.g.
// "site01.chiller*") is normalized at load time to its CRN
// interpretation under the request token's tenant (PRMT-190 §4.2):
//
//	crn:tenant/<token-tenant>/org/default/site/site01/chiller*
//
// New tokens may also be authored in native CRN form, in which case
// their tid is the explicit tenant id and the §6bis cross-tenant red
// line is enforced at authorize time.
//
// Site-tail (everything from /site/<sid> onward) bijects to the
// spec-001 §2 dot path; that bijection is what lets us reuse
// pkg/cpath for site-tail matching without leaking CRN awareness
// into pkg/cpath (R2).
package core

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// ErrCRNSyntax is returned by ParseCRN for any malformed CRN. Callers
// wrap with fmt.Errorf("...: %w", ErrCRNSyntax) so they can match
// with errors.Is while still surfacing the offending input.
var ErrCRNSyntax = errors.New("core: bad crn")

// crn is the parsed RBAC-layer canonical resource name (spec-004
// §6bis). tid is the tenant id; oid the org name; sid the site slug;
// tail the in-site node path. Per §6bis each of tid/oid/sid/tail
// may be a literal (matching the §6 grammar [a-z][a-z0-9-]{1,30} or
// the §2 site alphabet) OR a glob segment ("*" single seg, "**" any
// depth, "prefix*" in-segment match — L50 semantics).
type crn struct {
	tid  string
	oid  string
	sid  string
	tail []string
}

// crnPrefix is the literal scheme marker (§6bis EBNF).
const crnPrefix = "crn:"

// Per-segment alphabet used by ParseCRN. crnTidOidRe is the §6bis
// tid/oid literal alphabet: lowercase letter, then lowercase alnum
// or hyphen, 2–31 chars total. crnSiteRe is the spec-001 §2 site
// alphabet (no hyphens, no dots) — used to validate the site slug
// and each in-site node segment after dot-splitting. crnNodeRe is
// the per-crn-<node> form: a node is either "*", "**", or a dotted
// string of spec-001 §2 segments (so "pod002.cdu000.fan000.rpm"
// parses as four node entries in the bijection, while "*.chiller*"
// is rejected because "*" is not a legal spec-001 segment by itself
// inside a dotted run — keep it split). crnGlobSegRe validates an
// in-segment glob like "p*" / "*d" / "p*d" — same shape as
// CompileGlob's in-segment grammar.
//
// Hyphens in tid/oid are admitted per §6bis; the spec-001 §2 dot
// path grammar that drives site-tail matching lives in pkg/cpath
// and does NOT permit hyphens in segment content. Hyphen-bearing
// tid/oid values therefore pass ParseCRN but the resulting site
// tail cannot match a real spec-001 §2 dot path — documented so
// forward-compatibility (future tenants/orgs with hyphens) does not
// silently bind a wrong site.
var (
	crnTidOidRe  = regexp.MustCompile(`^[a-z][a-z0-9-]{1,30}$`)
	crnSiteRe    = regexp.MustCompile(`^[a-z0-9*]+$`)
	crnNodeRe    = regexp.MustCompile(`^[a-z0-9]+(?:\.[a-z0-9*]+)*$|^\*\.\*\*$`) // either dotted §2 path or "*.**" catch-all
	crnGlobSegRe = regexp.MustCompile(`^[a-z0-9*]*\*[a-z0-9*]*$`)                // in-segment glob "p*" / "*d" / "p*d"
)

// crnIsGlobSeg reports whether seg is a valid §6 glob segment
// (single "*", whole-segment "**", or an in-segment prefix/suffix
// "p*" / "*d" / "p*d" pattern over the same alphabet CompileGlob
// accepts). Bare "**" is special (any depth); bare "*" matches a
// single whole segment; mixed "p*d" matches a single segment whose
// shape satisfies the in-segment run.
//
// The reject list mirrors CompileGlob: "a**b", "***" are illegal;
// segments starting with "-" are also illegal here because no glob
// form begins with "-" and the per-segment alphabet disallows it.
func crnIsGlobSeg(seg string) bool {
	if seg == "" {
		return false
	}
	switch seg {
	case "*", "**":
		return true
	}
	// Must start with [a-z0-9] or "*" (no leading hyphen).
	c := seg[0]
	if c != '*' && !(c >= 'a' && c <= 'z') && !(c >= '0' && c <= '9') {
		return false
	}
	if strings.Contains(seg, "**") {
		return false
	}
	// In-segment glob shape: at most one '*', every other char in
	// [a-z0-9]. Mirrors CompileGlob's glob-segment alphabet.
	for i := 1; i < len(seg); i++ {
		ch := seg[i]
		if ch == '*' {
			continue
		}
		if !((ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9')) {
			return false
		}
	}
	return true
}

// crnIsTidOrOid reports whether s is acceptable as a tid or oid
// value (literal only — glob segments are accepted by crnIsGlobSeg).
func crnIsTidOrOid(s string) bool {
	if crnIsGlobSeg(s) {
		return true
	}
	return crnTidOidRe.MatchString(s)
}

// ParseCRN parses a §6bis canonical resource name of the form
//
//	crn:tenant/<tid>/org/<oid>/site/<sid>(/<seg>)*
//
// Returns the structured crn or an error wrapping ErrCRNSyntax. The
// site-tail segments may be glob segments ("*", "**", "p*") per L50,
// in which case the bijection to a spec-001 §2 dot path is preserved
// verbatim (CompileGlob accepts the same alphabet; "**" stays "**").
//
// ParseCRN does NOT verify that <tid> matches any real tenant or
// that <sid> is a known site — those are DB concerns outside the
// grammar layer (PRMT-188 gives TenantFromContext; site org linkage
// is PRMT-189).
func ParseCRN(s string) (crn, error) {
	if !strings.HasPrefix(s, crnPrefix) {
		return crn{}, fmt.Errorf("%w: missing %q scheme: %q", ErrCRNSyntax, crnPrefix, s)
	}
	rest := s[len(crnPrefix):]
	if rest == "" {
		return crn{}, fmt.Errorf("%w: empty body: %q", ErrCRNSyntax, s)
	}
	parts := strings.Split(rest, "/")
	// Layout: tenant/<tid>/org/<oid>/site/<sid>(/<node>)* → ≥ 5 segments.
	if len(parts) < 5 {
		return crn{}, fmt.Errorf("%w: too few segments (need tenant/<tid>/org/<oid>/site/<sid>(/<node>)*): %q",
			ErrCRNSyntax, s)
	}
	if parts[0] != "tenant" {
		return crn{}, fmt.Errorf("%w: expected \"tenant\", got %q: %q",
			ErrCRNSyntax, parts[0], s)
	}
	if parts[2] != "org" {
		return crn{}, fmt.Errorf("%w: expected \"org\", got %q: %q",
			ErrCRNSyntax, parts[2], s)
	}
	if parts[4] != "site" {
		return crn{}, fmt.Errorf("%w: expected \"site\", got %q: %q",
			ErrCRNSyntax, parts[4], s)
	}
	if !crnIsTidOrOid(parts[1]) {
		return crn{}, fmt.Errorf("%w: bad tid %q: %q", ErrCRNSyntax, parts[1], s)
	}
	if !crnIsTidOrOid(parts[3]) {
		return crn{}, fmt.Errorf("%w: bad oid %q: %q", ErrCRNSyntax, parts[3], s)
	}
	// Site slug: a spec-001 §2 site literal (no dots, no hyphens). A
	// path like "fra01.pod002" is not a site; the dot belongs in the
	// <node>* tail. This guards the test case
	// "crn:tenant/acme/org/emea/site/fra01.pod002" (rejected).
	if len(parts) < 6 {
		return crn{}, fmt.Errorf("%w: missing site slug: %q", ErrCRNSyntax, s)
	}
	sid := parts[5]
	if !crnSiteRe.MatchString(sid) {
		return crn{}, fmt.Errorf("%w: bad site %q: %q", ErrCRNSyntax, sid, s)
	}
	tail := make([]string, 0, len(parts)-6)
	for _, seg := range parts[6:] {
		// Each <node> in the crn URL can itself be a dotted §2 path,
		// "**", "*", a glob-seg mix, or "*.**". We validate it then
		// split on '.' for the bijection so each §2 component is
		// individually addressable in crn.tail.
		if !crnValidNode(seg) {
			return crn{}, fmt.Errorf("%w: bad tail segment %q: %q", ErrCRNSyntax, seg, s)
		}
		tail = append(tail, strings.Split(seg, ".")...)
	}
	return crn{tid: parts[1], oid: parts[3], sid: sid, tail: tail}, nil
}

// crnValidNode reports whether seg is a valid crn <node>:
//   - a spec-001 §2 site literal ([a-z0-9]+)
//   - a single-segment glob ("*", "**")
//   - an in-segment glob ("p*", "*d", "p*d" — same shape CompileGlob accepts)
//   - a dotted run of any of the above (e.g. "pod002.cdu000.fan000.rpm")
//   - the catch-all "*.**"
func crnValidNode(seg string) bool {
	if seg == "" {
		return false
	}
	if seg == "*" || seg == "**" {
		return true
	}
	// Split on '.' and validate each piece.
	for _, piece := range strings.Split(seg, ".") {
		if piece == "*" || piece == "**" {
			continue
		}
		// spec-001 §2 literal.
		literalOK := true
		for i := 0; i < len(piece); i++ {
			c := piece[i]
			if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')) {
				literalOK = false
				break
			}
		}
		if literalOK {
			continue
		}
		// In-segment glob shape (one '*' max per CompileGlob convention).
		if !crnGlobSegRe.MatchString(piece) {
			return false
		}
		// Reject illegal glob patterns like "a**b" or "***".
		if strings.Contains(piece, "**") {
			return false
		}
	}
	return true
}

// dotPath renders the site tail as the spec-001 §2 dot path. The
// bijection is: crn "crn:tenant/.../site/fra01/pod002" ↔
// "fra01.pod002". The dot-path form is what we feed to
// cpath.CompileGlob so the Match() machinery in pkg/cpath stays
// crn-free (R2).
//
// When tail is empty the dot path is just the site slug (matches the
// "site" form of spec-001 §2 asset paths, which is also legal).
func (c crn) dotPath() string {
	if len(c.tail) == 0 {
		return c.sid
	}
	var b strings.Builder
	b.WriteString(c.sid)
	for _, seg := range c.tail {
		b.WriteByte('.')
		b.WriteString(seg)
	}
	return b.String()
}

// String renders the canonical crn form. Inverse of ParseCRN on the
// site tail (tid/oid/sid themselves are echoed as parsed).
func (c crn) String() string {
	var b strings.Builder
	b.WriteString(crnPrefix)
	b.WriteString("tenant/")
	b.WriteString(c.tid)
	b.WriteString("/org/")
	b.WriteString(c.oid)
	b.WriteString("/site/")
	b.WriteString(c.sid)
	for _, seg := range c.tail {
		b.WriteByte('/')
		b.WriteString(seg)
	}
	return b.String()
}

// legacyToCRN interprets a v1.0 dot-glob scope as its §6bis transition
// crn form (PRMT-190 §4.1):
//
//	"site01.chiller*"          → tid=tokenTenant, oid="default",
//	                              sid="site01", tail=["chiller*"]
//	"site01"                   → sid="site01", tail=[]
//	"site01.**"                → sid="site01", tail=["**"]
//
// tokenTenant is the request token's tenant (PRMT-188
// TenantFromContext). When tokenTenant is empty (no tenant attached
// — ops-realm admin posture, R1) the transition tid stays empty so
// the red line can distinguish "not yet tenant-bound" from
// "tenant-bound and the red line must enforce" (PRMT-190 §4.2
// fallback). The legacy dot-glob is preserved verbatim on the site
// tail so that cpath.CompileGlob sees exactly what the v1.0 caller
// wrote (the dot path grammar in pkg/cpath is unchanged).
//
// legacyToCRN does NOT validate that the dot-glob itself parses as
// a cpath glob; that validation lives in compilePrincipalScopes via
// CompileGlob, which already rejects malformed scopes at load time.
// legacyToCRN only splits the v1.0 dot path into (site, tail).
func legacyToCRN(dotGlob, tokenTenant string) crn {
	// Empty dot path — defensively produce an empty crn (will not
	// match anything; CompileGlob would have already rejected "").
	if dotGlob == "" {
		return crn{}
	}
	parts := strings.Split(dotGlob, ".")
	sid := parts[0]
	tail := make([]string, 0, len(parts)-1)
	if len(parts) > 1 {
		tail = append(tail, parts[1:]...)
	}
	return crn{
		tid:  tokenTenant, // "" is the sentinel for "not yet tenant-bound"
		oid:  "default",
		sid:  sid,
		tail: tail,
	}
}
