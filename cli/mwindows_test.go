// Package cli — mwindows_test.go: CLI-level smoke coverage for
// `cios maintenance window list|create|delete` (PRMT-096).
//
// Each test owns the full mux layout (httptest fake core). The CLI
// subcommand is dispatched end-to-end against the fake; the test
// asserts status code, stderr/stdout shape, and (for create/delete)
// the request body the fake observed.
package cli

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestMaintenanceWindowListEmpty(t *testing.T) {
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/maintenance/windows", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"items":[]}`+"\n")
		})
	})
	defer srv.Close()
	code, _, errOut := runWithServer(t, srv, []string{"maintenance", "window", "list"}, nil)
	if code != 0 {
		t.Fatalf("want 0, got %d stderr=%s", code, errOut)
	}
	if !strings.Contains(errOut, "no maintenance windows") {
		t.Fatalf("stderr=%q", errOut)
	}
}

func TestMaintenanceWindowListTable(t *testing.T) {
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/maintenance/windows", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"items":[{"id":"mw_AAAAAAAAAAAAAAAA","asset_path":"site01.pod001.cdu000","starts_at":"2026-06-20T10:00:00Z","ends_at":"2026-06-21T10:00:00Z","reason":"swap"}]}`+"\n")
		})
	})
	defer srv.Close()
	code, out, _ := runWithServer(t, srv, []string{"maintenance", "window", "list"}, nil)
	if code != 0 {
		t.Fatalf("want 0, got %d", code)
	}
	for _, s := range []string{"MAINTENANCE WINDOWS", "STARTS", "ENDS", "ASSET", "ID", "site01.pod001.cdu000", "mw_AAAAAAAAAAAAAAAA"} {
		if !strings.Contains(out, s) {
			t.Fatalf("table missing %q in:\n%s", s, out)
		}
	}
}

func TestMaintenanceWindowCreate(t *testing.T) {
	var seenBody string
	var seenMethod string
	var seenPath string
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/maintenance/windows", func(w http.ResponseWriter, r *http.Request) {
			seenMethod = r.Method
			seenPath = r.URL.Path
			buf, _ := io.ReadAll(r.Body)
			seenBody = string(buf)
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"id":"mw_BBBBBBBBBBBBBBBB","asset_path":"site01.pod001.cdu000","starts_at":"2026-06-20T10:00:00Z","ends_at":"2026-06-21T10:00:00Z","reason":"swap"}`+"\n")
		})
	})
	defer srv.Close()
	code, out, errOut := runWithServer(t, srv, []string{
		"maintenance", "window", "create",
		"--asset-path", "site01.pod001.cdu000",
		"--starts-at", "2026-06-20T10:00:00Z",
		"--ends-at", "2026-06-21T10:00:00Z",
		"--reason", "swap",
	}, nil)
	if code != 0 {
		t.Fatalf("want 0, got %d stderr=%s", code, errOut)
	}
	if seenMethod != http.MethodPost {
		t.Errorf("method=%q", seenMethod)
	}
	if seenPath != "/v1/maintenance/windows" {
		t.Errorf("path=%q", seenPath)
	}
	if !strings.Contains(seenBody, `"asset_path":"site01.pod001.cdu000"`) {
		t.Errorf("body=%q", seenBody)
	}
	if !strings.Contains(out, "created") {
		t.Errorf("stdout=%q", out)
	}
}

func TestMaintenanceWindowCreate_BadStartsAt(t *testing.T) {
	srv := newFakeCore(t)
	defer srv.Close()
	code, _, errOut := runWithServer(t, srv, []string{
		"maintenance", "window", "create",
		"--asset-path", "site01.pod001.cdu000",
		"--starts-at", "not-a-time",
		"--ends-at", "2026-06-21T10:00:00Z",
	}, nil)
	if code != 2 {
		t.Fatalf("want 2, got %d stderr=%s", code, errOut)
	}
	if !strings.Contains(errOut, "--starts-at") {
		t.Fatalf("stderr=%q", errOut)
	}
}

func TestMaintenanceWindowDelete(t *testing.T) {
	var seenMethod, seenPath string
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/maintenance/windows/", func(w http.ResponseWriter, r *http.Request) {
			seenMethod = r.Method
			seenPath = r.URL.Path
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"deleted":"mw_BBBBBBBBBBBBBBBB"}`+"\n")
		})
	})
	defer srv.Close()
	code, out, errOut := runWithServer(t, srv,
		[]string{"maintenance", "window", "delete", "mw_BBBBBBBBBBBBBBBB"}, nil)
	if code != 0 {
		t.Fatalf("want 0, got %d stderr=%s", code, errOut)
	}
	if seenMethod != http.MethodDelete {
		t.Errorf("method=%q", seenMethod)
	}
	if seenPath != "/v1/maintenance/windows/mw_BBBBBBBBBBBBBBBB" {
		t.Errorf("path=%q", seenPath)
	}
	if !strings.Contains(out, "deleted mw_BBBBBBBBBBBBBBBB") {
		t.Errorf("stdout=%q", out)
	}
}

func TestMaintenanceWindowDeleteMissingArg(t *testing.T) {
	srv := newFakeCore(t)
	defer srv.Close()
	code, _, errOut := runWithServer(t, srv,
		[]string{"maintenance", "window", "delete"}, nil)
	if code != 2 {
		t.Fatalf("want 2, got %d stderr=%s", code, errOut)
	}
}

func TestMaintenanceWindowUnknownSub(t *testing.T) {
	srv := newFakeCore(t)
	defer srv.Close()
	code, _, errOut := runWithServer(t, srv,
		[]string{"maintenance", "window", "frobnicate"}, nil)
	if code != 2 {
		t.Fatalf("want 2, got %d stderr=%s", code, errOut)
	}
}
