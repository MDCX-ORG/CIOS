// Tests for GET /v1/sla (PRMT-209).
package core

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestCustomerSLA_Get_Constant(t *testing.T) {
	_, ts := newTestServer(t)
	resp := doReq(t, ts, http.MethodGet, "/v1/sla", "")
	if resp.code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", resp.code, resp.body)
	}
	var got customerSLAResponse
	if err := json.Unmarshal([]byte(resp.body), &got); err != nil {
		t.Fatalf("json: %v body=%s", err, resp.body)
	}
	if got.TargetPct != 99.9 {
		t.Errorf("target_pct = %v, want 99.9", got.TargetPct)
	}
	if got.Window != "calendar_month" {
		t.Errorf("window = %q", got.Window)
	}
	if got.CreditNote != "display-only; no financial effect" {
		t.Errorf("credit_note = %q", got.CreditNote)
	}
}

func TestCustomerSLA_MethodNotAllowed(t *testing.T) {
	_, ts := newTestServer(t)
	resp := doReq(t, ts, http.MethodPost, "/v1/sla", `{}`)
	if resp.code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", resp.code)
	}
}

func TestCustomerSLA_RejectNoToken_WhenAuthOn(t *testing.T) {
	ts := newAuthCoverageTestServer(t)
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/sla", nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no-token status = %d, want 401", res.StatusCode)
	}
}
