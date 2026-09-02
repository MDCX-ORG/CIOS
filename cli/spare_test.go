// Package cli — spare_test.go: end-to-end coverage of
// `cios spare list|get|adjust` against an httptest fake core.
// Covers happy path, 422 insufficient-stock → exit1, and the
// 404 / net-error branches. Each test owns the full mux layout.
package cli

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// --- list ----------------------------------------------------------------

func TestSpareListEmpty(t *testing.T) {
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/spares", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"items":[],"next_page_token":""}`+"\n")
		})
	})
	defer srv.Close()
	code, _, errOut := runWithServer(t, srv, []string{"spare", "list"}, nil)
	if code != 0 {
		t.Fatalf("want 0, got %d stderr=%s", code, errOut)
	}
	if !strings.Contains(errOut, "no spares") {
		t.Fatalf("stderr=%q", errOut)
	}
}

func TestSpareListTable(t *testing.T) {
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/spares", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"items":[{"id":"sp_1","sku":"PUMP-A","name":"Pump A","qty":3,"min_qty":1,"location":"sgp01.rack1.shelf2"}],"next_page_token":""}`+"\n")
		})
	})
	defer srv.Close()
	code, out, _ := runWithServer(t, srv, []string{"spare", "list"}, nil)
	if code != 0 {
		t.Fatalf("want 0, got %d", code)
	}
	for _, s := range []string{"ID", "SKU", "NAME", "QTY", "MIN", "LOCATION", "LOW?", "sp_1", "PUMP-A", "Pump A", "sgp01.rack1.shelf2"} {
		if !strings.Contains(out, s) {
			t.Fatalf("table missing %q in:\n%s", s, out)
		}
	}
}

func TestSpareListLowStockMark(t *testing.T) {
	// qty=0, min=2 → LOW column should appear.
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/spares", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"items":[{"id":"sp_low","sku":"X","name":"x","qty":0,"min_qty":2,"location":"L"}],"next_page_token":""}`+"\n")
		})
	})
	defer srv.Close()
	code, out, _ := runWithServer(t, srv, []string{"spare", "list"}, nil)
	if code != 0 {
		t.Fatalf("want 0, got %d", code)
	}
	if !strings.Contains(out, "LOW") {
		t.Fatalf("LOW column missing in:\n%s", out)
	}
}

func TestSpareListJSON(t *testing.T) {
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/spares", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"items":[{"id":"sp_1","sku":"X","name":"x","qty":1,"min_qty":0,"location":""}],"next_page_token":""}`+"\n")
		})
	})
	defer srv.Close()
	code, out, _ := runWithServer(t, srv, []string{"-o", "json", "spare", "list"}, nil)
	if code != 0 {
		t.Fatalf("want 0, got %d", code)
	}
	// Print re-marshals from the parsed slice (MarshalIndent). Check
	// the canonical token shape rather than a literal compact string.
	if !strings.Contains(out, `"id": "sp_1"`) {
		t.Fatalf("json out=%q", out)
	}
}

func TestSpareListRFC7807(t *testing.T) {
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/spares", func(w http.ResponseWriter, r *http.Request) {
			writeProblem(w, http.StatusInternalServerError, "store-error", "store error", "disk", r.URL.Path, "rid-sl")
		})
	})
	defer srv.Close()
	code, _, errOut := runWithServer(t, srv, []string{"spare", "list"}, nil)
	if code != 1 {
		t.Fatalf("want 1, got %d stderr=%s", code, errOut)
	}
	if !strings.Contains(errOut, "store error") || !strings.Contains(errOut, "request_id=rid-sl") {
		t.Fatalf("stderr=%q", errOut)
	}
}

// --- get -----------------------------------------------------------------

func TestSpareGetSuccess(t *testing.T) {
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/spares/sp_1", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"id":"sp_1","sku":"PUMP-A","name":"Pump A","qty":3,"min_qty":1,"location":"sgp01.rack1.shelf2","low_stock":false}`+"\n")
		})
	})
	defer srv.Close()
	code, out, _ := runWithServer(t, srv, []string{"spare", "get", "sp_1"}, nil)
	if code != 0 {
		t.Fatalf("want 0, got %d", code)
	}
	for _, s := range []string{"ID", "sp_1", "SKU", "PUMP-A", "QTY", "3", "min 1", "LOCATION", "sgp01.rack1.shelf2"} {
		if !strings.Contains(out, s) {
			t.Fatalf("get row missing %q in:\n%s", s, out)
		}
	}
}

func TestSpareGetWithRecentTxns(t *testing.T) {
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/spares/sp_1", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"id":"sp_1","sku":"X","name":"x","qty":2,"min_qty":1,"location":"L","low_stock":true,"recent_txns":[{"id":"tx_1","spare_id":"sp_1","delta":-1,"ticket_id":"tk_1","at":"2026-06-12T10:00:00Z"}]}`+"\n")
		})
	})
	defer srv.Close()
	code, out, _ := runWithServer(t, srv, []string{"spare", "get", "sp_1"}, nil)
	if code != 0 {
		t.Fatalf("want 0, got %d", code)
	}
	// printSpareDetail prints "LOW      yes" and a RECENT list with
	// the txn's at, delta, and ticket_id (the txn id itself is not
	// in the rendered text).
	if !strings.Contains(out, "LOW") || !strings.Contains(out, "RECENT") || !strings.Contains(out, "delta=-1") || !strings.Contains(out, "ticket=tk_1") {
		t.Fatalf("missing low/recent fields in:\n%s", out)
	}
}

func TestSpareGet404(t *testing.T) {
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/spares/sp_nope", func(w http.ResponseWriter, r *http.Request) {
			writeProblem(w, http.StatusNotFound, "path-not-found", "spare not found", "sp_nope", r.URL.Path, "rid-sg")
		})
	})
	defer srv.Close()
	code, _, errOut := runWithServer(t, srv, []string{"spare", "get", "sp_nope"}, nil)
	if code != 1 {
		t.Fatalf("want 1, got %d stderr=%s", code, errOut)
	}
	if !strings.Contains(errOut, "spare not found") || !strings.Contains(errOut, "request_id=rid-sg") {
		t.Fatalf("stderr=%q", errOut)
	}
}

func TestSpareGetMissingArg(t *testing.T) {
	srv := newFakeCore(t)
	defer srv.Close()
	code, _, errOut := runWithServer(t, srv, []string{"spare", "get"}, nil)
	if code != 2 {
		t.Fatalf("want 2, got %d stderr=%s", code, errOut)
	}
}

// --- adjust --------------------------------------------------------------

func TestSpareAdjustSuccess(t *testing.T) {
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/spares/sp_1:adjust", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				writeProblem(w, http.StatusMethodNotAllowed, "bad-request", "method", "", r.URL.Path, "rid")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"id":"sp_1","sku":"X","name":"x","qty":5,"min_qty":1,"location":"L","low_stock":false}`+"\n")
		})
	})
	defer srv.Close()
	code, out, _ := runWithServer(t, srv, []string{"spare", "adjust", "--delta", "2", "sp_1"}, nil)
	if code != 0 {
		t.Fatalf("want 0, got %d", code)
	}
	if !strings.Contains(out, "sp_1 adjusted: qty=5") {
		t.Fatalf("out=%q", out)
	}
}

// 422 → exit1: outbound adjustment pushes qty below zero
// (insufficient stock).
func TestSpareAdjustInsufficientStock422(t *testing.T) {
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/spares/sp_1:adjust", func(w http.ResponseWriter, r *http.Request) {
			writeProblem(w, http.StatusUnprocessableEntity, "insufficient-stock",
				"insufficient stock", "qty 0, requested -3", r.URL.Path, "rid-sa")
		})
	})
	defer srv.Close()
	code, _, errOut := runWithServer(t, srv, []string{"spare", "adjust", "--delta=-3", "sp_1"}, nil)
	if code != 1 {
		t.Fatalf("want 1, got %d stderr=%s", code, errOut)
	}
	if !strings.Contains(errOut, "insufficient stock") || !strings.Contains(errOut, "request_id=rid-sa") {
		t.Fatalf("stderr=%q", errOut)
	}
}

func TestSpareAdjustZeroDelta(t *testing.T) {
	// --delta 0 is a local input validation failure → exit 2.
	srv := newFakeCore(t)
	defer srv.Close()
	code, _, errOut := runWithServer(t, srv, []string{"spare", "adjust", "sp_1", "--delta", "0"}, nil)
	if code != 2 {
		t.Fatalf("want 2, got %d stderr=%s", code, errOut)
	}
	if !strings.Contains(errOut, "--delta") {
		t.Fatalf("stderr=%q", errOut)
	}
}

func TestSpareAdjustMissingArg(t *testing.T) {
	srv := newFakeCore(t)
	defer srv.Close()
	code, _, errOut := runWithServer(t, srv, []string{"spare", "adjust", "--delta", "1"}, nil)
	if code != 2 {
		t.Fatalf("want 2, got %d stderr=%s", code, errOut)
	}
}

func TestSpareUnknownSub(t *testing.T) {
	srv := newFakeCore(t)
	defer srv.Close()
	code, _, errOut := runWithServer(t, srv, []string{"spare", "frobnicate"}, nil)
	if code != 2 {
		t.Fatalf("want 2, got %d stderr=%s", code, errOut)
	}
}
