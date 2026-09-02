// Package cli — maintenance_test.go: end-to-end coverage of
// `cios maintenance upcoming` against an httptest fake core.
// Each test owns the full mux layout.
package cli

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestMaintenanceUpcomingEmpty(t *testing.T) {
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/maintenance/upcoming", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"items":[]}`+"\n")
		})
	})
	defer srv.Close()
	code, _, errOut := runWithServer(t, srv, []string{"maintenance", "upcoming"}, nil)
	if code != 0 {
		t.Fatalf("want 0, got %d stderr=%s", code, errOut)
	}
	if !strings.Contains(errOut, "no upcoming maintenance") {
		t.Fatalf("stderr=%q", errOut)
	}
}

func TestMaintenanceUpcomingTable(t *testing.T) {
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/maintenance/upcoming", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"items":[{"kind":"pm","id":"pm_1","asset_path":"site01.pod000.cdu000","title":"filter swap","next_due":"2026-06-20T10:00:00Z","overdue":false},{"kind":"inspection","id":"in_1","asset_path":"site01.pod000.cdu000","title":"visual check","next_due":"2026-06-15T10:00:00Z","overdue":true}]}`+"\n")
		})
	})
	defer srv.Close()
	code, out, _ := runWithServer(t, srv, []string{"maintenance", "upcoming"}, nil)
	if code != 0 {
		t.Fatalf("want 0, got %d", code)
	}
	// Columns: NEXT DUE | OVERDUE | KIND | TITLE (no id column in
	// the list view; the id is in the get view).
	for _, s := range []string{"UPCOMING MAINTENANCE", "NEXT DUE", "OVERDUE", "KIND", "TITLE", "pm", "inspection", "filter swap", "visual check", "yes", "no"} {
		if !strings.Contains(out, s) {
			t.Fatalf("table missing %q in:\n%s", s, out)
		}
	}
}

func TestMaintenanceUpcomingWithBefore(t *testing.T) {
	var seenQuery string
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/maintenance/upcoming", func(w http.ResponseWriter, r *http.Request) {
			seenQuery = r.URL.RawQuery
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"items":[]}`+"\n")
		})
	})
	defer srv.Close()
	code, _, _ := runWithServer(t, srv, []string{"maintenance", "upcoming", "--before", "2026-06-30T00:00:00Z"}, nil)
	if code != 0 {
		t.Fatalf("want 0, got %d", code)
	}
	if !strings.Contains(seenQuery, "before=2026-06-30T00%3A00%3A00Z") {
		t.Fatalf("missing before param: %s", seenQuery)
	}
}

func TestMaintenanceUpcomingWithOverdue(t *testing.T) {
	var seenQuery string
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/maintenance/upcoming", func(w http.ResponseWriter, r *http.Request) {
			seenQuery = r.URL.RawQuery
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"items":[]}`+"\n")
		})
	})
	defer srv.Close()
	code, _, _ := runWithServer(t, srv, []string{"maintenance", "upcoming", "--overdue"}, nil)
	if code != 0 {
		t.Fatalf("want 0, got %d", code)
	}
	if !strings.Contains(seenQuery, "overdue=true") {
		t.Fatalf("missing overdue param: %s", seenQuery)
	}
}

func TestMaintenanceUpcomingBadBefore(t *testing.T) {
	// --before that is not RFC3339 → local validation → exit 2.
	srv := newFakeCore(t)
	defer srv.Close()
	code, _, errOut := runWithServer(t, srv, []string{"maintenance", "upcoming", "--before", "garbage"}, nil)
	if code != 2 {
		t.Fatalf("want 2, got %d stderr=%s", code, errOut)
	}
	if !strings.Contains(errOut, "--before") {
		t.Fatalf("stderr=%q", errOut)
	}
}

func TestMaintenanceUpcomingJSON(t *testing.T) {
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/maintenance/upcoming", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"items":[{"kind":"pm","id":"pm_1","asset_path":"p","title":"t","next_due":"2026-06-20T10:00:00Z","overdue":false}]}`+"\n")
		})
	})
	defer srv.Close()
	code, out, _ := runWithServer(t, srv, []string{"-o", "json", "maintenance", "upcoming"}, nil)
	if code != 0 {
		t.Fatalf("want 0, got %d", code)
	}
	if !strings.Contains(out, `"id":"pm_1"`) {
		t.Fatalf("json out=%q", out)
	}
}

func TestMaintenanceUpcomingRFC7807(t *testing.T) {
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/maintenance/upcoming", func(w http.ResponseWriter, r *http.Request) {
			writeProblem(w, http.StatusInternalServerError, "store-error", "store error", "disk", r.URL.Path, "rid-mu")
		})
	})
	defer srv.Close()
	code, _, errOut := runWithServer(t, srv, []string{"maintenance", "upcoming"}, nil)
	if code != 1 {
		t.Fatalf("want 1, got %d stderr=%s", code, errOut)
	}
	if !strings.Contains(errOut, "store error") || !strings.Contains(errOut, "request_id=rid-mu") {
		t.Fatalf("stderr=%q", errOut)
	}
}

func TestMaintenanceUnknownSub(t *testing.T) {
	srv := newFakeCore(t)
	defer srv.Close()
	code, _, errOut := runWithServer(t, srv, []string{"maintenance", "frobnicate"}, nil)
	if code != 2 {
		t.Fatalf("want 2, got %d stderr=%s", code, errOut)
	}
}
