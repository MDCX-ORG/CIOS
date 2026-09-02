// Package cli — report_test.go: end-to-end coverage of
// `cios report ops|reconcile|generate` against an httptest fake
// core. Covers the degraded field rendering for reconcile and the
// HTML output path for generate. Each test owns the full mux layout.
package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- ops -----------------------------------------------------------------

func TestReportOpsTable(t *testing.T) {
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/reports/ops", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"mttr_seconds":120.5,"mean_response_seconds":60.25,"mtbf_seconds":3600,"ticket_counts":{"by_state":{"open":1,"acknowledged":0,"resolved":3,"closed":10},"by_severity":{"critical":2,"major":1,"minor":3,"info":0}},"alarm_top":[{"path":"p","count":5}],"window":{"since":"2026-06-12T00:00:00Z"}}`+"\n")
		})
	})
	defer srv.Close()
	code, out, _ := runWithServer(t, srv, []string{"report", "ops"}, nil)
	if code != 0 {
		t.Fatalf("want 0, got %d", code)
	}
	for _, s := range []string{"OPS REPORT", "MTTR", "120.500s", "Mean response time", "60.250s", "MTBF", "3600.000s", "TICKETS BY STATE", "open", "closed", "TICKETS BY SEVERITY", "critical", "ALARM TOP", "p", "5"} {
		if !strings.Contains(out, s) {
			t.Fatalf("ops table missing %q in:\n%s", s, out)
		}
	}
}

func TestReportOpsNilMetrics(t *testing.T) {
	// All metrics nil → should print "-" for each.
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/reports/ops", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"ticket_counts":{"by_state":{},"by_severity":{}}}`+"\n")
		})
	})
	defer srv.Close()
	code, out, _ := runWithServer(t, srv, []string{"report", "ops"}, nil)
	if code != 0 {
		t.Fatalf("want 0, got %d", code)
	}
	if !strings.Contains(out, "MTTR (mean resolve time) : -") {
		t.Fatalf("expected '-' for nil mttr in:\n%s", out)
	}
}

func TestReportOpsJSON(t *testing.T) {
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/reports/ops", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"mttr_seconds":10.0,"ticket_counts":{"by_state":{},"by_severity":{}}}`+"\n")
		})
	})
	defer srv.Close()
	code, out, _ := runWithServer(t, srv, []string{"-o", "json", "report", "ops"}, nil)
	if code != 0 {
		t.Fatalf("want 0, got %d", code)
	}
	var v map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &v); err != nil {
		t.Fatalf("json: %v out=%q", err, out)
	}
	if v["mttr_seconds"].(float64) != 10.0 {
		t.Fatalf("mttr_seconds wrong: %v", v["mttr_seconds"])
	}
}

func TestReportOpsTopOutOfRange(t *testing.T) {
	srv := newFakeCore(t)
	defer srv.Close()
	code, _, errOut := runWithServer(t, srv, []string{"report", "ops", "--top", "0"}, nil)
	if code != 2 {
		t.Fatalf("want 2, got %d stderr=%s", code, errOut)
	}
}

func TestReportOpsBadTopOver100(t *testing.T) {
	srv := newFakeCore(t)
	defer srv.Close()
	code, _, errOut := runWithServer(t, srv, []string{"report", "ops", "--top", "101"}, nil)
	if code != 2 {
		t.Fatalf("want 2, got %d stderr=%s", code, errOut)
	}
}

func TestReportOpsRFC7807(t *testing.T) {
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/reports/ops", func(w http.ResponseWriter, r *http.Request) {
			writeProblem(w, http.StatusInternalServerError, "store-error", "vm down", "no data", r.URL.Path, "rid-ro")
		})
	})
	defer srv.Close()
	code, _, errOut := runWithServer(t, srv, []string{"report", "ops"}, nil)
	if code != 1 {
		t.Fatalf("want 1, got %d stderr=%s", code, errOut)
	}
	if !strings.Contains(errOut, "vm down") || !strings.Contains(errOut, "request_id=rid-ro") {
		t.Fatalf("stderr=%q", errOut)
	}
}

// --- reconcile -----------------------------------------------------------

func TestReportReconcileTableOK(t *testing.T) {
	// degraded=false → must say "Degraded     : false" in the table.
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/reports/reconcile", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"window":"7d","degraded":false,"orphans_restricted":false,"entries":[{"path":"ok1","lifecycle":"active","state":"ok","telemetry_present":true},{"path":"ok2","lifecycle":"active","state":"ok","telemetry_present":true}],"orphans":[]}`+"\n")
		})
	})
	defer srv.Close()
	code, out, _ := runWithServer(t, srv, []string{"report", "reconcile"}, nil)
	if code != 0 {
		t.Fatalf("want 0, got %d", code)
	}
	for _, s := range []string{"RECONCILE REPORT", "Window", "7d", "Degraded     : false", "Orphans      : (none)", "OK (registered + telemetry) : 2", "ok1", "ok2"} {
		if !strings.Contains(out, s) {
			t.Fatalf("reconcile table missing %q in:\n%s", s, out)
		}
	}
}

func TestReportReconcileDegraded(t *testing.T) {
	// degraded=true must surface "Degraded     : true  (telemetry side
	// incomplete; some entries unknown)" so operators see it at a glance.
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/reports/reconcile", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"window":"7d","degraded":true,"orphans_restricted":false,"entries":[{"path":"unk1","lifecycle":"active","state":"ok","telemetry_present":true,"telemetry_unknown":true},{"path":"reg_no_tel","lifecycle":"active","state":"registered","telemetry_present":false}],"orphans":[]}`+"\n")
		})
	})
	defer srv.Close()
	code, out, _ := runWithServer(t, srv, []string{"report", "reconcile"}, nil)
	if code != 0 {
		t.Fatalf("want 0, got %d", code)
	}
	if !strings.Contains(out, "Degraded     : true") {
		t.Fatalf("degraded flag missing in:\n%s", out)
	}
	if !strings.Contains(out, "UNKNOWN (VM failed)") {
		t.Fatalf("UNKNOWN bucket missing in:\n%s", out)
	}
	if !strings.Contains(out, "REGISTERED, NO TELEMETRY") {
		t.Fatalf("REGISTERED bucket missing in:\n%s", out)
	}
	if !strings.Contains(out, "unk1") || !strings.Contains(out, "reg_no_tel") {
		t.Fatalf("paths not printed in:\n%s", out)
	}
}

func TestReportReconcileOrphansRestricted(t *testing.T) {
	// orphans_restricted=true (caller below operator role) — must
	// surface "(restricted — caller below operator role)".
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/reports/reconcile", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"window":"7d","degraded":false,"orphans_restricted":true,"entries":[],"orphans":[]}`+"\n")
		})
	})
	defer srv.Close()
	code, out, _ := runWithServer(t, srv, []string{"report", "reconcile"}, nil)
	if code != 0 {
		t.Fatalf("want 0, got %d", code)
	}
	if !strings.Contains(out, "Orphans      : (restricted") {
		t.Fatalf("orphans-restricted note missing in:\n%s", out)
	}
}

func TestReportReconcileJSON(t *testing.T) {
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/reports/reconcile", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"window":"7d","degraded":false,"orphans_restricted":false,"entries":[],"orphans":[]}`+"\n")
		})
	})
	defer srv.Close()
	code, out, _ := runWithServer(t, srv, []string{"-o", "json", "report", "reconcile"}, nil)
	if code != 0 {
		t.Fatalf("want 0, got %d", code)
	}
	if !strings.Contains(out, `"window":"7d"`) {
		t.Fatalf("json out=%q", out)
	}
}

func TestReportReconcileRFC7807(t *testing.T) {
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/reports/reconcile", func(w http.ResponseWriter, r *http.Request) {
			writeProblem(w, http.StatusInternalServerError, "vm-unavailable", "vm unavailable", "dial tcp", r.URL.Path, "rid-rr")
		})
	})
	defer srv.Close()
	code, _, errOut := runWithServer(t, srv, []string{"report", "reconcile"}, nil)
	if code != 1 {
		t.Fatalf("want 1, got %d stderr=%s", code, errOut)
	}
	if !strings.Contains(errOut, "vm unavailable") || !strings.Contains(errOut, "request_id=rid-rr") {
		t.Fatalf("stderr=%q", errOut)
	}
}

// --- generate ------------------------------------------------------------

func TestReportGenerateStdout(t *testing.T) {
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/reports/ops", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"mttr_seconds":42.0,"mean_response_seconds":10.0,"mtbf_seconds":3600,"ticket_counts":{"by_state":{"open":1,"acknowledged":0,"resolved":0,"closed":0},"by_severity":{"critical":0,"major":0,"minor":0,"info":0}}}`+"\n")
		})
	})
	defer srv.Close()
	code, out, _ := runWithServer(t, srv, []string{"report", "generate", "--type", "monthly"}, nil)
	if code != 0 {
		t.Fatalf("want 0, got %d", code)
	}
	for _, s := range []string{"<!doctype html>", "<h1>CIOS monthly Ops Report</h1>", "MTTR", "42.000s"} {
		if !strings.Contains(out, s) {
			t.Fatalf("html missing %q in:\n%s", s, out)
		}
	}
}

func TestReportGenerateToFile(t *testing.T) {
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/reports/ops", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"mttr_seconds":null,"ticket_counts":{"by_state":{},"by_severity":{}}}`+"\n")
		})
	})
	defer srv.Close()
	dir := t.TempDir()
	p := filepath.Join(dir, "rep.html")
	code, _, errOut := runWithServer(t, srv, []string{"report", "generate", "--type", "weekly", "--out", p}, nil)
	if code != 0 {
		t.Fatalf("want 0, got %d stderr=%s", code, errOut)
	}
	body, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	if !strings.Contains(string(body), "<h1>CIOS weekly Ops Report</h1>") {
		t.Fatalf("file content wrong: %s", body)
	}
}

func TestReportGenerateBadType(t *testing.T) {
	srv := newFakeCore(t)
	defer srv.Close()
	code, _, errOut := runWithServer(t, srv, []string{"report", "generate", "--type", "yearly"}, nil)
	if code != 2 {
		t.Fatalf("want 2, got %d stderr=%s", code, errOut)
	}
}

func TestReportGenerateRFC7807(t *testing.T) {
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/reports/ops", func(w http.ResponseWriter, r *http.Request) {
			writeProblem(w, http.StatusInternalServerError, "vm-down", "vm down", "no data", r.URL.Path, "rid-rg")
		})
	})
	defer srv.Close()
	code, _, errOut := runWithServer(t, srv, []string{"report", "generate"}, nil)
	if code != 1 {
		t.Fatalf("want 1, got %d stderr=%s", code, errOut)
	}
	if !strings.Contains(errOut, "vm down") || !strings.Contains(errOut, "request_id=rid-rg") {
		t.Fatalf("stderr=%q", errOut)
	}
}

func TestReportUnknownSub(t *testing.T) {
	srv := newFakeCore(t)
	defer srv.Close()
	code, _, errOut := runWithServer(t, srv, []string{"report", "frobnicate"}, nil)
	if code != 2 {
		t.Fatalf("want 2, got %d stderr=%s", code, errOut)
	}
}
