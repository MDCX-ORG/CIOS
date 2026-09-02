// pkg/tenant/org_test.go — table-driven tests for Org claim
// resolution and site-set membership. PRMT-110 §7 acceptance.
//
// Coverage map (every MUST from §5 has at least one test):
//   - happy org + sites round-trip (TestOrgFromClaims_Happy)
//   - missing org / empty org → fail-closed
//   - missing sites / empty sites → fail-closed
//   - whitespace-only / duplicate site entries are cleaned
//   - SiteAllowed: in-set allow, out-of-set deny, empty target
//     deny, empty site set deny (fail-closed)
package tenant

import (
	"testing"

	"github.com/yurimeng/cios/pkg/sts"
)

func TestOrgFromClaims_Happy(t *testing.T) {
	org, sites, ok := OrgFromClaims(sts.TokenClaims{
		Subject: "alice@example.com",
		Org:     "acme",
		Sites:   []string{"sgp01", "sgp02"},
	})
	if !ok {
		t.Fatalf("ok = false, want true")
	}
	if org != "acme" {
		t.Errorf("org = %q, want acme", org)
	}
	if len(sites) != 2 || sites[0] != "sgp01" || sites[1] != "sgp02" {
		t.Errorf("sites = %v, want [sgp01 sgp02]", sites)
	}
}

func TestOrgFromClaims_CleansWhitespaceAndDuplicates(t *testing.T) {
	org, sites, ok := OrgFromClaims(sts.TokenClaims{
		Subject: "alice",
		Org:     "  acme  ",
		Sites:   []string{" sgp01 ", "", "sgp01", "sgp02", "  "},
	})
	if !ok {
		t.Fatalf("ok = false, want true")
	}
	if org != "acme" {
		t.Errorf("org = %q, want acme (whitespace trimmed)", org)
	}
	if len(sites) != 2 || sites[0] != "sgp01" || sites[1] != "sgp02" {
		t.Errorf("sites = %v, want [sgp01 sgp02] (whitespace + dupes dropped)", sites)
	}
}

func TestOrgFromClaims_FailClosed(t *testing.T) {
	cases := []struct {
		name string
		c    sts.TokenClaims
	}{
		{
			name: "missing org",
			c:    sts.TokenClaims{Subject: "alice", Sites: []string{"sgp01"}},
		},
		{
			name: "empty org",
			c:    sts.TokenClaims{Subject: "alice", Org: "", Sites: []string{"sgp01"}},
		},
		{
			name: "whitespace org",
			c:    sts.TokenClaims{Subject: "alice", Org: "   ", Sites: []string{"sgp01"}},
		},
		{
			name: "missing sites",
			c:    sts.TokenClaims{Subject: "alice", Org: "acme"},
		},
		{
			name: "empty sites",
			c:    sts.TokenClaims{Subject: "alice", Org: "acme", Sites: []string{}},
		},
		{
			name: "all-whitespace sites",
			c:    sts.TokenClaims{Subject: "alice", Org: "acme", Sites: []string{"", "   "}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			org, sites, ok := OrgFromClaims(tc.c)
			if ok {
				t.Errorf("ok = true (org=%q sites=%v), want false (fail-closed)", org, sites)
			}
			if org != "" {
				t.Errorf("org = %q, want \"\" on fail-closed", org)
			}
			if sites != nil {
				t.Errorf("sites = %v, want nil on fail-closed", sites)
			}
		})
	}
}

func TestSiteAllowed(t *testing.T) {
	sites := []string{"sgp01", "sgp02"}
	cases := []struct {
		name   string
		sites  []string
		target string
		want   bool
	}{
		{name: "in set", sites: sites, target: "sgp01", want: true},
		{name: "other in set", sites: sites, target: "sgp02", want: true},
		{name: "out of set", sites: sites, target: "sgp09", want: false},
		{name: "empty target", sites: sites, target: "", want: false},
		{name: "whitespace target", sites: sites, target: "   ", want: false},
		{name: "empty sites fail-closed", sites: nil, target: "sgp01", want: false},
		{name: "empty slice fail-closed", sites: []string{}, target: "sgp01", want: false},
		{name: "case sensitive", sites: sites, target: "SGP01", want: false},
		{name: "target with surrounding whitespace",
			sites: sites, target: "  sgp01  ", want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := SiteAllowed(tc.sites, tc.target); got != tc.want {
				t.Errorf("SiteAllowed(%v, %q) = %v, want %v",
					tc.sites, tc.target, got, tc.want)
			}
		})
	}
}
