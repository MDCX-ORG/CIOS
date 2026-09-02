// Package cli — case_test.go: end-to-end coverage of
// `cios case list` against an httptest fake core. The case
// command exposes a single subcommand (list) plus a --csv
// export mode. Each test owns the full mux layout.
package cli

import (
	"encoding/csv"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestCaseListEmpty(t *testing.T) {
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/cases", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"items":[]}`+"\n")
		})
	})
	defer srv.Close()
	code, out, _ := runWithServer(t, srv, []string{"case", "list"}, nil)
	if code != 0 {
		t.Fatalf("want 0, got %d", code)
	}
	if !strings.Contains(out, "CASES (closed tickets)") || !strings.Contains(out, "(none)") {
		t.Fatalf("empty table wrong: %q", out)
	}
}

func TestCaseListTable(t *testing.T) {
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/cases", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"items":[{"id":"tk_abcdefgh_ij","asset_path":"site01.pod000.cdu000","title":"pump swap","severity":"major","state":"closed","opened_at":"2026-06-12T10:00:00Z","closed_at":"2026-06-12T11:00:00Z","runbook":"RB-001"}]}`+"\n")
		})
	})
	defer srv.Close()
	code, out, _ := runWithServer(t, srv, []string{"case", "list"}, nil)
	if code != 0 {
		t.Fatalf("want 0, got %d", code)
	}
	for _, s := range []string{"CASES (closed tickets)", "CLOSED", "SEV", "ID", "TITLE", "2026-06-12T11:00:00Z", "major", "tk_abcd", "pump swap"} {
		if !strings.Contains(out, s) {
			t.Fatalf("table missing %q in:\n%s", s, out)
		}
	}
}

func TestCaseListJSON(t *testing.T) {
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/cases", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"items":[{"id":"tk_x","asset_path":"p","title":"t","severity":"info","state":"closed","opened_at":"2026-06-12T10:00:00Z","closed_at":"2026-06-12T11:00:00Z"}]}`+"\n")
		})
	})
	defer srv.Close()
	code, out, _ := runWithServer(t, srv, []string{"-o", "json", "case", "list"}, nil)
	if code != 0 {
		t.Fatalf("want 0, got %d", code)
	}
	if !strings.Contains(out, `"id":"tk_x"`) {
		t.Fatalf("json out=%q", out)
	}
}

func TestCaseListCSV(t *testing.T) {
	// --csv: response is streamed to stdout; verify the body is
	// a parseable CSV envelope and that the format=csv query is set.
	var seenFormat string
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/cases", func(w http.ResponseWriter, r *http.Request) {
			seenFormat = r.URL.Query().Get("format")
			w.Header().Set("Content-Type", "text/csv")
			io.WriteString(w, "id,asset_path,title,severity,closed_at\ntk_csv_1,p,csv case,major,2026-06-12T11:00:00Z\n")
		})
	})
	defer srv.Close()
	code, out, _ := runWithServer(t, srv, []string{"case", "list", "--csv"}, nil)
	if code != 0 {
		t.Fatalf("want 0, got %d", code)
	}
	if seenFormat != "csv" {
		t.Fatalf("want format=csv, got %q", seenFormat)
	}
	r := csv.NewReader(strings.NewReader(out))
	recs, err := r.ReadAll()
	if err != nil {
		t.Fatalf("csv parse: %v out=%q", err, out)
	}
	if len(recs) != 2 || recs[1][0] != "tk_csv_1" {
		t.Fatalf("csv wrong: %v", recs)
	}
}

func TestCaseListWithFilters(t *testing.T) {
	var seenQuery string
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/cases", func(w http.ResponseWriter, r *http.Request) {
			seenQuery = r.URL.RawQuery
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"items":[]}`+"\n")
		})
	})
	defer srv.Close()
	code, _, _ := runWithServer(t, srv, []string{"case", "list", "--filter", "site01.*", "--severity", "major", "--asset-prefix", "site01", "--since", "2026-06-01T00:00:00Z", "--until", "2026-06-30T00:00:00Z", "--limit", "10"}, nil)
	if code != 0 {
		t.Fatalf("want 0, got %d", code)
	}
	for _, want := range []string{"filter=site01", "severity=major", "asset_prefix=site01", "since=2026-06-01T00%3A00%3A00Z", "until=2026-06-30T00%3A00%3A00Z", "limit=10"} {
		if !strings.Contains(seenQuery, want) {
			t.Fatalf("missing %q in query: %s", want, seenQuery)
		}
	}
}

func TestCaseListBadSince(t *testing.T) {
	// Bad RFC3339 --since → local validation failure → exit 2.
	srv := newFakeCore(t)
	defer srv.Close()
	code, _, errOut := runWithServer(t, srv, []string{"case", "list", "--since", "garbage"}, nil)
	if code != 2 {
		t.Fatalf("want 2, got %d stderr=%s", code, errOut)
	}
	if !strings.Contains(errOut, "--since") {
		t.Fatalf("stderr=%q", errOut)
	}
}

func TestCaseListBadUntil(t *testing.T) {
	srv := newFakeCore(t)
	defer srv.Close()
	code, _, errOut := runWithServer(t, srv, []string{"case", "list", "--until", "garbage"}, nil)
	if code != 2 {
		t.Fatalf("want 2, got %d stderr=%s", code, errOut)
	}
}

func TestCaseListRFC7807(t *testing.T) {
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/cases", func(w http.ResponseWriter, r *http.Request) {
			writeProblem(w, http.StatusInternalServerError, "store-error", "store error", "disk", r.URL.Path, "rid-cl")
		})
	})
	defer srv.Close()
	code, _, errOut := runWithServer(t, srv, []string{"case", "list"}, nil)
	if code != 1 {
		t.Fatalf("want 1, got %d stderr=%s", code, errOut)
	}
	if !strings.Contains(errOut, "store error") || !strings.Contains(errOut, "request_id=rid-cl") {
		t.Fatalf("stderr=%q", errOut)
	}
}

func TestCaseUnknownSub(t *testing.T) {
	srv := newFakeCore(t)
	defer srv.Close()
	code, _, errOut := runWithServer(t, srv, []string{"case", "frobnicate"}, nil)
	if code != 2 {
		t.Fatalf("want 2, got %d stderr=%s", code, errOut)
	}
}
