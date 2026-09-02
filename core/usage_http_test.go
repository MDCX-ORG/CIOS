// Package core — usage_http_test.go: HTTP tests for GET /v1/usage,
// GET /v1/usage:export, POST /v1/usage:compute (PRMT-193 §7).
//
// Coverage:
//   - list filters + pagination (decimal page_token)
//   - tenant-scope-mismatch on conflicting tenant_id
//   - CSV header + one row
//   - compute admin rack_hour + energy; non-admin 403
//   - method not allowed
package core

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type usageHTTPResp struct {
	code int
	body string
	ct   string
}

func doUsageReq(t *testing.T, ts *httptest.Server, method, path, body, token, tenant string) usageHTTPResp {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = bytes.NewReader([]byte(body))
	}
	req, err := http.NewRequest(method, ts.URL+path, rdr)
	if err != nil {
		t.Fatalf("new req: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if tenant != "" {
		req.Header.Set(tenantHeaderName, tenant)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	bb, _ := io.ReadAll(resp.Body)
	return usageHTTPResp{code: resp.StatusCode, body: string(bb), ct: resp.Header.Get("Content-Type")}
}

func seedUsageRecord(t *testing.T, srv *Server, rec UsageRecord) UsageRecord {
	t.Helper()
	got, err := srv.st.UpsertUsage(context.Background(), rec)
	if err != nil {
		t.Fatalf("UpsertUsage: %v", err)
	}
	return got
}

func TestUsageHTTP_List_FilterAndPagination(t *testing.T) {
	v, viewerTok, _, adminTok := buildR2Verifier(t, []string{"**"}, nil)
	srv := newAuthTestServer(t, &AuthConfig{Verifier: v})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	start := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC)
	for i, path := range []string{
		"sgp01.pod001.rack001",
		"sgp01.pod001.rack002",
		"sgp01.pod001.rack003",
	} {
		seedUsageRecord(t, srv, UsageRecord{
			Kind:        UsageKindRackHour,
			TenantID:    "tn_a",
			SiteID:      "sgp01",
			AssetPath:   path,
			PeriodStart: start.Add(time.Duration(i) * time.Hour),
			PeriodEnd:   end,
			Granularity: UsageDaily,
			Quantity:    24,
			Unit:        "h",
		})
	}
	seedUsageRecord(t, srv, UsageRecord{
		Kind:        UsageKindEnergy,
		TenantID:    "tn_a",
		SiteID:      "sgp01",
		AssetPath:   "sgp01.pod001.pdu001",
		PeriodStart: start,
		PeriodEnd:   end,
		Granularity: UsageDaily,
		Quantity:    10,
		Unit:        "kWh",
	})
	seedUsageRecord(t, srv, UsageRecord{
		Kind:        UsageKindRackHour,
		TenantID:    "tn_b",
		SiteID:      "hkg01",
		AssetPath:   "hkg01.pod001.rack001",
		PeriodStart: start,
		PeriodEnd:   end,
		Granularity: UsageMonthly,
		Quantity:    720,
		Unit:        "h",
	})

	// Full list for tn_a → 4 records (viewer has ActionRead list-scope).
	r := doUsageReq(t, ts, http.MethodGet, "/v1/usage?tenant_id=tn_a", "", viewerTok, "")
	if r.code != http.StatusOK {
		t.Fatalf("list tn_a: code=%d body=%s", r.code, r.body)
	}
	var env listUsageResponse
	if err := json.Unmarshal([]byte(r.body), &env); err != nil {
		t.Fatalf("decode: %v body=%s", err, r.body)
	}
	if len(env.Items) != 4 {
		t.Fatalf("items len=%d, want 4; got %+v", len(env.Items), env.Items)
	}

	// kind filter
	r = doUsageReq(t, ts, http.MethodGet, "/v1/usage?kind=energy", "", adminTok, "")
	if r.code != http.StatusOK {
		t.Fatalf("kind=energy: code=%d body=%s", r.code, r.body)
	}
	if err := json.Unmarshal([]byte(r.body), &env); err != nil {
		t.Fatalf("decode energy: %v", err)
	}
	if len(env.Items) != 1 || env.Items[0].Kind != UsageKindEnergy {
		t.Fatalf("energy filter: %+v", env.Items)
	}

	// page_size=2 + page_token decimal offset
	r = doUsageReq(t, ts, http.MethodGet, "/v1/usage?tenant_id=tn_a&page_size=2", "", adminTok, "")
	if r.code != http.StatusOK {
		t.Fatalf("page1: code=%d body=%s", r.code, r.body)
	}
	if err := json.Unmarshal([]byte(r.body), &env); err != nil {
		t.Fatalf("decode page1: %v", err)
	}
	if len(env.Items) != 2 {
		t.Fatalf("page1 len=%d, want 2", len(env.Items))
	}
	if env.NextPageToken != "2" {
		t.Fatalf("next_page_token=%q, want \"2\"", env.NextPageToken)
	}
	r = doUsageReq(t, ts, http.MethodGet, "/v1/usage?tenant_id=tn_a&page_size=2&page_token=2", "", adminTok, "")
	if r.code != http.StatusOK {
		t.Fatalf("page2: code=%d body=%s", r.code, r.body)
	}
	if err := json.Unmarshal([]byte(r.body), &env); err != nil {
		t.Fatalf("decode page2: %v", err)
	}
	if len(env.Items) != 2 {
		t.Fatalf("page2 len=%d, want 2", len(env.Items))
	}
	if env.NextPageToken != "" {
		t.Fatalf("page2 next=%q, want empty", env.NextPageToken)
	}
}

func TestUsageHTTP_TenantScopeMismatch(t *testing.T) {
	v, _, _, adminTok := buildR2Verifier(t, nil, nil)
	srv := newAuthTestServer(t, &AuthConfig{Verifier: v})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	r := doUsageReq(t, ts, http.MethodGet, "/v1/usage?tenant_id=other", "", adminTok, "tn_a")
	if r.code != http.StatusForbidden {
		t.Fatalf("code=%d body=%s, want 403", r.code, r.body)
	}
	if !strings.Contains(r.body, "tenant-scope-mismatch") {
		t.Errorf("body=%q, want tenant-scope-mismatch", r.body)
	}
}

func TestUsageHTTP_ExportCSV(t *testing.T) {
	v, _, _, adminTok := buildR2Verifier(t, nil, nil)
	srv := newAuthTestServer(t, &AuthConfig{Verifier: v})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	start := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC)
	rec := seedUsageRecord(t, srv, UsageRecord{
		Kind:        UsageKindEnergy,
		TenantID:    "tn_csv",
		SiteID:      "sgp01",
		AssetPath:   "sgp01.pod001.pdu001",
		PeriodStart: start,
		PeriodEnd:   end,
		Granularity: UsageDaily,
		Quantity:    1.5,
		Unit:        "kWh",
	})

	r := doUsageReq(t, ts, http.MethodGet, "/v1/usage:export?tenant_id=tn_csv", "", adminTok, "")
	if r.code != http.StatusOK {
		t.Fatalf("export: code=%d body=%s", r.code, r.body)
	}
	if !strings.HasPrefix(r.ct, "text/csv") {
		t.Errorf("Content-Type=%q, want text/csv", r.ct)
	}
	lines := strings.Split(strings.TrimSpace(r.body), "\n")
	if len(lines) < 2 {
		t.Fatalf("csv lines=%d, want >=2; body=%q", len(lines), r.body)
	}
	wantHeader := "id,kind,tenant_id,site_id,asset_path,period_start,period_end,granularity,quantity,unit"
	if lines[0] != wantHeader {
		t.Errorf("header=%q, want %q", lines[0], wantHeader)
	}
	if !strings.Contains(lines[1], rec.ID) || !strings.Contains(lines[1], "energy") {
		t.Errorf("row=%q, want id=%s energy", lines[1], rec.ID)
	}
}

func TestUsageHTTP_Compute_Admin(t *testing.T) {
	v, viewerTok, operatorTok, adminTok := buildR2Verifier(t, []string{"**"}, []string{"**"})
	srv := newAuthTestServer(t, &AuthConfig{Verifier: v})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	// Seed one active rack asset for rack_hour compute.
	if _, err := srv.st.PutAsset(context.Background(), Asset{
		Path: "sgp01.pod001.rack001",
		Spec: map[string]any{"type": "rack", "lifecycle": "active"},
	}, 0); err != nil {
		t.Fatalf("PutAsset: %v", err)
	}

	body := `{
		"period_start":"2026-07-01T00:00:00Z",
		"period_end":"2026-07-02T00:00:00Z",
		"granularity":"daily",
		"kinds":["energy","rack_hour"],
		"measurements":[
			{"asset_path":"sgp01.pod001.pdu001","time":"2026-07-01T06:00:00Z","quantity":2.5,"unit":"kWh"},
			{"asset_path":"sgp01.pod001.pdu001","time":"2026-07-01T12:00:00Z","quantity":1.5,"unit":"kWh"}
		]
	}`
	r := doUsageReq(t, ts, http.MethodPost, "/v1/usage:compute", body, adminTok, "tn_seed")
	if r.code != http.StatusOK {
		t.Fatalf("compute: code=%d body=%s", r.code, r.body)
	}
	var env usageComputeResponse
	if err := json.Unmarshal([]byte(r.body), &env); err != nil {
		t.Fatalf("decode: %v body=%s", err, r.body)
	}
	// 1 rack_hour + 1 energy aggregate
	if len(env.Items) != 2 {
		t.Fatalf("items len=%d, want 2; got %+v", len(env.Items), env.Items)
	}
	var sawRack, sawEnergy bool
	for _, it := range env.Items {
		if it.ID == "" {
			t.Errorf("upserted id empty: %+v", it)
		}
		if it.TenantID != "tn_seed" {
			t.Errorf("tenant_id=%q, want tn_seed (forced from header)", it.TenantID)
		}
		switch it.Kind {
		case UsageKindRackHour:
			sawRack = true
			if it.Quantity != 24 {
				t.Errorf("rack_hour qty=%v, want 24", it.Quantity)
			}
		case UsageKindEnergy:
			sawEnergy = true
			if it.Quantity != 4 {
				t.Errorf("energy qty=%v, want 4", it.Quantity)
			}
		}
	}
	if !sawRack || !sawEnergy {
		t.Errorf("sawRack=%v sawEnergy=%v", sawRack, sawEnergy)
	}

	// Listed via store / HTTP
	r = doUsageReq(t, ts, http.MethodGet, "/v1/usage?tenant_id=tn_seed", "", adminTok, "")
	if r.code != http.StatusOK {
		t.Fatalf("list after compute: code=%d body=%s", r.code, r.body)
	}

	// Non-admin → 403 (viewer fails ActionApply at middleware; operator may reach handler)
	r = doUsageReq(t, ts, http.MethodPost, "/v1/usage:compute", body, viewerTok, "")
	if r.code != http.StatusForbidden {
		t.Fatalf("viewer compute: code=%d, want 403 body=%s", r.code, r.body)
	}
	r = doUsageReq(t, ts, http.MethodPost, "/v1/usage:compute", body, operatorTok, "")
	if r.code != http.StatusForbidden {
		t.Fatalf("operator compute: code=%d, want 403 body=%s", r.code, r.body)
	}
}

func TestUsageHTTP_MethodNotAllowed(t *testing.T) {
	v, _, _, adminTok := buildR2Verifier(t, nil, nil)
	srv := newAuthTestServer(t, &AuthConfig{Verifier: v})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	r := doUsageReq(t, ts, http.MethodPost, "/v1/usage", `{}`, adminTok, "")
	if r.code != http.StatusMethodNotAllowed {
		t.Errorf("POST /v1/usage: code=%d, want 405", r.code)
	}
	r = doUsageReq(t, ts, http.MethodGet, "/v1/usage:compute", "", adminTok, "")
	if r.code != http.StatusMethodNotAllowed {
		t.Errorf("GET /v1/usage:compute: code=%d, want 405", r.code)
	}
}

func TestUsageHTTP_BadQuery(t *testing.T) {
	v, _, _, adminTok := buildR2Verifier(t, nil, nil)
	srv := newAuthTestServer(t, &AuthConfig{Verifier: v})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	cases := []string{
		"/v1/usage?kind=gpu_hour",
		"/v1/usage?granularity=hourly",
		"/v1/usage?page_size=0",
		"/v1/usage?page_size=1001",
		"/v1/usage?page_token=abc",
		"/v1/usage?period_start=not-a-date",
	}
	for _, path := range cases {
		r := doUsageReq(t, ts, http.MethodGet, path, "", adminTok, "")
		if r.code != http.StatusBadRequest {
			t.Errorf("%s: code=%d, want 400 body=%s", path, r.code, r.body)
		}
	}
}
