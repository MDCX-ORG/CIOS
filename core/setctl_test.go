package core

import (
	"net/http"
	"testing"
)

func TestLookupRiskClass_L108(t *testing.T) {
	cases := []struct {
		path string
		ok   bool
		cls  RiskClass
	}{
		{"sgp01.pod000.chiller000.compressor.status", true, RiskClassA},
		{"sgp01.site.chiller.fws.supply.temp", true, RiskClassA},
		{"sgp01.pod000.cdu000.pump.rpm", true, RiskClassA},
		{"sgp01.pod000.drycooler000.fan.rpm", true, RiskClassB},
		{"site01.pod000.cdu000.tcs.opening", true, RiskClassB},
		{"sgp01.pod000.node000.gpu.clock", true, RiskClassC},
		{"sgp01.pod000.cdu000.fws.supply.flow", false, ""}, // not on allow-list
		{"sgp01.pod000.something.maintenance.status", true, RiskClassC},
	}
	for _, tc := range cases {
		cls, ok := LookupRiskClass(tc.path)
		if ok != tc.ok {
			t.Errorf("%s: ok=%v want %v", tc.path, ok, tc.ok)
			continue
		}
		if ok && cls != tc.cls {
			t.Errorf("%s: class=%q want %q", tc.path, cls, tc.cls)
		}
	}
}

func TestEvaluateSetPolicy_Gates(t *testing.T) {
	pathA := "sgp01.pod000.chiller000.compressor.status"
	// default ro
	if _, code, _ := EvaluateSetPolicy("sgp01.pod000.cdu000.fws.supply.flow", "alice", SetRequest{TTLSeconds: 30}); code != http.StatusForbidden {
		t.Fatalf("ro point: code=%d", code)
	}
	// A: the policy gate itself now passes with no body
	// second_approver — dual approval is enforced by the two-phase
	// pending flow (PRMT-235), not by this string field.
	if _, code, _ := EvaluateSetPolicy(pathA, "alice", SetRequest{TTLSeconds: 30, Value: 1}); code != 0 {
		t.Fatalf("A single-phase gate: code=%d", code)
	}
	// B needs readback
	pathB := "sgp01.pod000.drycooler000.fan.rpm"
	if _, code, _ := EvaluateSetPolicy(pathB, "alice", SetRequest{TTLSeconds: 30, Value: 50}); code != http.StatusBadRequest {
		t.Fatalf("B without readback: code=%d", code)
	}
	if _, code, _ := EvaluateSetPolicy(pathB, "alice", SetRequest{TTLSeconds: 30, Value: 50, RequireReadback: true}); code != 0 {
		t.Fatalf("B ok: code=%d", code)
	}
	// C single
	pathC := "sgp01.pod000.node000.gpu.clock"
	if _, code, _ := EvaluateSetPolicy(pathC, "alice", SetRequest{TTLSeconds: 10, Value: 1200}); code != 0 {
		t.Fatalf("C ok: code=%d", code)
	}
	// missing actor
	if _, code, _ := EvaluateSetPolicy(pathC, "", SetRequest{TTLSeconds: 10}); code != http.StatusUnauthorized {
		t.Fatalf("no actor: code=%d", code)
	}
}
