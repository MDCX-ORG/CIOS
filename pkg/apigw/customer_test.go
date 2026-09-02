// Tests for GET /api/customer/status and /api/customer/sla (PRMT-208).
package apigw

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yurimeng/cios/pkg/sts"
)

var customerTestClaims = sts.TokenClaims{
	Subject:       "customer@example.com",
	Realm:         "ops",
	Tenant:        "acme",
	IsolationTier: "label",
}

func customerCtx(raw string) func(*http.Request) *http.Request {
	return func(r *http.Request) *http.Request {
		ctx := WithClaims(r.Context(), customerTestClaims)
		if raw != "" {
			ctx = WithRawToken(ctx, raw)
		}
		return r.WithContext(ctx)
	}
}

// customerUpstream routes /v1/alarms and /v1/sites (and optionally /v1/sla, /v1/usage).
func customerUpstream(t *testing.T, alarmsBody, sitesBody string, slaStatus int, slaBody string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/alarms":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, alarmsBody)
		case "/v1/sites":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, sitesBody)
		case "/v1/sla":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(slaStatus)
			_, _ = io.WriteString(w, slaBody)
		case "/v1/usage":
			// Echo tenant_id so tests can assert force-scoping.
			tid := r.URL.Query().Get("tenant_id")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"items":[{"id":"us_1","tenant_id":"`+tid+`","kind":"energy","quantity":1,"unit":"kWh"}]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestHandleCustomerStatus_HappyPath(t *testing.T) {
	alarms := `{"items":[
		{"path":"sgp01.pod000.cdu000","severity":"major","state":"firing"},
		{"path":"sjc01.pod001","severity":"critical","state":"firing"},
		{"path":"sgp01.pod001","severity":"minor","state":"resolved"}
	]}`
	sites := `{"items":[{"id":"sgp01"},{"id":"sjc01"},{"id":"ord01"}]}`
	upstream := customerUpstream(t, alarms, sites, 404, "")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	r := httptest.NewRequest(http.MethodGet, "/api/customer/status", nil)
	r = customerCtx("tok")(r)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var got customerStatusResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("json: %v", err)
	}
	if got.TenantID != "acme" {
		t.Errorf("tenant_id = %q, want acme", got.TenantID)
	}
	if got.AsOf == "" {
		t.Error("as_of empty")
	}
	byID := map[string]customerSiteStatus{}
	for _, s := range got.Sites {
		byID[s.ID] = s
	}
	if s, ok := byID["sgp01"]; !ok || s.Health != "yellow" || s.OpenAlarms != 1 {
		t.Errorf("sgp01 = %+v, want yellow open=1", s)
	}
	if s, ok := byID["sjc01"]; !ok || s.Health != "red" || s.OpenAlarms != 1 {
		t.Errorf("sjc01 = %+v, want red open=1", s)
	}
	if s, ok := byID["ord01"]; !ok || s.Health != "green" || s.OpenAlarms != 0 {
		t.Errorf("ord01 = %+v, want green open=0", s)
	}
}

func TestHandleCustomerStatus_MissingTenant_403(t *testing.T) {
	upstream := customerUpstream(t, `{"items":[]}`, `{"items":[]}`, 404, "")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	claims := sts.TokenClaims{Subject: "x", Realm: "ops"} // no tenant
	r := httptest.NewRequest(http.MethodGet, "/api/customer/status", nil)
	r = r.WithContext(WithClaims(r.Context(), claims))
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
}

func TestHandleCustomerStatus_MethodNotAllowed(t *testing.T) {
	upstream := customerUpstream(t, `{"items":[]}`, `{"items":[]}`, 404, "")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))
	r := httptest.NewRequest(http.MethodPost, "/api/customer/status", nil)
	r = customerCtx("tok")(r)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", w.Code)
	}
}

func TestHandleCustomerSLA_ConstantFallback(t *testing.T) {
	// Core /v1/sla absent → gateway constants.
	upstream := customerUpstream(t, `{"items":[]}`, `{"items":[]}`, 404, "nope")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	r := httptest.NewRequest(http.MethodGet, "/api/customer/sla", nil)
	r = customerCtx("tok")(r)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var got customerSLAResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("json: %v", err)
	}
	if got.TargetPct != 99.9 || got.Window != "calendar_month" {
		t.Errorf("got %+v", got)
	}
	if !strings.Contains(got.CreditNote, "display-only") {
		t.Errorf("credit_note = %q", got.CreditNote)
	}
}

func TestHandleCustomerSLA_ForwardsCore(t *testing.T) {
	body := `{"target_pct":99.9,"window":"calendar_month","credit_note":"display-only; no financial effect"}`
	upstream := customerUpstream(t, `{"items":[]}`, `{"items":[]}`, 200, body)
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	r := httptest.NewRequest(http.MethodGet, "/api/customer/sla", nil)
	r = customerCtx("tok")(r)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), "99.9") {
		t.Errorf("body = %s", w.Body.String())
	}
}

func TestSiteIDFromPath(t *testing.T) {
	if got := siteIDFromPath("sgp01.pod000.cdu000"); got != "sgp01" {
		t.Errorf("got %q", got)
	}
	if got := siteIDFromPath("  "); got != "" {
		t.Errorf("empty path got %q", got)
	}
}

func TestHandleCustomerStatus_DegradedWhenUpstreamDown(t *testing.T) {
	// Upstream that always 503s — status must flag degraded, not look all-green.
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(bad.Close)
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: bad.URL},
		NewUpstream(bad.URL, bad.Client()))

	r := httptest.NewRequest(http.MethodGet, "/api/customer/status", nil)
	r = customerCtx("tok")(r)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var got customerStatusResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Degraded {
		t.Fatalf("want degraded=true, got %+v", got)
	}
	if got.Note == "" {
		t.Fatal("want note when degraded")
	}
}

func TestHandleCustomerUsage_ForcesTenant(t *testing.T) {
	upstream := customerUpstream(t, `{"items":[]}`, `{"items":[]}`, 404, "")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))

	r := httptest.NewRequest(http.MethodGet, "/api/customer/usage?kind=energy", nil)
	r = customerCtx("tok")(r)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"tenant_id":"acme"`) {
		t.Errorf("body = %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "energy") {
		t.Errorf("body missing kind: %s", w.Body.String())
	}
}

func TestHandleCustomerUsage_MethodNotAllowed(t *testing.T) {
	upstream := customerUpstream(t, `{"items":[]}`, `{"items":[]}`, 404, "")
	srv := NewServer(Config{ListenAddr: ":0", UpstreamURL: upstream.URL},
		NewUpstream(upstream.URL, upstream.Client()))
	r := httptest.NewRequest(http.MethodPost, "/api/customer/usage", nil)
	r = customerCtx("tok")(r)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d", w.Code)
	}
}
