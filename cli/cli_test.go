// Package cli — cli_test.go: end-to-end coverage of every command
// against an httptest fake core. The fake mirrors the shapes the
// real PRMT-011 core emits (problem+json for errors, JSON envelope
// for lists, server Asset JSON for apply/get).
package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// runWithServer points CLI_BASE at the test server and runs Main.
// We can't override the Base via flag, so we set -s/--server on the
// global flag and use the CIOS_SERVER-equivalent arg path.
func runWithServer(t *testing.T, srv *httptest.Server, args []string, stdin io.Reader) (int, string, string) {
	t.Helper()
	if srv != nil {
		args = append([]string{"-s", srv.URL}, args...)
	}
	out := &bytes.Buffer{}
	errOut := &bytes.Buffer{}
	if stdin == nil {
		stdin = strings.NewReader("")
	}
	code := Main(args, stdin, out, errOut)
	return code, out.String(), errOut.String()
}

// newFakeCore returns a server with default empty responses.
func newFakeCore(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	registerDefaults(mux)
	return httptest.NewServer(mux)
}

// registerDefaults wires up the M0 endpoints with safe, empty
// responses. Tests that exercise one endpoint call this first and
// then register their own handler for the endpoints they want to
// inspect. Go 1.22's mux panics on duplicate registration, so each
// test owns the full mux layout.
func registerDefaults(mux *http.ServeMux) {
	mux.HandleFunc("/v1/assets", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"items":[],"next_page_token":""}`)
			return
		}
		writeProblem(w, http.StatusMethodNotAllowed, "bad-request", "method not allowed", "", r.URL.Path, "test-rid")
	})
	mux.HandleFunc("/v1/assets/", func(w http.ResponseWriter, r *http.Request) {
		writeProblem(w, http.StatusNotFound, "path-not-found", "asset path not found", r.URL.Path, "/v1/assets/", "test-rid")
	})
	mux.HandleFunc("/v1/alarms", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"items":[],"next_page_token":""}`)
	})
	mux.HandleFunc("/v1/metrics/query", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"success","data":{"resultType":"vector","result":[]}}`)
	})
	mux.HandleFunc("/v1/points/", func(w http.ResponseWriter, r *http.Request) {
		writeProblem(w, http.StatusNotFound, "path-not-found", "asset path not found", r.URL.Path, r.URL.Path, "test-rid")
	})
}

// newFakeCoreWith gives the test full ownership of the mux. Tests
// that exercise one endpoint should call registerDefaults(mux) at
// the top of their register callback to install safe fallbacks for
// endpoints they are NOT inspecting, then re-register the endpoint
// they DO care about with their own handler.
func newFakeCoreWith(t *testing.T, register func(mux *http.ServeMux)) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	register(mux)
	return httptest.NewServer(mux)
}

// writeProblem helper for the fake.
func writeProblem(w http.ResponseWriter, status int, typeTail, title, detail, instance, rid string) {
	body := map[string]any{
		"type":       "https://cios.dev/errors/" + typeTail,
		"title":      title,
		"status":     status,
		"detail":     detail,
		"instance":   instance,
		"request_id": rid,
	}
	if detail == "" {
		delete(body, "detail")
	}
	if instance == "" {
		delete(body, "instance")
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// --- version -------------------------------------------------------------

func TestVersion(t *testing.T) {
	srv := newFakeCore(t)
	defer srv.Close()
	code, out, errOut := runWithServer(t, srv, []string{"version"}, nil)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errOut)
	}
	if strings.TrimSpace(out) != "cios dev" {
		t.Fatalf("out=%q", out)
	}
}

// --- apply ----------------------------------------------------------------

func TestApplySuccess(t *testing.T) {
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		registerDefaults(mux)
		mux.HandleFunc("/v1/assets/site01.pod000.cdu000", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPut {
				writeProblem(w, http.StatusMethodNotAllowed, "bad-request", "method", "", r.URL.Path, "rid")
				return
			}
			var body struct {
				RequestID string         `json:"request_id"`
				Spec      map[string]any `json:"spec"`
			}
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &body)
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"path":"site01.pod000.cdu000","resource_version":%d,"spec":%q,"created_at":"2026-06-12T10:00:00Z","updated_at":"2026-06-12T10:00:00Z"}`+"\n",
				1, string(raw))
			// Echo the request_id in a custom header for the test to read back.
			w.Header().Set("X-Echo-Rid", body.RequestID)
		})
	})
	defer srv.Close()
	doc := `kind: Asset
metadata:
  path: site01.pod000.cdu000
  request_id: 01HABCDEFGHIJKLMNOP
spec:
  type: cdu
`
	code, out, errOut := runWithServer(t, srv, []string{"apply", "-f", "-"}, strings.NewReader(doc))
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errOut)
	}
	if !strings.Contains(out, "applied site01.pod000.cdu000 (rv=1)") {
		t.Fatalf("out=%q", out)
	}
}

func TestApplyStdin(t *testing.T) {
	// -f - must read from stdin and PUT against /v1/assets/p1.
	var seen []byte
	var seenMethod string
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		registerDefaults(mux)
		mux.HandleFunc("/v1/assets/p1", func(w http.ResponseWriter, r *http.Request) {
			seenMethod = r.Method
			seen, _ = io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"path":"p1","resource_version":1,"spec":{"type":"x"},"created_at":"","updated_at":""}`+"\n")
		})
	})
	defer srv.Close()
	doc := "kind: Asset\nmetadata:\n  path: p1\nspec: {type: x}\n"
	code, out, errOut := runWithServer(t, srv, []string{"apply", "-f", "-"}, strings.NewReader(doc))
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errOut)
	}
	if seenMethod != "PUT" {
		t.Fatalf("want PUT, got %s", seenMethod)
	}
	if !bytes.Contains(seen, []byte(`"type":"x"`)) {
		t.Fatalf("server did not see spec in body: %s", seen)
	}
	if !strings.Contains(out, "applied p1") {
		t.Fatalf("out=%q", out)
	}
}

func TestApplyMissingFile(t *testing.T) {
	srv := newFakeCore(t)
	defer srv.Close()
	code, _, errOut := runWithServer(t, srv, []string{"apply", "-f", "/no/such/file.yaml"}, nil)
	if code != 2 {
		t.Fatalf("want exit 2, got %d stderr=%s", code, errOut)
	}
}

func TestApplyMissingPath(t *testing.T) {
	srv := newFakeCore(t)
	defer srv.Close()
	doc := "kind: Asset\nmetadata: {}\nspec: {}\n"
	code, _, errOut := runWithServer(t, srv, []string{"apply", "-f", "-"}, strings.NewReader(doc))
	if code != 2 {
		t.Fatalf("want exit 2, got %d stderr=%s", code, errOut)
	}
}

func TestApplyRFC7807(t *testing.T) {
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		registerDefaults(mux)
		mux.HandleFunc("/v1/assets/bad", func(w http.ResponseWriter, r *http.Request) {
			writeProblem(w, http.StatusBadRequest, "bad-path", "bad asset path", "x", r.URL.Path, "rid-001")
		})
	})
	defer srv.Close()
	doc := "kind: Asset\nmetadata: {path: bad}\nspec: {type: cdu}\n"
	code, _, errOut := runWithServer(t, srv, []string{"apply", "-f", "-"}, strings.NewReader(doc))
	if code != 1 {
		t.Fatalf("want exit 1, got %d stderr=%s", code, errOut)
	}
	if !strings.Contains(errOut, "bad asset path") || !strings.Contains(errOut, "request_id=rid-001") {
		t.Fatalf("stderr=%q", errOut)
	}
}

func TestApplyRequestIDGeneration(t *testing.T) {
	var rids []string
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		registerDefaults(mux)
		mux.HandleFunc("/v1/assets/p1", func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				RequestID string `json:"request_id"`
			}
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &body)
			rids = append(rids, body.RequestID)
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"path":"p1","resource_version":1,"spec":{},"created_at":"","updated_at":""}`+"\n")
		})
	})
	defer srv.Close()
	doc := "kind: Asset\nmetadata: {path: p1}\nspec: {type: x}\n"
	for i := 0; i < 2; i++ {
		code, _, errOut := runWithServer(t, srv, []string{"apply", "-f", "-"}, strings.NewReader(doc))
		if code != 0 {
			t.Fatalf("iter %d: exit=%d stderr=%s", i, code, errOut)
		}
	}
	if len(rids) != 2 || rids[0] == rids[1] {
		t.Fatalf("want two distinct generated rids, got %v", rids)
	}
	if !strings.HasPrefix(rids[0], "01H") || len(rids[0]) != 19 {
		t.Fatalf("rid format wrong: %q", rids[0])
	}
}

// --- delete --------------------------------------------------------------

func TestDeleteSuccess(t *testing.T) {
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		registerDefaults(mux)
		mux.HandleFunc("/v1/assets/p1", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodDelete {
				writeProblem(w, http.StatusMethodNotAllowed, "bad-request", "method", "", r.URL.Path, "rid")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"deleted":1}`+"\n")
		})
	})
	defer srv.Close()
	code, out, errOut := runWithServer(t, srv, []string{"delete", "p1"}, nil)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errOut)
	}
	if !strings.Contains(out, "deleted p1") {
		t.Fatalf("out=%q", out)
	}
}

// --- asset list ----------------------------------------------------------

func TestAssetListEmpty(t *testing.T) {
	srv := newFakeCore(t)
	defer srv.Close()
	code, out, errOut := runWithServer(t, srv, []string{"asset", "list"}, nil)
	if code != 0 {
		t.Fatalf("want 0, got %d stderr=%s", code, errOut)
	}
	if !strings.Contains(errOut, "no assets") {
		t.Fatalf("stderr=%q", out)
	}
}

func TestAssetListTable(t *testing.T) {
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/assets", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"items":[{"path":"site01.pod000.cdu000","resource_version":3,"spec":{"type":"cdu"},"created_at":"2026-06-12T10:00:00Z","updated_at":"2026-06-12T11:00:00Z"}],"next_page_token":""}`+"\n")
		})
	})
	defer srv.Close()
	code, out, errOut := runWithServer(t, srv, []string{"-o", "table", "asset", "list"}, nil)
	if code != 0 {
		t.Fatalf("want 0, got %d stderr=%s", code, errOut)
	}
	want := "PATH\tTYPE\tRV\tUPDATED\nsite01.pod000.cdu000\tcdu\t3\t2026-06-12T11:00:00Z\n"
	// tabwriter pads with spaces; compare ignoring whitespace after
	// each tab-aligned column by checking key tokens instead.
	for _, s := range []string{"PATH", "TYPE", "RV", "UPDATED", "site01.pod000.cdu000", "cdu", "3", "2026-06-12T11:00:00Z"} {
		if !strings.Contains(out, s) {
			t.Fatalf("table missing token %q in:\n%s", s, out)
		}
	}
	if !strings.HasPrefix(out, "PATH") || !strings.Contains(out, "site01.pod000.cdu000") {
		t.Fatalf("unexpected output: %q", out)
	}
	_ = want
}

func TestAssetListJSON(t *testing.T) {
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/assets", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"items":[{"path":"a","resource_version":1,"spec":{"type":"cdu"}}],"next_page_token":""}`+"\n")
		})
	})
	code, out, _ := runWithServer(t, srv, []string{"-o", "json", "asset", "list"}, nil)
	if code != 0 {
		t.Fatalf("want 0, got %d", code)
	}
	var items []map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &items); err != nil {
		t.Fatalf("json unmarshal: %v out=%q", err, out)
	}
	if len(items) != 1 || items[0]["path"] != "a" {
		t.Fatalf("unexpected items: %v", items)
	}
}

func TestAssetListYAML(t *testing.T) {
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/assets", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"items":[{"path":"a","resource_version":1,"spec":{"type":"cdu"}}],"next_page_token":""}`+"\n")
		})
	})
	code, out, _ := runWithServer(t, srv, []string{"-o", "yaml", "asset", "list"}, nil)
	if code != 0 {
		t.Fatalf("want 0, got %d", code)
	}
	if !strings.Contains(out, "path: a") {
		t.Fatalf("yaml missing key: %q", out)
	}
}

func TestAssetListRFC7807(t *testing.T) {
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/assets", func(w http.ResponseWriter, r *http.Request) {
			writeProblem(w, http.StatusInternalServerError, "bad-request", "store error", "disk full", r.URL.Path, "rid-007")
		})
	})
	code, _, errOut := runWithServer(t, srv, []string{"asset", "list"}, nil)
	if code != 1 {
		t.Fatalf("want 1, got %d stderr=%s", code, errOut)
	}
	if !strings.Contains(errOut, "store error") || !strings.Contains(errOut, "request_id=rid-007") {
		t.Fatalf("stderr=%q", errOut)
	}
}

func TestAssetListPagination(t *testing.T) {
	// 3 pages with next_page_token plumbing; verify items aggregate.
	var calls int
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/assets", func(w http.ResponseWriter, r *http.Request) {
			calls++
			pt := r.URL.Query().Get("page_token")
			w.Header().Set("Content-Type", "application/json")
			switch pt {
			case "":
				io.WriteString(w, `{"items":[{"path":"a","resource_version":1,"spec":{"type":"cdu"}}],"next_page_token":"T1"}`+"\n")
			case "T1":
				io.WriteString(w, `{"items":[{"path":"b","resource_version":1,"spec":{"type":"cdu"}}],"next_page_token":"T2"}`+"\n")
			case "T2":
				io.WriteString(w, `{"items":[{"path":"c","resource_version":1,"spec":{"type":"cdu"}}],"next_page_token":""}`+"\n")
			default:
				t.Errorf("unexpected page_token=%q", pt)
			}
		})
	})
	code, out, _ := runWithServer(t, srv, []string{"-o", "json", "asset", "list"}, nil)
	if code != 0 {
		t.Fatalf("want 0, got %d", code)
	}
	if calls != 3 {
		t.Fatalf("want 3 calls, got %d", calls)
	}
	if !strings.Contains(out, `"path":"a"`) || !strings.Contains(out, `"path":"b"`) || !strings.Contains(out, `"path":"c"`) {
		t.Fatalf("missing items in out=%s", out)
	}
}

// --- asset get -----------------------------------------------------------

func TestAssetGetSuccess(t *testing.T) {
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		registerDefaults(mux)
		mux.HandleFunc("/v1/assets/p1", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"path":"p1","resource_version":2,"spec":{"type":"cdu"},"created_at":"2026-06-12T10:00:00Z","updated_at":"2026-06-12T11:00:00Z"}`+"\n")
		})
	})
	defer srv.Close()
	code, out, errOut := runWithServer(t, srv, []string{"asset", "get", "p1"}, nil)
	if code != 0 {
		t.Fatalf("want 0, got %d stderr=%s", code, errOut)
	}
	if !strings.Contains(out, "PATH") || !strings.Contains(out, "TYPE") || !strings.Contains(out, "CREATED") || !strings.Contains(out, "UPDATED") {
		t.Fatalf("missing table header: %q", out)
	}
	if !strings.Contains(out, "p1") || !strings.Contains(out, "cdu") || !strings.Contains(out, "2026-06-12T10:00:00Z") {
		t.Fatalf("row missing: %q", out)
	}
}

func TestAssetGetRFC7807(t *testing.T) {
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		registerDefaults(mux)
		mux.HandleFunc("/v1/assets/nope", func(w http.ResponseWriter, r *http.Request) {
			writeProblem(w, http.StatusNotFound, "path-not-found", "asset path not found", "nope", r.URL.Path, "rid-get")
		})
	})
	defer srv.Close()
	code, _, errOut := runWithServer(t, srv, []string{"asset", "get", "nope"}, nil)
	if code != 1 {
		t.Fatalf("want 1, got %d", code)
	}
	if !strings.Contains(errOut, "asset path not found") || !strings.Contains(errOut, "request_id=rid-get") {
		t.Fatalf("stderr=%q", errOut)
	}
}

// --- query ---------------------------------------------------------------

func TestQueryTable(t *testing.T) {
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/points/site01.pod000.cdu000.fws.supply.temp", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"path":"site01.pod000.cdu000.fws.supply.temp","value":23.5,"unit":"celsius","ts":"2026-06-12T12:00:00Z","quality":"good"}`+"\n")
		})
	})
	code, out, errOut := runWithServer(t, srv, []string{"query", "site01.pod000.cdu000.fws.supply.temp"}, nil)
	if code != 0 {
		t.Fatalf("want 0, got %d stderr=%s", code, errOut)
	}
	want := "23.5 celsius (good, 2026-06-12T12:00:00Z)\n"
	if out != want {
		t.Fatalf("want %q got %q", want, out)
	}
}

func TestQueryTableNoUnit(t *testing.T) {
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/points/p", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"path":"p","value":42,"unit":"","ts":"2026-06-12T12:00:00Z","quality":"good"}`+"\n")
		})
	})
	code, out, _ := runWithServer(t, srv, []string{"query", "p"}, nil)
	if code != 0 {
		t.Fatalf("want 0, got %d", code)
	}
	if strings.Contains(out, "  ") {
		t.Fatalf("double space when unit empty: %q", out)
	}
	if !strings.HasPrefix(out, "42 ") {
		t.Fatalf("missing value: %q", out)
	}
}

func TestQueryJSON(t *testing.T) {
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/points/p", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"path":"p","value":23.5,"unit":"celsius","ts":"2026-06-12T12:00:00Z","quality":"good"}`+"\n")
		})
	})
	code, out, _ := runWithServer(t, srv, []string{"-o", "json", "query", "p"}, nil)
	if code != 0 {
		t.Fatalf("want 0, got %d", code)
	}
	var v map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &v); err != nil {
		t.Fatalf("json: %v out=%q", err, out)
	}
	if v["unit"] != "celsius" {
		t.Fatalf("want unit=celsius, got %v", v["unit"])
	}
}

func TestQueryRFC7807(t *testing.T) {
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/points/nope", func(w http.ResponseWriter, r *http.Request) {
			writeProblem(w, http.StatusNotFound, "path-not-found", "asset path not found", "nope", r.URL.Path, "rid-q")
		})
	})
	code, _, errOut := runWithServer(t, srv, []string{"query", "nope"}, nil)
	if code != 1 {
		t.Fatalf("want 1, got %d", code)
	}
	if !strings.Contains(errOut, "asset path not found") {
		t.Fatalf("stderr=%q", errOut)
	}
}

// --- metric query --------------------------------------------------------

func TestMetricQueryEmpty(t *testing.T) {
	srv := newFakeCore(t)
	defer srv.Close()
	code, _, errOut := runWithServer(t, srv, []string{"metric", "query", "up"}, nil)
	if code != 0 {
		t.Fatalf("want 0, got %d", code)
	}
	if !strings.Contains(errOut, "no metric results") {
		t.Fatalf("stderr=%q", errOut)
	}
}

func TestMetricQueryTable(t *testing.T) {
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/metrics/query", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"status":"success","data":{"resultType":"vector","result":[{"metric":{"__name__":"cios_temp_celsius","site":"site01"},"value":[1749720000,"23.5"]}]}}`+"\n")
		})
	})
	code, out, _ := runWithServer(t, srv, []string{"metric", "query", `cios_temp_celsius{site="site01"}`}, nil)
	if code != 0 {
		t.Fatalf("want 0, got %d", code)
	}
	if !strings.Contains(out, "METRIC") || !strings.Contains(out, "LABELS") || !strings.Contains(out, "VALUE") || !strings.Contains(out, "TS") {
		t.Fatalf("header: %q", out)
	}
	if !strings.Contains(out, "cios_temp_celsius") || !strings.Contains(out, `site="site01"`) || !strings.Contains(out, "23.5") {
		t.Fatalf("row content: %q", out)
	}
}

func TestMetricQueryNoLabels(t *testing.T) {
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/metrics/query", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"status":"success","data":{"resultType":"vector","result":[{"metric":{"__name__":"up"},"value":[1749720000,"1"]}]}}`+"\n")
		})
	})
	code, out, _ := runWithServer(t, srv, []string{"metric", "query", "up"}, nil)
	if code != 0 {
		t.Fatalf("want 0, got %d", code)
	}
	if strings.Contains(out, "-  ") {
		t.Fatalf("placeholder dash present: %q", out)
	}
	if !strings.Contains(out, "up") || !strings.Contains(out, "1") {
		t.Fatalf("missing empty-label row: %q", out)
	}
}

func TestMetricQueryUnnamed(t *testing.T) {
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/metrics/query", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1749720000,"1"]}]}}`+"\n")
		})
	})
	code, out, _ := runWithServer(t, srv, []string{"metric", "query", "x"}, nil)
	if code != 0 {
		t.Fatalf("want 0, got %d", code)
	}
	if !strings.Contains(out, "<unnamed>") {
		t.Fatalf("missing <unnamed>: %q", out)
	}
}

func TestMetricQueryRFC7807(t *testing.T) {
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/metrics/query", func(w http.ResponseWriter, r *http.Request) {
			writeProblem(w, http.StatusBadGateway, "upstream-unavailable", "victoria metrics unavailable", "dial tcp", r.URL.Path, "rid-m")
		})
	})
	code, _, errOut := runWithServer(t, srv, []string{"metric", "query", "up"}, nil)
	if code != 1 {
		t.Fatalf("want 1, got %d", code)
	}
	if !strings.Contains(errOut, "victoria metrics unavailable") {
		t.Fatalf("stderr=%q", errOut)
	}
}

// --- alarm list ----------------------------------------------------------

func TestAlarmListEmpty(t *testing.T) {
	srv := newFakeCore(t)
	defer srv.Close()
	code, _, errOut := runWithServer(t, srv, []string{"alarm", "list"}, nil)
	if code != 0 {
		t.Fatalf("want 0, got %d", code)
	}
	if !strings.Contains(errOut, "no alarms") {
		t.Fatalf("stderr=%q", errOut)
	}
}

func TestAlarmListTable(t *testing.T) {
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/alarms", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"items":[{"id":"a1","path":"site01.pod000.cdu000","severity":"critical","state":"firing","summary":"inlet temp high","since":"2026-06-12T11:00:00Z"}],"next_page_token":""}`+"\n")
		})
	})
	code, out, _ := runWithServer(t, srv, []string{"-o", "table", "alarm", "list"}, nil)
	if code != 0 {
		t.Fatalf("want 0, got %d", code)
	}
	want := "SEVERITY\tSTATE\tPATH\tSINCE\tSUMMARY\ncritical\tfiring\tsite01.pod000.cdu000\t2026-06-12T11:00:00Z\tinlet temp high\n"
	for _, s := range []string{"SEVERITY", "STATE", "PATH", "SINCE", "SUMMARY", "critical", "firing", "site01.pod000.cdu000", "2026-06-12T11:00:00Z", "inlet temp high"} {
		if !strings.Contains(out, s) {
			t.Fatalf("alarm table missing token %q in:\n%s", s, out)
		}
	}
	_ = want
}

// --- local input errors --------------------------------------------------

func TestUnknownCommand(t *testing.T) {
	srv := newFakeCore(t)
	defer srv.Close()
	code, _, errOut := runWithServer(t, srv, []string{"frobnicate"}, nil)
	if code != 2 {
		t.Fatalf("want 2, got %d stderr=%s", code, errOut)
	}
}

func TestMissingServerEnvFallback(t *testing.T) {
	// No -s flag, no CIOS_SERVER would default to 127.0.0.1:8080, which is
	// a flake source when something else holds that port. Pin the env to a
	// guaranteed-closed loopback address (port 1 = tcpmux, never bound) so
	// the test exercises "server unreachable → exit 1" deterministically.
	t.Setenv("CIOS_SERVER", "http://127.0.0.1:1")
	code, _, _ := runWithServer(t, nil, []string{"asset", "list"}, nil)
	if code != 1 {
		t.Fatalf("want 1 (network err), got %d", code)
	}
}

// silence unused import in case the suite gets trimmed.
var _ = time.Second
