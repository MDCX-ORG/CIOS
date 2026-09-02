// pkg/tenant/claim_test.go — table-driven tests for tier parsing
// and tenant claim resolution. PRMT-109 §7 acceptance.
package tenant

import (
	"errors"
	"testing"

	"github.com/yurimeng/cios/pkg/sts"
)

func TestParseTier_Accept(t *testing.T) {
	for _, want := range []Tier{TierDB, TierRow, TierLabel} {
		got, err := ParseTier(string(want))
		if err != nil {
			t.Errorf("ParseTier(%q): %v", want, err)
			continue
		}
		if got != want {
			t.Errorf("ParseTier(%q) = %q, want %q", want, got, want)
		}
	}
}

func TestParseTier_Reject(t *testing.T) {
	cases := []string{
		"",   // empty (PRMT-109 §5: tier 缺失 → 403 fail-closed)
		"DB", // case sensitivity is deliberate
		"Row",
		"LABEL",
		"database", // alias
		"row ",     // trailing space
		" row",     // leading space
		"none",
		"admin",
		"db/row",
	}
	for _, in := range cases {
		_, err := ParseTier(in)
		if err == nil {
			t.Errorf("ParseTier(%q): nil err, want error", in)
			continue
		}
		if !errors.Is(err, ErrInvalidTier) {
			t.Errorf("ParseTier(%q): err = %v, want ErrInvalidTier", in, err)
		}
	}
}

func TestTenantFromClaims_Happy(t *testing.T) {
	for _, tier := range []Tier{TierDB, TierRow, TierLabel} {
		id, got, ok := TenantFromClaims(sts.TokenClaims{
			Subject:       "alice@example.com",
			Tenant:        "acme",
			IsolationTier: string(tier),
		})
		if !ok {
			t.Fatalf("tier %s: ok = false, want true", tier)
		}
		if id != "acme" {
			t.Errorf("tier %s: id = %q, want acme", tier, id)
		}
		if got != tier {
			t.Errorf("tier %s: got = %q, want %q", tier, got, tier)
		}
	}
}

func TestTenantFromClaims_FailClosed(t *testing.T) {
	cases := []struct {
		name string
		c    sts.TokenClaims
	}{
		{
			name: "missing tenant",
			c:    sts.TokenClaims{Subject: "alice", IsolationTier: "label"},
		},
		{
			name: "missing tier",
			c:    sts.TokenClaims{Subject: "alice", Tenant: "acme"},
		},
		{
			name: "invalid tier",
			c:    sts.TokenClaims{Subject: "alice", Tenant: "acme", IsolationTier: "database"},
		},
		{
			name: "empty tenant",
			c:    sts.TokenClaims{Subject: "alice", Tenant: "", IsolationTier: "label"},
		},
		{
			name: "whitespace tenant",
			c:    sts.TokenClaims{Subject: "alice", Tenant: "   ", IsolationTier: "label"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id, tier, ok := TenantFromClaims(tc.c)
			if ok {
				t.Errorf("ok = true (id=%q tier=%q), want false (fail-closed)", id, tier)
			}
		})
	}
}

// TestTenantFromClaims_TierConstantsPresent is the §7 grep target:
// all three tiers must be wired through TierFromClaims → ParseTier →
// the constants. The grep in §7 (`grep -rn "TierDB\|TierRow\|TierLabel"`)
// is the canonical check; this test pins the constants' values for
// extra coverage.
func TestTenantFromClaims_TierConstantsPresent(t *testing.T) {
	if string(TierDB) != "db" {
		t.Errorf("TierDB = %q, want db", TierDB)
	}
	if string(TierRow) != "row" {
		t.Errorf("TierRow = %q, want row", TierRow)
	}
	if string(TierLabel) != "label" {
		t.Errorf("TierLabel = %q, want label", TierLabel)
	}
}

// TestTenantPropagationHeader_FixedContract pins the header name
// (X-CIOS-Tenant) and the value passthrough. spec-004 §6 is silent
// on tenant propagation; PRMT-109 §8 flags this as an open spec
// point — the contract here MUST be reviewed by the architect.
func TestTenantPropagationHeader_FixedContract(t *testing.T) {
	name, value := TenantPropagationHeader("acme")
	if name != "X-CIOS-Tenant" {
		t.Errorf("name = %q, want X-CIOS-Tenant", name)
	}
	if value != "acme" {
		t.Errorf("value = %q, want acme", value)
	}
	// And the constant the spec site points at is the same one.
	if TenantHeaderName != "X-CIOS-Tenant" {
		t.Errorf("TenantHeaderName = %q, want X-CIOS-Tenant", TenantHeaderName)
	}
}
