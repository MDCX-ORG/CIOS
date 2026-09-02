// Package cli — ticket_test.go: end-to-end coverage of
// `cios ticket list|get|open|ack|resolve|close|note|assign` against
// an httptest fake core. Each test owns the full mux layout
// (Go 1.22 mux panics on duplicate registration) and only registers
// the endpoints it exercises.
package cli

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// --- list ----------------------------------------------------------------

func TestTicketListEmpty(t *testing.T) {
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/tickets", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"items":[],"next_page_token":""}`+"\n")
		})
	})
	defer srv.Close()
	code, _, errOut := runWithServer(t, srv, []string{"ticket", "list"}, nil)
	if code != 0 {
		t.Fatalf("want 0, got %d stderr=%s", code, errOut)
	}
	if !strings.Contains(errOut, "no tickets") {
		t.Fatalf("stderr=%q", errOut)
	}
}

func TestTicketListTable(t *testing.T) {
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/tickets", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"items":[{"id":"tk_abcdefgh","asset_path":"site01.pod000.cdu000","title":"inlet temp high","severity":"critical","state":"open","assignee":"alice","opened_at":"2026-06-12T10:00:00Z"}],"next_page_token":""}`+"\n")
		})
	})
	defer srv.Close()
	code, out, _ := runWithServer(t, srv, []string{"-o", "table", "ticket", "list"}, nil)
	if code != 0 {
		t.Fatalf("want 0, got %d", code)
	}
	for _, s := range []string{"ID", "STATE", "SEVERITY", "ASSET", "TITLE", "tk_abcdefgh", "open", "critical", "inlet temp high"} {
		if !strings.Contains(out, s) {
			t.Fatalf("table missing token %q in:\n%s", s, out)
		}
	}
}

func TestTicketListJSON(t *testing.T) {
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/tickets", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"items":[{"id":"tk_1","asset_path":"p","title":"t","severity":"info","state":"open","assignee":"","opened_at":"2026-06-12T10:00:00Z"}],"next_page_token":""}`+"\n")
		})
	})
	defer srv.Close()
	code, out, _ := runWithServer(t, srv, []string{"-o", "json", "ticket", "list"}, nil)
	if code != 0 {
		t.Fatalf("want 0, got %d", code)
	}
	// Print re-marshals from the parsed slice (MarshalIndent), so the
	// canonical token shape is "id": "tk_1" with the surrounding space.
	if !strings.Contains(out, `"id": "tk_1"`) {
		t.Fatalf("json out=%q", out)
	}
}

func TestTicketListFilters(t *testing.T) {
	var seenQuery string
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/tickets", func(w http.ResponseWriter, r *http.Request) {
			seenQuery = r.URL.RawQuery
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"items":[],"next_page_token":""}`+"\n")
		})
	})
	defer srv.Close()
	code, _, _ := runWithServer(t, srv, []string{"ticket", "list", "--severity", "critical", "--state", "open", "--filter", "site01.*"}, nil)
	if code != 0 {
		t.Fatalf("want 0, got %d", code)
	}
	if !strings.Contains(seenQuery, "severity=critical") || !strings.Contains(seenQuery, "state=open") || !strings.Contains(seenQuery, "filter=site01") {
		t.Fatalf("missing filters in query: %s", seenQuery)
	}
}

func TestTicketListRFC7807(t *testing.T) {
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/tickets", func(w http.ResponseWriter, r *http.Request) {
			writeProblem(w, http.StatusInternalServerError, "store-error", "store error", "disk", r.URL.Path, "rid-tl")
		})
	})
	defer srv.Close()
	code, _, errOut := runWithServer(t, srv, []string{"ticket", "list"}, nil)
	if code != 1 {
		t.Fatalf("want 1, got %d stderr=%s", code, errOut)
	}
	if !strings.Contains(errOut, "store error") || !strings.Contains(errOut, "request_id=rid-tl") {
		t.Fatalf("stderr=%q", errOut)
	}
}

// --- get -----------------------------------------------------------------

func TestTicketGetSuccess(t *testing.T) {
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/tickets/tk_1", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"id":"tk_1","asset_path":"p","title":"t","severity":"major","state":"open","assignee":"","opened_at":"2026-06-12T10:00:00Z"}`+"\n")
		})
	})
	defer srv.Close()
	code, out, _ := runWithServer(t, srv, []string{"ticket", "get", "tk_1"}, nil)
	if code != 0 {
		t.Fatalf("want 0, got %d", code)
	}
	for _, s := range []string{"ID", "STATE", "SEVERITY", "ASSET", "TITLE", "tk_1", "open", "major", "p"} {
		if !strings.Contains(out, s) {
			t.Fatalf("get row missing %q in:\n%s", s, out)
		}
	}
}

func TestTicketGetJSON(t *testing.T) {
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/tickets/tk_1", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"id":"tk_1","asset_path":"p","title":"t","severity":"info","state":"closed","assignee":"","opened_at":"2026-06-12T10:00:00Z","closed_at":"2026-06-12T11:00:00Z"}`+"\n")
		})
	})
	defer srv.Close()
	code, out, _ := runWithServer(t, srv, []string{"-o", "json", "ticket", "get", "tk_1"}, nil)
	if code != 0 {
		t.Fatalf("want 0, got %d", code)
	}
	if !strings.Contains(out, `"id":"tk_1"`) {
		t.Fatalf("json out=%q", out)
	}
}

func TestTicketGet404(t *testing.T) {
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/tickets/tk_nope", func(w http.ResponseWriter, r *http.Request) {
			writeProblem(w, http.StatusNotFound, "path-not-found", "ticket not found", "tk_nope", r.URL.Path, "rid-tg")
		})
	})
	defer srv.Close()
	code, _, errOut := runWithServer(t, srv, []string{"ticket", "get", "tk_nope"}, nil)
	if code != 1 {
		t.Fatalf("want 1, got %d", code)
	}
	if !strings.Contains(errOut, "ticket not found") || !strings.Contains(errOut, "request_id=rid-tg") {
		t.Fatalf("stderr=%q", errOut)
	}
}

func TestTicketGetMissingArg(t *testing.T) {
	srv := newFakeCore(t)
	defer srv.Close()
	code, _, errOut := runWithServer(t, srv, []string{"ticket", "get"}, nil)
	if code != 2 {
		t.Fatalf("want 2, got %d stderr=%s", code, errOut)
	}
}

func TestTicketUnknownSub(t *testing.T) {
	srv := newFakeCore(t)
	defer srv.Close()
	code, _, errOut := runWithServer(t, srv, []string{"ticket", "frobnicate"}, nil)
	if code != 2 {
		t.Fatalf("want 2, got %d stderr=%s", code, errOut)
	}
}

// --- open ----------------------------------------------------------------

func TestTicketOpenSuccess(t *testing.T) {
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/tickets", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				writeProblem(w, http.StatusMethodNotAllowed, "bad-request", "method", "", r.URL.Path, "rid")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"id":"tk_new","asset_path":"p","title":"t","severity":"major","state":"open","assignee":"","opened_at":"2026-06-12T10:00:00Z"}`+"\n")
		})
	})
	defer srv.Close()
	code, out, _ := runWithServer(t, srv, []string{"ticket", "open", "--asset", "p", "--title", "t", "--severity", "major"}, nil)
	if code != 0 {
		t.Fatalf("want 0, got %d", code)
	}
	if !strings.Contains(out, "opened tk_new") {
		t.Fatalf("out=%q", out)
	}
}

func TestTicketOpenMissingFlags(t *testing.T) {
	srv := newFakeCore(t)
	defer srv.Close()
	code, _, errOut := runWithServer(t, srv, []string{"ticket", "open", "--asset", "p"}, nil)
	if code != 2 {
		t.Fatalf("want 2, got %d stderr=%s", code, errOut)
	}
}

func TestTicketOpen400(t *testing.T) {
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/tickets", func(w http.ResponseWriter, r *http.Request) {
			writeProblem(w, http.StatusBadRequest, "bad-path", "bad asset path", "syntax", r.URL.Path, "rid-to")
		})
	})
	defer srv.Close()
	code, _, errOut := runWithServer(t, srv, []string{"ticket", "open", "--asset", "bad", "--title", "t", "--severity", "major"}, nil)
	if code != 1 {
		t.Fatalf("want 1, got %d stderr=%s", code, errOut)
	}
	if !strings.Contains(errOut, "bad asset path") || !strings.Contains(errOut, "request_id=rid-to") {
		t.Fatalf("stderr=%q", errOut)
	}
}

// --- transition: ack/resolve/close (state-machine POST) ----------------

func TestTicketTransitionAckSuccess(t *testing.T) {
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/tickets/tk_1:transition", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				writeProblem(w, http.StatusMethodNotAllowed, "bad-request", "method", "", r.URL.Path, "rid")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"id":"tk_1","asset_path":"p","title":"t","severity":"major","state":"acknowledged","assignee":"alice","opened_at":"2026-06-12T10:00:00Z","acked_at":"2026-06-12T10:30:00Z"}`+"\n")
		})
	})
	defer srv.Close()
	code, out, _ := runWithServer(t, srv, []string{"ticket", "ack", "tk_1"}, nil)
	if code != 0 {
		t.Fatalf("want 0, got %d", code)
	}
	if !strings.Contains(out, "acknowledged tk_1") {
		t.Fatalf("out=%q", out)
	}
}

func TestTicketTransitionResolveSuccess(t *testing.T) {
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/tickets/tk_1:transition", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"id":"tk_1","asset_path":"p","title":"t","severity":"major","state":"resolved","assignee":"alice","opened_at":"2026-06-12T10:00:00Z","resolved_at":"2026-06-12T11:00:00Z"}`+"\n")
		})
	})
	defer srv.Close()
	code, out, _ := runWithServer(t, srv, []string{"ticket", "resolve", "tk_1"}, nil)
	if code != 0 {
		t.Fatalf("want 0, got %d", code)
	}
	if !strings.Contains(out, "resolved tk_1") {
		t.Fatalf("out=%q", out)
	}
}

func TestTicketTransitionCloseSuccess(t *testing.T) {
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/tickets/tk_1:transition", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"id":"tk_1","asset_path":"p","title":"t","severity":"major","state":"closed","assignee":"alice","opened_at":"2026-06-12T10:00:00Z","closed_at":"2026-06-12T12:00:00Z"}`+"\n")
		})
	})
	defer srv.Close()
	code, out, _ := runWithServer(t, srv, []string{"ticket", "close", "tk_1"}, nil)
	if code != 0 {
		t.Fatalf("want 0, got %d", code)
	}
	if !strings.Contains(out, "closed tk_1") {
		t.Fatalf("out=%q", out)
	}
}

// 422 → exit1 + RFC7807: invalid state transition (e.g. ack on
// already-closed ticket).
func TestTicketTransitionInvalid422(t *testing.T) {
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/tickets/tk_1:transition", func(w http.ResponseWriter, r *http.Request) {
			writeProblem(w, http.StatusUnprocessableEntity, "invalid-transition",
				"invalid transition", "cannot ack a closed ticket", r.URL.Path, "rid-ti")
		})
	})
	defer srv.Close()
	code, _, errOut := runWithServer(t, srv, []string{"ticket", "ack", "tk_1"}, nil)
	if code != 1 {
		t.Fatalf("want 1, got %d stderr=%s", code, errOut)
	}
	if !strings.Contains(errOut, "invalid transition") || !strings.Contains(errOut, "request_id=rid-ti") {
		t.Fatalf("stderr=%q", errOut)
	}
}

func TestTicketTransitionMissingArg(t *testing.T) {
	srv := newFakeCore(t)
	defer srv.Close()
	code, _, errOut := runWithServer(t, srv, []string{"ticket", "ack"}, nil)
	if code != 2 {
		t.Fatalf("want 2, got %d stderr=%s", code, errOut)
	}
}

// --- net error: server unreachable ---------------------------------------

func TestTicketListNetError(t *testing.T) {
	t.Setenv("CIOS_SERVER", "http://127.0.0.1:1")
	code, _, errOut := runWithServer(t, nil, []string{"ticket", "list"}, nil)
	if code != 1 {
		t.Fatalf("want 1, got %d stderr=%s", code, errOut)
	}
	if !strings.Contains(errOut, "net") {
		t.Fatalf("stderr=%q", errOut)
	}
}
