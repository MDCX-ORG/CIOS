// Package cli — extra_test.go: additional cases to push cli/ coverage
// above the 85% threshold called out in PRMT-012 §7. These cover
// error paths and branches that the main suite does not exercise.
package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// --- output.Print unknown mode -------------------------------------------

func TestPrintUnknownMode(t *testing.T) {
	err := Print(io.Discard, "xml", []any{1}, TableSpec{Columns: []string{"A"}})
	if err == nil || !strings.Contains(err.Error(), "unknown mode") {
		t.Fatalf("want unknown mode error, got %v", err)
	}
}

func TestPrintJSONMarshal(t *testing.T) {
	// channels cannot be JSON-marshalled → Print returns an error.
	ch := make(chan int)
	err := Print(io.Discard, "json", ch, TableSpec{})
	if err == nil {
		t.Fatalf("want json marshal error")
	}
}

// --- client.Do error paths -----------------------------------------------

func TestClientDoNetworkError(t *testing.T) {
	// Point at a closed port.
	c := NewClient("http://127.0.0.1:1")
	status, body, err := c.Do("GET", "/", nil, nil)
	if status != 0 {
		t.Fatalf("want status 0, got %d", status)
	}
	if body != nil {
		t.Fatalf("want nil body, got %s", body)
	}
	if _, ok := IsNetError(err); !ok {
		t.Fatalf("want *NetError, got %T %v", err, err)
	}
}

func TestClientDoProblemBadJSON(t *testing.T) {
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/assets/badprob", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(500)
			_, _ = io.WriteString(w, "not json{{{")
		})
	})
	defer srv.Close()
	c := NewClient(srv.URL)
	status, body, err := c.Do("GET", "/v1/assets/badprob", nil, nil)
	if status != 500 {
		t.Fatalf("want status 500, got %d", status)
	}
	if err != nil {
		t.Fatalf("want nil err (fall through per §4.3), got %v", err)
	}
	if !strings.Contains(string(body), "not json") {
		t.Fatalf("body=%q", body)
	}
}

func TestProblemErrorNoRequestID(t *testing.T) {
	p := &Problem{Title: "boom"}
	if got := p.Error(); got != "boom" {
		t.Fatalf("want 'boom', got %q", got)
	}
}

func TestProblemErrorEmpty(t *testing.T) {
	p := &Problem{}
	if got := p.Error(); got != "problem" {
		t.Fatalf("want 'problem', got %q", got)
	}
}

func TestNetErrorUnwrap(t *testing.T) {
	inner := &simpleErr{"dial fail"}
	ne := &NetError{Op: "do", Err: inner}
	if ne.Unwrap() != inner {
		t.Fatalf("unwrap mismatch")
	}
	if !strings.Contains(ne.Error(), "net do: dial fail") {
		t.Fatalf("Error()=%q", ne.Error())
	}
}

type simpleErr struct{ s string }

func (e *simpleErr) Error() string { return e.s }

// --- root.go dispatch paths ----------------------------------------------

func TestMainNoArgs(t *testing.T) {
	code, _, errOut := runWithServer(t, nil, []string{}, nil)
	if code != 2 {
		t.Fatalf("want 2, got %d", code)
	}
	if !strings.Contains(errOut, "usage") {
		t.Fatalf("errOut=%q", errOut)
	}
}

func TestMainUnknownFlag(t *testing.T) {
	code, _, errOut := runWithServer(t, nil, []string{"-frobnicate", "version"}, nil)
	if code != 2 {
		t.Fatalf("want 2, got %d stderr=%s", code, errOut)
	}
}

func TestMainVersionUsageError(t *testing.T) {
	code, _, errOut := runWithServer(t, nil, []string{"version", "-bogus"}, nil)
	if code != 2 {
		t.Fatalf("want 2, got %d stderr=%s", code, errOut)
	}
}

func TestMainServerFlag(t *testing.T) {
	srv := newFakeCore(t)
	defer srv.Close()
	// -s with embedded = form and -o to verify global parsing.
	code, out, _ := runWithServer(t, srv, []string{"-o", "table", "version"}, nil)
	if code != 0 || strings.TrimSpace(out) != "cios dev" {
		t.Fatalf("want version, got code=%d out=%q", code, out)
	}
}

func TestMainOutputFlagAsGlobal(t *testing.T) {
	// -o in a position that suggests global (between -s and the subcommand).
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/assets", func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, `{"items":[],"next_page_token":""}`+"\n")
		})
	})
	defer srv.Close()
	code, _, _ := runWithServer(t, srv, []string{"-o", "json", "asset", "list"}, nil)
	if code != 0 {
		t.Fatalf("want 0, got %d", code)
	}
}

// --- apply: json/yaml modes ---------------------------------------------

func TestApplyJSON(t *testing.T) {
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/assets/p1", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"path":"p1","resource_version":7,"spec":{"type":"cdu"},"created_at":"","updated_at":""}`+"\n")
		})
	})
	defer srv.Close()
	doc := "kind: Asset\nmetadata: {path: p1}\nspec: {type: cdu}\n"
	code, out, _ := runWithServer(t, srv, []string{"-o", "json", "apply", "-f", "-"}, strings.NewReader(doc))
	if code != 0 {
		t.Fatalf("want 0, got %d", code)
	}
	var v map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &v); err != nil {
		t.Fatalf("json out: %v out=%q", err, out)
	}
	if v["path"] != "p1" || v["resource_version"].(float64) != 7 {
		t.Fatalf("unexpected: %v", v)
	}
}

func TestApplyYAML(t *testing.T) {
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/assets/p1", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"path":"p1","resource_version":1,"spec":{"type":"cdu"},"created_at":"","updated_at":""}`+"\n")
		})
	})
	defer srv.Close()
	doc := "kind: Asset\nmetadata: {path: p1}\nspec: {type: cdu}\n"
	code, out, _ := runWithServer(t, srv, []string{"-o", "yaml", "apply", "-f", "-"}, strings.NewReader(doc))
	if code != 0 {
		t.Fatalf("want 0, got %d", code)
	}
	if !strings.Contains(out, "path: p1") {
		t.Fatalf("yaml out: %q", out)
	}
}

func TestApplyBadYAML(t *testing.T) {
	srv := newFakeCore(t)
	defer srv.Close()
	code, _, errOut := runWithServer(t, srv, []string{"apply", "-f", "-"}, strings.NewReader("kind: Asset\nmetadata:\n  path: p1\nspec: [unbalanced"))
	if code != 2 {
		t.Fatalf("want 2, got %d stderr=%s", code, errOut)
	}
	if !strings.Contains(errOut, "yaml") {
		t.Fatalf("stderr=%q", errOut)
	}
}

func TestApplyBadKind(t *testing.T) {
	srv := newFakeCore(t)
	defer srv.Close()
	code, _, errOut := runWithServer(t, srv, []string{"apply", "-f", "-"}, strings.NewReader("kind: NotAsset\nmetadata: {path: p1}\nspec: {}\n"))
	if code != 2 {
		t.Fatalf("want 2, got %d stderr=%s", code, errOut)
	}
	if !strings.Contains(errOut, "kind") {
		t.Fatalf("stderr=%q", errOut)
	}
}

func TestApplyMissingFlag(t *testing.T) {
	srv := newFakeCore(t)
	defer srv.Close()
	code, _, errOut := runWithServer(t, srv, []string{"apply"}, nil)
	if code != 2 {
		t.Fatalf("want 2, got %d stderr=%s", code, errOut)
	}
}

// --- delete cascade + error ----------------------------------------------

func TestDeleteCascade(t *testing.T) {
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/assets/parent", func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("cascade") != "true" {
				t.Errorf("want cascade=true, got %q", r.URL.RawQuery)
			}
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"deleted":3}`+"\n")
		})
	})
	defer srv.Close()
	code, out, errOut := runWithServer(t, srv, []string{"delete", "--cascade", "parent"}, nil)
	if code != 0 || !strings.Contains(out, "deleted parent") {
		t.Fatalf("code=%d out=%q stderr=%q", code, out, errOut)
	}
}

func TestDeleteMissingArg(t *testing.T) {
	srv := newFakeCore(t)
	defer srv.Close()
	code, _, errOut := runWithServer(t, srv, []string{"delete"}, nil)
	if code != 2 {
		t.Fatalf("want 2, got %d stderr=%s", code, errOut)
	}
}

func TestDeleteRFC7807(t *testing.T) {
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/assets/x", func(w http.ResponseWriter, r *http.Request) {
			writeProblem(w, http.StatusNotFound, "path-not-found", "asset path not found", "x", r.URL.Path, "rid-del")
		})
	})
	defer srv.Close()
	code, _, errOut := runWithServer(t, srv, []string{"delete", "x"}, nil)
	if code != 1 || !strings.Contains(errOut, "request_id=rid-del") {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
}

// --- asset: filter + bad page-size + json list --------------------------

func TestAssetListWithFilter(t *testing.T) {
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/assets", func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("filter") != "site01.*" {
				t.Errorf("want filter=site01.*, got %q", r.URL.RawQuery)
			}
			io.WriteString(w, `{"items":[],"next_page_token":""}`+"\n")
		})
	})
	defer srv.Close()
	code, _, _ := runWithServer(t, srv, []string{"asset", "list", "--filter", "site01.*"}, nil)
	if code != 0 {
		t.Fatalf("want 0, got %d", code)
	}
}

func TestAssetUnknownSub(t *testing.T) {
	srv := newFakeCore(t)
	defer srv.Close()
	code, _, errOut := runWithServer(t, srv, []string{"asset", "frobnicate"}, nil)
	if code != 2 {
		t.Fatalf("want 2, got %d stderr=%s", code, errOut)
	}
}

func TestAssetNoSub(t *testing.T) {
	srv := newFakeCore(t)
	defer srv.Close()
	code, _, _ := runWithServer(t, srv, []string{"asset"}, nil)
	if code != 2 {
		t.Fatalf("want 2, got %d", code)
	}
}

func TestAssetGetMissingArg(t *testing.T) {
	srv := newFakeCore(t)
	defer srv.Close()
	code, _, errOut := runWithServer(t, srv, []string{"asset", "get"}, nil)
	if code != 2 {
		t.Fatalf("want 2, got %d stderr=%s", code, errOut)
	}
}

// --- query: bad JSON, no path, metric no sub -----------------------------

func TestQueryBadJSON(t *testing.T) {
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/points/p", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `not json`)
		})
	})
	defer srv.Close()
	code, _, errOut := runWithServer(t, srv, []string{"query", "p"}, nil)
	if code != 1 || !strings.Contains(errOut, "decode") {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
}

func TestQueryNonProblemError(t *testing.T) {
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/points/p", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(500)
			io.WriteString(w, "boom")
		})
	})
	defer srv.Close()
	code, _, errOut := runWithServer(t, srv, []string{"query", "p"}, nil)
	if code != 1 {
		t.Fatalf("want 1, got %d stderr=%s", code, errOut)
	}
}

func TestQueryMissingArg(t *testing.T) {
	srv := newFakeCore(t)
	defer srv.Close()
	code, _, _ := runWithServer(t, srv, []string{"query"}, nil)
	if code != 2 {
		t.Fatalf("want 2, got %d", code)
	}
}

func TestMetricUnknownSub(t *testing.T) {
	srv := newFakeCore(t)
	defer srv.Close()
	code, _, _ := runWithServer(t, srv, []string{"metric", "range"}, nil)
	if code != 2 {
		t.Fatalf("want 2, got %d", code)
	}
}

func TestMetricNoSub(t *testing.T) {
	srv := newFakeCore(t)
	defer srv.Close()
	code, _, _ := runWithServer(t, srv, []string{"metric"}, nil)
	if code != 2 {
		t.Fatalf("want 2, got %d", code)
	}
}

func TestMetricQueryMissingArg(t *testing.T) {
	srv := newFakeCore(t)
	defer srv.Close()
	code, _, _ := runWithServer(t, srv, []string{"metric", "query"}, nil)
	if code != 2 {
		t.Fatalf("want 2, got %d", code)
	}
}

func TestMetricQueryRFC7807NonProblem(t *testing.T) {
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/metrics/query", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(500)
			io.WriteString(w, "boom")
		})
	})
	defer srv.Close()
	code, _, _ := runWithServer(t, srv, []string{"metric", "query", "x"}, nil)
	if code != 1 {
		t.Fatalf("want 1, got %d", code)
	}
}

func TestMetricQueryBadJSON(t *testing.T) {
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/metrics/query", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, "not json")
		})
	})
	defer srv.Close()
	code, _, errOut := runWithServer(t, srv, []string{"metric", "query", "x"}, nil)
	if code != 1 || !strings.Contains(errOut, "decode") {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
}

// --- alarm: unknown sub + filter -----------------------------------------

func TestAlarmUnknownSub(t *testing.T) {
	srv := newFakeCore(t)
	defer srv.Close()
	code, _, _ := runWithServer(t, srv, []string{"alarm", "ack"}, nil)
	if code != 2 {
		t.Fatalf("want 2, got %d", code)
	}
}

func TestAlarmNoSub(t *testing.T) {
	srv := newFakeCore(t)
	defer srv.Close()
	code, _, _ := runWithServer(t, srv, []string{"alarm"}, nil)
	if code != 2 {
		t.Fatalf("want 2, got %d", code)
	}
}

func TestAlarmListWithFilters(t *testing.T) {
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/alarms", func(w http.ResponseWriter, r *http.Request) {
			q := r.URL.Query()
			if q.Get("severity") != "critical" || q.Get("state") != "firing" || q.Get("filter") != "site01.*" {
				t.Errorf("missing filters: %s", r.URL.RawQuery)
			}
			io.WriteString(w, `{"items":[],"next_page_token":""}`+"\n")
		})
	})
	defer srv.Close()
	code, _, _ := runWithServer(t, srv, []string{"alarm", "list", "--severity", "critical", "--state", "firing", "--filter", "site01.*"}, nil)
	if code != 0 {
		t.Fatalf("want 0, got %d", code)
	}
}

// --- pagination overflow --------------------------------------------------

func TestAssetListPaginationOverflow(t *testing.T) {
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/assets", func(w http.ResponseWriter, r *http.Request) {
			// Always return a non-empty next_page_token.
			io.WriteString(w, `{"items":[{"path":"a","resource_version":1,"spec":{"type":"cdu"}}],"next_page_token":"next"}`+"\n")
		})
	})
	defer srv.Close()
	code, _, errOut := runWithServer(t, srv, []string{"asset", "list"}, nil)
	if code != 1 || !strings.Contains(errOut, "pagination overflow") {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
}

func TestAlarmListPaginationOverflow(t *testing.T) {
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/alarms", func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, `{"items":[{"id":"a","path":"p","severity":"info","state":"firing","summary":"s","since":"2026-01-01T00:00:00Z"}],"next_page_token":"next"}`+"\n")
		})
	})
	defer srv.Close()
	code, _, errOut := runWithServer(t, srv, []string{"alarm", "list"}, nil)
	if code != 1 || !strings.Contains(errOut, "pagination overflow") {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
}

// --- mid-loop RFC7807 aborts ---------------------------------------------

func TestAssetListMidLoopProblem(t *testing.T) {
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/assets", func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("page_token") == "" {
				io.WriteString(w, `{"items":[{"path":"a","resource_version":1,"spec":{"type":"cdu"}}],"next_page_token":"next"}`+"\n")
				return
			}
			writeProblem(w, http.StatusInternalServerError, "bad-request", "store error", "disk", r.URL.Path, "rid-loop")
		})
	})
	defer srv.Close()
	code, _, errOut := runWithServer(t, srv, []string{"asset", "list"}, nil)
	if code != 1 || !strings.Contains(errOut, "store error") {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
}

// --- renderTable path ----------------------------------------------------

func TestRenderTableRowWidth(t *testing.T) {
	// Row returns wrong number of cells → renderTable returns error.
	v := []any{"x"}
	tbl := TableSpec{
		Columns: []string{"A", "B"},
		Row:     func(any) []string { return []string{"only-one"} },
	}
	err := Print(io.Discard, "table", v, tbl)
	if err == nil {
		t.Fatalf("want row-width error")
	}
}

// --- client.Do with non-nil body -----------------------------------------

func TestClientDoWithBody(t *testing.T) {
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/assets/p1", func(w http.ResponseWriter, r *http.Request) {
			var got map[string]any
			raw, _ := io.ReadAll(r.Body)
			json.Unmarshal(raw, &got)
			if got["k"] != "v" {
				t.Errorf("body not as expected: %s", raw)
			}
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"path":"p1","resource_version":1,"spec":{"type":"cdu"},"created_at":"","updated_at":""}`+"\n")
		})
	})
	defer srv.Close()
	c := NewClient(srv.URL)
	status, _, err := c.Do("PUT", "/v1/assets/p1", nil, map[string]any{"k": "v"})
	if status/100 != 2 || err != nil {
		t.Fatalf("status=%d err=%v", status, err)
	}
}

// --- output helpers ------------------------------------------------------

func TestFormatRFC3339StringInvalid(t *testing.T) {
	// Unparseable → raw passthrough.
	if got := formatRFC3339String("not-a-time"); got != "not-a-time" {
		t.Fatalf("want passthrough, got %q", got)
	}
}

func TestMetricTimeStringInvalid(t *testing.T) {
	if got := metricTimeString("garbage"); got != "garbage" {
		t.Fatalf("want passthrough, got %q", got)
	}
}

// --- writeHTTPStatus path ------------------------------------------------

func TestApplyNonProblemHTTPError(t *testing.T) {
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/assets/p1", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(http.StatusInternalServerError)
			io.WriteString(w, "disk full")
		})
	})
	defer srv.Close()
	doc := "kind: Asset\nmetadata: {path: p1}\nspec: {type: cdu}\n"
	code, _, errOut := runWithServer(t, srv, []string{"apply", "-f", "-"}, strings.NewReader(doc))
	if code != 1 || !strings.Contains(errOut, "disk full") {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
}

// --- asset get json/yaml mode (passthrough) ------------------------------

func TestAssetGetJSON(t *testing.T) {
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/assets/p1", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"path":"p1","resource_version":2,"spec":{"type":"cdu"},"created_at":"2026-06-12T10:00:00Z","updated_at":"2026-06-12T11:00:00Z"}`+"\n")
		})
	})
	defer srv.Close()
	code, out, _ := runWithServer(t, srv, []string{"-o", "json", "asset", "get", "p1"}, nil)
	if code != 0 {
		t.Fatalf("want 0, got %d", code)
	}
	if !strings.Contains(out, `"path":"p1"`) || !strings.Contains(out, `"resource_version":2`) {
		t.Fatalf("out=%q", out)
	}
}

// --- root: program-name strip path ---------------------------------------

func TestMainStripsProgramName(t *testing.T) {
	srv := newFakeCore(t)
	defer srv.Close()
	// Don't go through runWithServer — invoke Main directly with the
	// "cios" program-name prefix to exercise the strip branch.
	out := &bytes.Buffer{}
	errOut := &bytes.Buffer{}
	code := Main([]string{"cios", "version"}, strings.NewReader(""), out, errOut)
	if code != 0 || strings.TrimSpace(out.String()) != "cios dev" {
		t.Fatalf("want version, got code=%d out=%q stderr=%q", code, out, errOut)
	}
}

func TestMainKeepsKnownSubcommandAtArgZero(t *testing.T) {
	// If args[0] is a known subcommand, we do NOT strip (defensive).
	srv := newFakeCore(t)
	defer srv.Close()
	code, out, _ := runWithServer(t, srv, []string{"version"}, nil)
	if code != 0 || strings.TrimSpace(out) != "cios dev" {
		t.Fatalf("want version, got code=%d out=%q", code, out)
	}
}

func TestExitCodeFor(t *testing.T) {
	if ExitCodeFor(nil) != 0 {
		t.Fatalf("nil err should map to 0")
	}
	if ExitCodeFor(io.EOF) != 1 {
		t.Fatalf("any err should map to 1")
	}
}

// --- -o output flag validation (PRMT-090) -------------------------------

func TestMainInvalidOutputFlag(t *testing.T) {
	// -o xml is not in the legal set {json,yaml,table}; must fail with
	// exit 2 and a readable stderr message — not silently fall back to
	// table.
	srv := newFakeCore(t)
	defer srv.Close()
	code, _, errOut := runWithServer(t, srv, []string{"-o", "xml", "version"}, nil)
	if code != 2 {
		t.Fatalf("want 2, got %d stderr=%s", code, errOut)
	}
	if !strings.Contains(errOut, "xml") {
		t.Fatalf("errOut should mention 'xml', got %q", errOut)
	}
	if !strings.Contains(errOut, "usage") {
		t.Fatalf("errOut should include usage, got %q", errOut)
	}
}

func TestMainValidOutputFlags(t *testing.T) {
	// All three legal values must continue to work (behavior unchanged).
	srv := newFakeCore(t)
	defer srv.Close()
	for _, mode := range []string{"json", "yaml", "table"} {
		code, _, errOut := runWithServer(t, srv, []string{"-o", mode, "version"}, nil)
		if code != 0 {
			t.Fatalf("-o %s: want 0, got %d stderr=%s", mode, code, errOut)
		}
	}
}

func TestMainInvalidOutputFlagBeforeSubcommand(t *testing.T) {
	// -o garbage placed before a subcommand must still be rejected at
	// parseGlobals (so the subcommand is never invoked).
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/assets", func(w http.ResponseWriter, r *http.Request) {
			t.Errorf("server should not be called for invalid -o")
		})
	})
	defer srv.Close()
	code, _, errOut := runWithServer(t, srv, []string{"-o", "garbage", "asset", "list"}, nil)
	if code != 2 {
		t.Fatalf("want 2, got %d stderr=%s", code, errOut)
	}
	if !strings.Contains(errOut, "garbage") {
		t.Fatalf("errOut should mention 'garbage', got %q", errOut)
	}
}
