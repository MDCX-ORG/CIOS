// Package cli — inspection_test.go: end-to-end coverage of
// `cios inspection list|get|create` against an httptest fake core.
// Each test owns the full mux layout.
package cli

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// --- list ----------------------------------------------------------------

func TestInspectionListEmpty(t *testing.T) {
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/inspections", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"items":[]}`+"\n")
		})
	})
	defer srv.Close()
	code, _, errOut := runWithServer(t, srv, []string{"inspection", "list"}, nil)
	if code != 0 {
		t.Fatalf("want 0, got %d stderr=%s", code, errOut)
	}
	if !strings.Contains(errOut, "no inspections") {
		t.Fatalf("stderr=%q", errOut)
	}
}

func TestInspectionListTable(t *testing.T) {
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/inspections", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			// interval=86400000000000ns = 24h, which the formatter
			// renders as "1d" (whole-day suffix).
			io.WriteString(w, `{"items":[{"id":"in_1","asset_path":"site01.pod000.cdu000","title":"visual check","items":["leak","temp"],"interval":86400000000000,"next_due":"2026-06-13T10:00:00Z","enabled":true}]}`+"\n")
		})
	})
	defer srv.Close()
	code, out, _ := runWithServer(t, srv, []string{"inspection", "list"}, nil)
	if code != 0 {
		t.Fatalf("want 0, got %d", code)
	}
	// Columns: NEXT DUE | INTERVAL | ITEMS | ENABLED | TITLE
	for _, s := range []string{"INSPECTIONS", "NEXT DUE", "INTERVAL", "ITEMS", "ENABLED", "TITLE", "visual check", "1d", "2", "yes"} {
		if !strings.Contains(out, s) {
			t.Fatalf("table missing %q in:\n%s", s, out)
		}
	}
}

func TestInspectionListIntervalUnits(t *testing.T) {
	// Render durations in days (168h = 7d) and minutes (30m).
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/inspections", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"items":[`+
				`{"id":"i_w","asset_path":"p","title":"w","items":[],"interval":604800000000000,"next_due":"2026-06-12T10:00:00Z","enabled":true},`+
				`{"id":"i_m","asset_path":"p","title":"m","items":[],"interval":1800000000000,"next_due":"2026-06-12T10:00:00Z","enabled":false}`+
				`]}`+"\n")
		})
	})
	defer srv.Close()
	code, out, _ := runWithServer(t, srv, []string{"inspection", "list"}, nil)
	if code != 0 {
		t.Fatalf("want 0, got %d", code)
	}
	if !strings.Contains(out, "7d") {
		t.Fatalf("missing 7d in:\n%s", out)
	}
	if !strings.Contains(out, "30m") {
		t.Fatalf("missing 30m in:\n%s", out)
	}
	if !strings.Contains(out, "no") {
		t.Fatalf("missing 'no' for disabled in:\n%s", out)
	}
}

func TestInspectionListWithFilter(t *testing.T) {
	var seenFilter string
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/inspections", func(w http.ResponseWriter, r *http.Request) {
			seenFilter = r.URL.Query().Get("filter")
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"items":[]}`+"\n")
		})
	})
	defer srv.Close()
	code, _, _ := runWithServer(t, srv, []string{"inspection", "list", "--filter", "site01"}, nil)
	if code != 0 {
		t.Fatalf("want 0, got %d", code)
	}
	if seenFilter != "site01" {
		t.Fatalf("want filter=site01, got %q", seenFilter)
	}
}

func TestInspectionListRFC7807(t *testing.T) {
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/inspections", func(w http.ResponseWriter, r *http.Request) {
			writeProblem(w, http.StatusInternalServerError, "store-error", "store error", "disk", r.URL.Path, "rid-il")
		})
	})
	defer srv.Close()
	code, _, errOut := runWithServer(t, srv, []string{"inspection", "list"}, nil)
	if code != 1 {
		t.Fatalf("want 1, got %d stderr=%s", code, errOut)
	}
	if !strings.Contains(errOut, "store error") || !strings.Contains(errOut, "request_id=rid-il") {
		t.Fatalf("stderr=%q", errOut)
	}
}

// --- get -----------------------------------------------------------------

func TestInspectionGetSuccess(t *testing.T) {
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/inspections/in_1", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"id":"in_1","asset_path":"site01.pod000.cdu000","title":"visual check","items":["leak","temp"],"interval":86400000000000,"next_due":"2026-06-13T10:00:00Z","enabled":true}`+"\n")
		})
	})
	defer srv.Close()
	code, out, _ := runWithServer(t, srv, []string{"inspection", "get", "in_1"}, nil)
	if code != 0 {
		t.Fatalf("want 0, got %d", code)
	}
	// 86400000000000ns = 24h, formatted as "1d".
	for _, s := range []string{"ID", "in_1", "ASSET", "site01.pod000.cdu000", "TITLE", "visual check", "INTERVAL", "1d", "ITEMS", "leak", "temp", "ENABLED", "yes"} {
		if !strings.Contains(out, s) {
			t.Fatalf("detail row missing %q in:\n%s", s, out)
		}
	}
}

func TestInspectionGet404(t *testing.T) {
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/inspections/in_nope", func(w http.ResponseWriter, r *http.Request) {
			writeProblem(w, http.StatusNotFound, "path-not-found", "inspection not found", "in_nope", r.URL.Path, "rid-ig")
		})
	})
	defer srv.Close()
	code, _, errOut := runWithServer(t, srv, []string{"inspection", "get", "in_nope"}, nil)
	if code != 1 {
		t.Fatalf("want 1, got %d stderr=%s", code, errOut)
	}
	if !strings.Contains(errOut, "inspection not found") || !strings.Contains(errOut, "request_id=rid-ig") {
		t.Fatalf("stderr=%q", errOut)
	}
}

func TestInspectionGetMissingArg(t *testing.T) {
	srv := newFakeCore(t)
	defer srv.Close()
	code, _, errOut := runWithServer(t, srv, []string{"inspection", "get"}, nil)
	if code != 2 {
		t.Fatalf("want 2, got %d stderr=%s", code, errOut)
	}
}

// --- create --------------------------------------------------------------

func TestInspectionCreateSuccess(t *testing.T) {
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/inspections", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				writeProblem(w, http.StatusMethodNotAllowed, "bad-request", "method", "", r.URL.Path, "rid")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"id":"in_new","asset_path":"p","title":"t","items":["a","b"],"interval":86400000000000,"next_due":"2026-06-13T10:00:00Z","enabled":true}`+"\n")
		})
	})
	defer srv.Close()
	code, out, _ := runWithServer(t, srv, []string{"inspection", "create", "--asset", "p", "--title", "t", "--interval", "24h", "--items", "a,b"}, nil)
	if code != 0 {
		t.Fatalf("want 0, got %d", code)
	}
	if !strings.Contains(out, "created in_new") {
		t.Fatalf("out=%q", out)
	}
}

func TestInspectionCreateMissingFlags(t *testing.T) {
	srv := newFakeCore(t)
	defer srv.Close()
	code, _, errOut := runWithServer(t, srv, []string{"inspection", "create", "--asset", "p"}, nil)
	if code != 2 {
		t.Fatalf("want 2, got %d stderr=%s", code, errOut)
	}
}

func TestInspectionCreateBadInterval(t *testing.T) {
	// --interval that doesn't parse → local validation failure → exit 2.
	srv := newFakeCore(t)
	defer srv.Close()
	code, _, errOut := runWithServer(t, srv, []string{"inspection", "create", "--asset", "p", "--title", "t", "--interval", "garbage"}, nil)
	if code != 2 {
		t.Fatalf("want 2, got %d stderr=%s", code, errOut)
	}
	if !strings.Contains(errOut, "--interval") {
		t.Fatalf("stderr=%q", errOut)
	}
}

// 400 → exit1: bad input (e.g. interval <= 0 server-side, or bad path).
func TestInspectionCreate400(t *testing.T) {
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/inspections", func(w http.ResponseWriter, r *http.Request) {
			writeProblem(w, http.StatusBadRequest, "bad-path", "bad asset path", "syntax", r.URL.Path, "rid-ic")
		})
	})
	defer srv.Close()
	code, _, errOut := runWithServer(t, srv, []string{"inspection", "create", "--asset", "bad", "--title", "t", "--interval", "24h"}, nil)
	if code != 1 {
		t.Fatalf("want 1, got %d stderr=%s", code, errOut)
	}
	if !strings.Contains(errOut, "bad asset path") || !strings.Contains(errOut, "request_id=rid-ic") {
		t.Fatalf("stderr=%q", errOut)
	}
}

func TestInspectionUnknownSub(t *testing.T) {
	srv := newFakeCore(t)
	defer srv.Close()
	code, _, errOut := runWithServer(t, srv, []string{"inspection", "frobnicate"}, nil)
	if code != 2 {
		t.Fatalf("want 2, got %d stderr=%s", code, errOut)
	}
}
