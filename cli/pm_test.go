// Package cli — pm_test.go: end-to-end coverage of
// `cios pm list|get|create` against an httptest fake core.
// Each test owns the full mux layout.
package cli

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// --- list ----------------------------------------------------------------

func TestPMListEmpty(t *testing.T) {
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/pm/schedules", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"items":[]}`+"\n")
		})
	})
	defer srv.Close()
	code, _, errOut := runWithServer(t, srv, []string{"pm", "list"}, nil)
	if code != 0 {
		t.Fatalf("want 0, got %d stderr=%s", code, errOut)
	}
	if !strings.Contains(errOut, "no pm schedules") {
		t.Fatalf("stderr=%q", errOut)
	}
}

func TestPMListTable(t *testing.T) {
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/pm/schedules", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"items":[{"id":"pm_1","asset_path":"site01.pod000.cdu000","kind":"filter_swap","interval_days":30,"next_due":"2026-07-12T10:00:00Z","title":"filter swap","severity":"minor","enabled":true}]}`+"\n")
		})
	})
	defer srv.Close()
	code, out, _ := runWithServer(t, srv, []string{"pm", "list"}, nil)
	if code != 0 {
		t.Fatalf("want 0, got %d", code)
	}
	// Columns: NEXT DUE | INTERVAL | SEV | ENABLED | TITLE
	// (id is not in the list table; only in the get view).
	for _, s := range []string{"PM SCHEDULES", "NEXT DUE", "INTERVAL", "SEV", "ENABLED", "TITLE", "30d", "filter swap", "yes", "minor"} {
		if !strings.Contains(out, s) {
			t.Fatalf("table missing %q in:\n%s", s, out)
		}
	}
}

func TestPMListDisabled(t *testing.T) {
	// enabled=false must render "no".
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/pm/schedules", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"items":[{"id":"pm_off","asset_path":"p","kind":"k","interval_days":7,"next_due":"2026-06-12T10:00:00Z","title":"t","severity":"info","enabled":false}]}`+"\n")
		})
	})
	defer srv.Close()
	code, out, _ := runWithServer(t, srv, []string{"pm", "list"}, nil)
	if code != 0 {
		t.Fatalf("want 0, got %d", code)
	}
	if !strings.Contains(out, "no") {
		t.Fatalf("expected 'no' for disabled in:\n%s", out)
	}
}

func TestPMListWithFilter(t *testing.T) {
	var seenFilter string
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/pm/schedules", func(w http.ResponseWriter, r *http.Request) {
			seenFilter = r.URL.Query().Get("filter")
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"items":[]}`+"\n")
		})
	})
	defer srv.Close()
	code, _, _ := runWithServer(t, srv, []string{"pm", "list", "--filter", "site01"}, nil)
	if code != 0 {
		t.Fatalf("want 0, got %d", code)
	}
	if seenFilter != "site01" {
		t.Fatalf("want filter=site01, got %q", seenFilter)
	}
}

func TestPMListJSON(t *testing.T) {
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/pm/schedules", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"items":[{"id":"pm_1","asset_path":"p","kind":"k","interval_days":7,"next_due":"2026-06-12T10:00:00Z","title":"t","severity":"info","enabled":true}]}`+"\n")
		})
	})
	defer srv.Close()
	code, out, _ := runWithServer(t, srv, []string{"-o", "json", "pm", "list"}, nil)
	if code != 0 {
		t.Fatalf("want 0, got %d", code)
	}
	if !strings.Contains(out, `"id":"pm_1"`) {
		t.Fatalf("json out=%q", out)
	}
}

func TestPMListRFC7807(t *testing.T) {
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/pm/schedules", func(w http.ResponseWriter, r *http.Request) {
			writeProblem(w, http.StatusInternalServerError, "store-error", "store error", "disk", r.URL.Path, "rid-pl")
		})
	})
	defer srv.Close()
	code, _, errOut := runWithServer(t, srv, []string{"pm", "list"}, nil)
	if code != 1 {
		t.Fatalf("want 1, got %d stderr=%s", code, errOut)
	}
	if !strings.Contains(errOut, "store error") || !strings.Contains(errOut, "request_id=rid-pl") {
		t.Fatalf("stderr=%q", errOut)
	}
}

// --- get -----------------------------------------------------------------

func TestPMGetSuccess(t *testing.T) {
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/pm/schedules/pm_1", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"id":"pm_1","asset_path":"site01.pod000.cdu000","kind":"filter_swap","interval_days":30,"next_due":"2026-07-12T10:00:00Z","title":"filter swap","severity":"minor","enabled":true}`+"\n")
		})
	})
	defer srv.Close()
	code, out, _ := runWithServer(t, srv, []string{"pm", "get", "pm_1"}, nil)
	if code != 0 {
		t.Fatalf("want 0, got %d", code)
	}
	for _, s := range []string{"ID", "pm_1", "ASSET", "site01.pod000.cdu000", "KIND", "filter_swap", "INTERVAL", "30 days", "TITLE", "filter swap", "SEVERITY", "minor", "ENABLED", "yes"} {
		if !strings.Contains(out, s) {
			t.Fatalf("detail row missing %q in:\n%s", s, out)
		}
	}
}

func TestPMGet404(t *testing.T) {
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/pm/schedules/pm_nope", func(w http.ResponseWriter, r *http.Request) {
			writeProblem(w, http.StatusNotFound, "path-not-found", "pm schedule not found", "pm_nope", r.URL.Path, "rid-pg")
		})
	})
	defer srv.Close()
	code, _, errOut := runWithServer(t, srv, []string{"pm", "get", "pm_nope"}, nil)
	if code != 1 {
		t.Fatalf("want 1, got %d stderr=%s", code, errOut)
	}
	if !strings.Contains(errOut, "pm schedule not found") || !strings.Contains(errOut, "request_id=rid-pg") {
		t.Fatalf("stderr=%q", errOut)
	}
}

func TestPMGetMissingArg(t *testing.T) {
	srv := newFakeCore(t)
	defer srv.Close()
	code, _, errOut := runWithServer(t, srv, []string{"pm", "get"}, nil)
	if code != 2 {
		t.Fatalf("want 2, got %d stderr=%s", code, errOut)
	}
}

// --- create --------------------------------------------------------------

func TestPMCreateSuccess(t *testing.T) {
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/pm/schedules", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				writeProblem(w, http.StatusMethodNotAllowed, "bad-request", "method", "", r.URL.Path, "rid")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"id":"pm_new","asset_path":"p","kind":"filter_swap","interval_days":30,"next_due":"2026-07-12T10:00:00Z","title":"t","severity":"minor","enabled":true}`+"\n")
		})
	})
	defer srv.Close()
	code, out, _ := runWithServer(t, srv, []string{"pm", "create", "--asset", "p", "--title", "t", "--severity", "minor", "--interval-days", "30"}, nil)
	if code != 0 {
		t.Fatalf("want 0, got %d", code)
	}
	if !strings.Contains(out, "created pm_new") {
		t.Fatalf("out=%q", out)
	}
}

func TestPMCreateMissingFlags(t *testing.T) {
	srv := newFakeCore(t)
	defer srv.Close()
	code, _, errOut := runWithServer(t, srv, []string{"pm", "create", "--asset", "p"}, nil)
	if code != 2 {
		t.Fatalf("want 2, got %d stderr=%s", code, errOut)
	}
}

func TestPMCreateBadInterval(t *testing.T) {
	// interval-days=0 → local validation failure → exit 2.
	srv := newFakeCore(t)
	defer srv.Close()
	code, _, errOut := runWithServer(t, srv, []string{"pm", "create", "--asset", "p", "--title", "t", "--severity", "minor", "--interval-days", "0"}, nil)
	if code != 2 {
		t.Fatalf("want 2, got %d stderr=%s", code, errOut)
	}
}

// 400 → exit1: bad input (e.g. asset path syntax).
func TestPMCreate400(t *testing.T) {
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/pm/schedules", func(w http.ResponseWriter, r *http.Request) {
			writeProblem(w, http.StatusBadRequest, "bad-path", "bad asset path", "syntax", r.URL.Path, "rid-pc")
		})
	})
	defer srv.Close()
	code, _, errOut := runWithServer(t, srv, []string{"pm", "create", "--asset", "bad", "--title", "t", "--severity", "minor", "--interval-days", "30"}, nil)
	if code != 1 {
		t.Fatalf("want 1, got %d stderr=%s", code, errOut)
	}
	if !strings.Contains(errOut, "bad asset path") || !strings.Contains(errOut, "request_id=rid-pc") {
		t.Fatalf("stderr=%q", errOut)
	}
}

func TestPMUnknownSub(t *testing.T) {
	srv := newFakeCore(t)
	defer srv.Close()
	code, _, errOut := runWithServer(t, srv, []string{"pm", "frobnicate"}, nil)
	if code != 2 {
		t.Fatalf("want 2, got %d stderr=%s", code, errOut)
	}
}
