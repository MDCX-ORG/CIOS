// Package cli — doctor_test.go: end-to-end coverage of `cios doctor`
// against an httptest fake core. Each test owns the full mux layout
// (Go 1.22 mux panics on duplicate registration) and registers only
// the endpoints it exercises. Defaults from cli_test.go registerDefaults
// are NOT used here because doctor only talks to three endpoints.
package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// --- happy path: all PASS, exit 0 ---------------------------------------

func TestDoctorAllPass(t *testing.T) {
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/health", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"status":"ok"}`+"\n")
		})
		mux.HandleFunc("/v1/health/ready", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"status":"ok"}`+"\n")
		})
		mux.HandleFunc("/v1/alarms", func(w http.ResponseWriter, r *http.Request) {
			// Auth enforced: no token → 401 (PASS for auth-config).
			writeProblem(w, http.StatusUnauthorized, "unauthorized",
				"Unauthorized", "missing token", r.URL.Path, "rid-doc-1")
		})
	})
	defer srv.Close()
	code, out, errOut := runWithServer(t, srv, []string{"doctor"}, nil)
	if code != 0 {
		t.Fatalf("want exit 0, got %d stderr=%s", code, errOut)
	}
	for _, s := range []string{"core reachable", "deps ready", "auth config", "version", "PASS"} {
		if !strings.Contains(out, s) {
			t.Fatalf("table missing token %q in:\n%s", s, out)
		}
	}
	// No FAIL anywhere → exit 0, and no FAIL row in the table.
	if strings.Contains(out, "FAIL") {
		t.Fatalf("unexpected FAIL row in:\n%s", out)
	}
}

func TestDoctorAllPassJSON(t *testing.T) {
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/health", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"status":"ok"}`+"\n")
		})
		mux.HandleFunc("/v1/health/ready", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"status":"ok"}`+"\n")
		})
		mux.HandleFunc("/v1/alarms", func(w http.ResponseWriter, r *http.Request) {
			writeProblem(w, http.StatusUnauthorized, "unauthorized",
				"Unauthorized", "missing token", r.URL.Path, "rid-doc-json")
		})
	})
	defer srv.Close()
	code, out, errOut := runWithServer(t, srv, []string{"doctor", "--json"}, nil)
	if code != 0 {
		t.Fatalf("want 0, got %d stderr=%s", code, errOut)
	}
	var report doctorReport
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &report); err != nil {
		t.Fatalf("json unmarshal: %v out=%q", err, out)
	}
	if len(report.Results) != 4 {
		t.Fatalf("want 4 rows, got %d in %+v", len(report.Results), report.Results)
	}
	for _, r := range report.Results {
		if r.Status != doctorPass {
			t.Fatalf("row %q: want PASS, got %s (%s)", r.Check, r.Status, r.Detail)
		}
	}
}

// --- FAIL on a single check → exit 1 ------------------------------------

func TestDoctorCoreUnreachableExit1(t *testing.T) {
	// Pin the env to a closed loopback port so the network
	// failure is deterministic (see cli_test.go PRMT-046 fix).
	t.Setenv("CIOS_SERVER", "http://127.0.0.1:1")
	code, out, errOut := runWithServer(t, nil, []string{"doctor"}, nil)
	if code != 1 {
		t.Fatalf("want 1, got %d stderr=%s", code, errOut)
	}
	if !strings.Contains(out, "core reachable") || !strings.Contains(out, "FAIL") {
		t.Fatalf("missing FAIL row in:\n%s", out)
	}
}

func TestDoctorDepsDownExit1(t *testing.T) {
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/health", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"status":"ok"}`+"\n")
		})
		mux.HandleFunc("/v1/health/ready", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"status":"degraded","down":["pg","vm"]}`+"\n")
		})
		mux.HandleFunc("/v1/alarms", func(w http.ResponseWriter, r *http.Request) {
			writeProblem(w, http.StatusUnauthorized, "unauthorized",
				"Unauthorized", "missing token", r.URL.Path, "rid-deps")
		})
	})
	defer srv.Close()
	code, out, errOut := runWithServer(t, srv, []string{"doctor"}, nil)
	if code != 1 {
		t.Fatalf("want 1, got %d stderr=%s", code, errOut)
	}
	if !strings.Contains(out, "deps ready") || !strings.Contains(out, "FAIL") {
		t.Fatalf("missing FAIL row in:\n%s", out)
	}
	// Dep list must appear in the detail column.
	if !strings.Contains(out, "pg") || !strings.Contains(out, "vm") {
		t.Fatalf("missing dep list in:\n%s", out)
	}
}

// --- WARN scenarios: exit 0, WARN row visible ---------------------------

func TestDoctorReadyEndpointAbsentWARN(t *testing.T) {
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/health", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"status":"ok"}`+"\n")
		})
		// /v1/health/ready absent (pre-PRMT-066 server).
		mux.HandleFunc("/v1/alarms", func(w http.ResponseWriter, r *http.Request) {
			writeProblem(w, http.StatusUnauthorized, "unauthorized",
				"Unauthorized", "missing token", r.URL.Path, "rid-no-ready")
		})
	})
	defer srv.Close()
	code, out, errOut := runWithServer(t, srv, []string{"doctor"}, nil)
	if code != 0 {
		t.Fatalf("want 0 (WARN does not escalate), got %d stderr=%s", code, errOut)
	}
	if !strings.Contains(out, "WARN") || !strings.Contains(out, "ready endpoint absent") {
		t.Fatalf("missing ready-absent WARN row in:\n%s", out)
	}
}

func TestDoctorAuthDisabledWARN(t *testing.T) {
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/health", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"status":"ok"}`+"\n")
		})
		mux.HandleFunc("/v1/health/ready", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"status":"ok"}`+"\n")
		})
		// Auth OFF: no token → 200 (M0 兼容模式).
		mux.HandleFunc("/v1/alarms", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"items":[],"next_page_token":""}`+"\n")
		})
	})
	defer srv.Close()
	code, out, errOut := runWithServer(t, srv, []string{"doctor"}, nil)
	if code != 0 {
		t.Fatalf("want 0 (WARN does not escalate), got %d stderr=%s", code, errOut)
	}
	if !strings.Contains(out, "WARN") || !strings.Contains(out, "auth disabled (M0 兼容模式)") {
		t.Fatalf("missing auth-disabled WARN row in:\n%s", out)
	}
}

func TestDoctorAuthDisabledJSON(t *testing.T) {
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/health", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"status":"ok"}`+"\n")
		})
		mux.HandleFunc("/v1/health/ready", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"status":"ok"}`+"\n")
		})
		mux.HandleFunc("/v1/alarms", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"items":[],"next_page_token":""}`+"\n")
		})
	})
	defer srv.Close()
	code, out, errOut := runWithServer(t, srv, []string{"doctor", "--json"}, nil)
	if code != 0 {
		t.Fatalf("want 0, got %d stderr=%s", code, errOut)
	}
	var report doctorReport
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &report); err != nil {
		t.Fatalf("json unmarshal: %v out=%q", err, out)
	}
	var authRow *doctorResult
	for i := range report.Results {
		if report.Results[i].Check == "auth config" {
			authRow = &report.Results[i]
		}
	}
	if authRow == nil {
		t.Fatalf("auth config row missing in %+v", report.Results)
	}
	if authRow.Status != doctorWarn {
		t.Fatalf("auth config: want WARN, got %s", authRow.Status)
	}
	if !strings.Contains(authRow.Detail, "M0 兼容模式") {
		t.Fatalf("auth config detail: %q", authRow.Detail)
	}
}

// --- core reachable FAIL when /v1/health returns non-2xx ---------------

func TestDoctorCoreUnhealthyExit1(t *testing.T) {
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/health", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, "boom")
		})
		mux.HandleFunc("/v1/health/ready", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"status":"ok"}`+"\n")
		})
		mux.HandleFunc("/v1/alarms", func(w http.ResponseWriter, r *http.Request) {
			writeProblem(w, http.StatusUnauthorized, "unauthorized",
				"Unauthorized", "missing token", r.URL.Path, "rid-unh")
		})
	})
	defer srv.Close()
	code, out, errOut := runWithServer(t, srv, []string{"doctor"}, nil)
	if code != 1 {
		t.Fatalf("want 1, got %d stderr=%s", code, errOut)
	}
	if !strings.Contains(out, "core reachable") || !strings.Contains(out, "FAIL") {
		t.Fatalf("missing core-reachable FAIL in:\n%s", out)
	}
}
