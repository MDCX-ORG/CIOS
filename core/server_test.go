package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yurimeng/cios/pkg/cpath"
)

// newTestServer builds a Server backed by an in-memory Store
// (disk-backed in t.TempDir) and a fake VM. The dict is loaded
// from the real protocol/ directory.
func newTestServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	return newTestServerWith(t, nil)
}

// newTestServerWith is the shared constructor for newTestServer and
// newTestServerAs. wrap, when non-nil, wraps srv.Handler() before it
// is served (e.g. principalHandler for fixed CI accounts).
func newTestServerWith(t *testing.T, wrap func(http.Handler) http.Handler) (*Server, *httptest.Server) {
	t.Helper()
	root := moduleRoot(t)
	dict, err := cpath.LoadDict(filepath.Join(root, "protocol"))
	if err != nil {
		t.Fatalf("LoadDict: %v", err)
	}
	st, err := NewFileStore(filepath.Join(t.TempDir(), "store.json"))
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	vm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Default: success vector with one sample.
		if r.URL.Query().Get("query") == "boom" {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"success","data":{"resultType":"vector","result":[{"metric":{"quality":"good"},"value":[1700000000,"23.5"]}]}}`)
	}))
	t.Cleanup(vm.Close)
	srv := NewServer(st, dict, vm.URL)
	h := http.Handler(srv.Handler())
	if wrap != nil {
		h = wrap(h)
	}
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return srv, ts
}

// moduleRoot walks up from the cwd until it finds go.mod.
func moduleRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	dir := wd
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("module root not found")
		}
		dir = parent
	}
}

// --- /v1/assets ---------------------------------------------------------

func TestAssets_PutAndGet(t *testing.T) {
	_, ts := newTestServer(t)
	body := `{"spec":{"type":"cdu"}}`
	resp := doReq(t, ts, http.MethodPut, "/v1/assets/site01.pod000.cdu000", body)
	if resp.code != http.StatusCreated {
		t.Fatalf("PUT: %d %s", resp.code, resp.body)
	}
	var a Asset
	mustJSON(t, resp.body, &a)
	if a.ResourceVersion != 1 {
		t.Errorf("version = %d, want 1", a.ResourceVersion)
	}
	resp = doReq(t, ts, http.MethodGet, "/v1/assets/site01.pod000.cdu000", "")
	if resp.code != http.StatusOK {
		t.Fatalf("GET: %d %s", resp.code, resp.body)
	}
}

func TestAssets_PutSpecTypeMismatch(t *testing.T) {
	_, ts := newTestServer(t)
	resp := doReq(t, ts, http.MethodPut, "/v1/assets/site01.pod000.cdu000",
		`{"spec":{"type":"pod"}}`)
	if resp.code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d %s", resp.code, resp.body)
	}
	mustProblem(t, resp.body, "bad-request")
}

func TestAssets_PutBadPath(t *testing.T) {
	_, ts := newTestServer(t)
	resp := doReq(t, ts, http.MethodPut, "/v1/assets/garbage..path",
		`{"spec":{"type":"cdu"}}`)
	if resp.code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d %s", resp.code, resp.body)
	}
	mustProblem(t, resp.body, "bad-path")
}

func TestAssets_GetMissing(t *testing.T) {
	_, ts := newTestServer(t)
	resp := doReq(t, ts, http.MethodGet, "/v1/assets/site01.pod000.cdu000", "")
	if resp.code != http.StatusNotFound {
		t.Fatalf("want 404, got %d %s", resp.code, resp.body)
	}
	mustProblem(t, resp.body, "path-not-found")
}

func TestAssets_ListFilterAndPage(t *testing.T) {
	_, ts := newTestServer(t)
	for _, p := range []string{
		"site01.pod000.cdu000",
		"site01.pod000.cdu001",
		"site01.pod001.cdu000",
		"site02.pod000.cdu000",
	} {
		resp := doReq(t, ts, http.MethodPut, "/v1/assets/"+p, `{"spec":{"type":"cdu"}}`)
		if resp.code/100 != 2 {
			t.Fatalf("put %s: %d %s", p, resp.code, resp.body)
		}
	}
	// filter=site01.** (cpath glob: "**" matches zero+ whole segments).
	resp := doReq(t, ts, http.MethodGet, "/v1/assets?filter=site01.**", "")
	if resp.code != http.StatusOK {
		t.Fatalf("list: %d", resp.code)
	}
	var lr listAssetsResponse
	mustJSON(t, resp.body, &lr)
	if len(lr.Items) != 3 {
		t.Errorf("filter len = %d, want 3", len(lr.Items))
	}
	// page_size=1 with filter → 3 pages
	got := []string{}
	next := ""
	for i := 0; i < 4; i++ {
		u := "/v1/assets?filter=site01.**&page_size=1"
		if next != "" {
			u += "&page_token=" + next
		}
		r := doReq(t, ts, http.MethodGet, u, "")
		if r.code != http.StatusOK {
			t.Fatalf("page %d: %d %s", i, r.code, r.body)
		}
		var pg listAssetsResponse
		mustJSON(t, r.body, &pg)
		for _, it := range pg.Items {
			got = append(got, it.Path)
		}
		next = pg.NextPageToken
		if next == "" {
			break
		}
	}
	if len(got) != 3 {
		t.Errorf("paged: %v", got)
	}
}

func TestAssets_DeleteCascadeAndBlock(t *testing.T) {
	_, ts := newTestServer(t)
	for _, p := range []string{
		"site01.pod000.cdu000",
		"site01.pod000.cdu001",
	} {
		doReq(t, ts, http.MethodPut, "/v1/assets/"+p, `{"spec":{"type":"cdu"}}`)
	}
	// Delete pod without cascade → 409.
	r := doReq(t, ts, http.MethodDelete, "/v1/assets/site01.pod000", "")
	if r.code != http.StatusConflict {
		t.Fatalf("no-cascade: want 409 got %d %s", r.code, r.body)
	}
	mustProblem(t, r.body, "conflict")
	// Cascade → 200.
	r = doReq(t, ts, http.MethodDelete, "/v1/assets/site01.pod000?cascade=true", "")
	if r.code != http.StatusOK {
		t.Fatalf("cascade: want 200 got %d %s", r.code, r.body)
	}
}

func TestAssets_PutOptimisticLock(t *testing.T) {
	_, ts := newTestServer(t)
	_ = doReq(t, ts, http.MethodPut, "/v1/assets/site01.pod000.cdu000", `{"spec":{"type":"cdu"}}`)
	// Stale version → 409.
	r := doReq(t, ts, http.MethodPut, "/v1/assets/site01.pod000.cdu000",
		`{"spec":{"type":"cdu"},"resource_version":99}`)
	if r.code != http.StatusConflict {
		t.Fatalf("want 409, got %d %s", r.code, r.body)
	}
	mustProblem(t, r.body, "conflict")
}

func TestAssets_PutDedupReplay(t *testing.T) {
	_, ts := newTestServer(t)
	body := `{"spec":{"type":"cdu"},"request_id":"01HABCDEFGHIJKLMN"}`
	r1 := doReq(t, ts, http.MethodPut, "/v1/assets/site01.pod000.cdu000", body)
	if r1.code/100 != 2 {
		t.Fatalf("first: %d %s", r1.code, r1.body)
	}
	r2 := doReq(t, ts, http.MethodPut, "/v1/assets/site01.pod000.cdu000", body)
	// Spec-004 §5 contract: dedup replay is byte-for-byte. Both
	// writeJSON and captureResponse now use json.Marshal + "\n"
	// so the bodies are identical.
	if r1.body != r2.body {
		t.Errorf("dedup body mismatch:\n  first:  %q\n  second: %q", r1.body, r2.body)
	}
	if r1.code != r2.code {
		t.Errorf("dedup status mismatch: %d vs %d", r1.code, r2.code)
	}
	// And the store saw exactly one Put (resource_version=1, not 2).
	r := doReq(t, ts, http.MethodGet, "/v1/assets/site01.pod000.cdu000", "")
	var a Asset
	mustJSON(t, r.body, &a)
	if a.ResourceVersion != 1 {
		t.Errorf("version after dedup = %d, want 1", a.ResourceVersion)
	}
}

// --- /v1/metrics --------------------------------------------------------

func TestMetrics_QueryPassThrough(t *testing.T) {
	_, ts := newTestServer(t)
	r := doReq(t, ts, http.MethodGet, "/v1/metrics/query?query=up&time=1700000000", "")
	if r.code != http.StatusOK {
		t.Fatalf("want 200, got %d %s", r.code, r.body)
	}
	// The fake VM returns a success vector; the body must come through verbatim.
	if !bytes.Contains([]byte(r.body), []byte(`"23.5"`)) {
		t.Errorf("body missing VM payload: %s", r.body)
	}
}

func TestMetrics_UpstreamPassThrough(t *testing.T) {
	// PRMT-083 §2: VM upstream 5xx no longer leaks the body via
	// pass-through. The body is captured into errUpstreamStatus
	// and the caller returns a scrubbed 502 upstream-unavailable
	// problem. The server-side log retains the upstream body for
	// operators (see TestProblem5xxScrub_VMUpstream).
	_, ts := newTestServer(t)
	r := doReq(t, ts, http.MethodGet, "/v1/metrics/query?query=boom", "")
	if r.code != http.StatusBadGateway {
		t.Errorf("upstream 500: got %d %s", r.code, r.body)
	}
	mustProblem(t, r.body, "upstream-unavailable")
}

func TestMetrics_MissingQuery(t *testing.T) {
	_, ts := newTestServer(t)
	r := doReq(t, ts, http.MethodGet, "/v1/metrics/query", "")
	if r.code != http.StatusBadRequest {
		t.Errorf("want 400, got %d", r.code)
	}
	mustProblem(t, r.body, "bad-request")
}

// --- /v1/points ---------------------------------------------------------

func TestPoints_GetSuccess(t *testing.T) {
	_, ts := newTestServer(t)
	r := doReq(t, ts, http.MethodGet, "/v1/points/site01.pod000.cdu000.fws.supply.temp", "")
	if r.code != http.StatusOK {
		t.Fatalf("want 200, got %d %s", r.code, r.body)
	}
	var pr pointResponse
	mustJSON(t, r.body, &pr)
	if pr.Value != 23.5 {
		t.Errorf("value = %v, want 23.5", pr.Value)
	}
	if pr.Quality != "good" {
		t.Errorf("quality = %q, want good", pr.Quality)
	}
}

func TestPoints_GetBadPath(t *testing.T) {
	_, ts := newTestServer(t)
	r := doReq(t, ts, http.MethodGet, "/v1/points/garbage..path.temp", "")
	if r.code != http.StatusBadRequest {
		t.Errorf("want 400, got %d", r.code)
	}
	mustProblem(t, r.body, "bad-path")
}

// --- /v1/alarms ---------------------------------------------------------

func TestAlarms_SeedAndList(t *testing.T) {
	srv, ts := newTestServer(t)
	_ = srv.st.SeedAlarms(context.Background(), []Alarm{
		{ID: "A1", Path: "site01.pod000.cdu000", Severity: "critical", State: "firing", Summary: "x"},
		{ID: "A2", Path: "site01.pod000.cdu001", Severity: "info", State: "firing", Summary: "y"},
	})
	r := doReq(t, ts, http.MethodGet, "/v1/alarms?severity=critical", "")
	if r.code != http.StatusOK {
		t.Fatalf("want 200, got %d %s", r.code, r.body)
	}
	var lr listAlarmsResponse
	mustJSON(t, r.body, &lr)
	if len(lr.Items) != 1 || lr.Items[0].ID != "A1" {
		t.Errorf("filter: %+v", lr.Items)
	}
}

func TestAlarms_BadSeverity(t *testing.T) {
	_, ts := newTestServer(t)
	r := doReq(t, ts, http.MethodGet, "/v1/alarms?severity=banana", "")
	if r.code != http.StatusBadRequest {
		t.Errorf("want 400, got %d", r.code)
	}
	mustProblem(t, r.body, "bad-request")
}

func TestAlarms_BadState(t *testing.T) {
	_, ts := newTestServer(t)
	r := doReq(t, ts, http.MethodGet, "/v1/alarms?state=banana", "")
	if r.code != http.StatusBadRequest {
		t.Errorf("want 400, got %d", r.code)
	}
}

func TestAlarms_PageTokenRoundTrip(t *testing.T) {
	srv, ts := newTestServer(t)
	// 3 alarms.
	for i := 0; i < 3; i++ {
		_ = srv.st.SeedAlarms(context.Background(), []Alarm{{
			ID: "A" + string(rune('1'+i)), Path: "site01.pod000",
			Severity: "critical", State: "firing", Summary: "x",
		}})
	}
	// page_size=2 → 2 pages
	all := []string{}
	next := ""
	for i := 0; i < 3; i++ {
		u := "/v1/alarms?page_size=2"
		if next != "" {
			u += "&page_token=" + next
		}
		r := doReq(t, ts, http.MethodGet, u, "")
		if r.code != http.StatusOK {
			t.Fatalf("page %d: %d %s", i, r.code, r.body)
		}
		var pg listAlarmsResponse
		mustJSON(t, r.body, &pg)
		for _, a := range pg.Items {
			all = append(all, a.ID)
		}
		next = pg.NextPageToken
		if next == "" {
			break
		}
	}
	if len(all) != 3 {
		t.Errorf("paged alarms: %v", all)
	}
}

func TestAlarms_FilterGlob(t *testing.T) {
	srv, ts := newTestServer(t)
	_ = srv.st.SeedAlarms(context.Background(), []Alarm{
		{ID: "A1", Path: "site01.pod000.cdu000", Severity: "critical", State: "firing"},
		{ID: "A2", Path: "site02.pod000.cdu000", Severity: "critical", State: "firing"},
	})
	r := doReq(t, ts, http.MethodGet, "/v1/alarms?filter=site01.**", "")
	if r.code != http.StatusOK {
		t.Fatalf("filter: %d %s", r.code, r.body)
	}
	var lr listAlarmsResponse
	mustJSON(t, r.body, &lr)
	if len(lr.Items) != 1 || lr.Items[0].ID != "A1" {
		t.Errorf("filter: %+v", lr.Items)
	}
}

func TestAlarms_BadPageToken(t *testing.T) {
	_, ts := newTestServer(t)
	r := doReq(t, ts, http.MethodGet, "/v1/alarms?page_token=not-base64", "")
	if r.code != http.StatusBadRequest {
		t.Errorf("want 400, got %d", r.code)
	}
}

func TestAlarms_BadFilter(t *testing.T) {
	_, ts := newTestServer(t)
	// "*" alone in a segment is a single star not matching the
	// grammar "[a-z0-9.*]+" — wait, "*" alone IS in the grammar
	// but CompileGlob treats it as a literal segment wildcard,
	// which is fine. Use an actually-bad pattern instead.
	r := doReq(t, ts, http.MethodGet, "/v1/alarms?filter=a..b", "")
	if r.code != http.StatusBadRequest {
		t.Errorf("want 400, got %d", r.code)
	}
}

func TestAssets_ListTypeFilter(t *testing.T) {
	_, ts := newTestServer(t)
	cases := []struct {
		path string
		typ  string
	}{
		{"site01.pod000.cdu000", "cdu"},
		{"site01.pod001.cdu000", "cdu"},
		{"site01.pod000.meter000", "meter"},
	}
	for _, c := range cases {
		doReq(t, ts, http.MethodPut, "/v1/assets/"+c.path, `{"spec":{"type":"`+c.typ+`"}}`)
	}
	r := doReq(t, ts, http.MethodGet, "/v1/assets?type=cdu", "")
	if r.code != http.StatusOK {
		t.Fatalf("type filter: %d", r.code)
	}
	var lr listAssetsResponse
	mustJSON(t, r.body, &lr)
	if len(lr.Items) != 2 {
		t.Errorf("cdu type filter len = %d, want 2", len(lr.Items))
	}
}

func TestAssets_ListBadPageToken(t *testing.T) {
	_, ts := newTestServer(t)
	r := doReq(t, ts, http.MethodGet, "/v1/assets?page_token=not-a-token", "")
	if r.code != http.StatusBadRequest {
		t.Errorf("want 400, got %d", r.code)
	}
}

func TestAssets_ListBadPageSize(t *testing.T) {
	_, ts := newTestServer(t)
	r := doReq(t, ts, http.MethodGet, "/v1/assets?page_size=-1", "")
	if r.code != http.StatusBadRequest {
		t.Errorf("want 400, got %d", r.code)
	}
	r = doReq(t, ts, http.MethodGet, "/v1/assets?page_size=9999", "")
	if r.code != http.StatusBadRequest {
		t.Errorf("want 400 for >1000, got %d", r.code)
	}
}

func TestAssets_ListBadOrderBy(t *testing.T) {
	_, ts := newTestServer(t)
	r := doReq(t, ts, http.MethodGet, "/v1/assets?order_by=id", "")
	if r.code != http.StatusBadRequest {
		t.Errorf("want 400, got %d", r.code)
	}
}

func TestAssets_PutBadJSON(t *testing.T) {
	_, ts := newTestServer(t)
	r := doReq(t, ts, http.MethodPut, "/v1/assets/site01.pod000.cdu000", `not json`)
	if r.code != http.StatusBadRequest {
		t.Errorf("want 400, got %d", r.code)
	}
}

func TestAssets_DeleteMissingPath(t *testing.T) {
	_, ts := newTestServer(t)
	r := doReq(t, ts, http.MethodDelete, "/v1/assets/site01.pod000.cdu000", "")
	if r.code != http.StatusNotFound {
		t.Errorf("want 404, got %d %s", r.code, r.body)
	}
}

func TestAssets_PutUpdatePath(t *testing.T) {
	// 200 on overwrite, 201 on create.
	_, ts := newTestServer(t)
	r1 := doReq(t, ts, http.MethodPut, "/v1/assets/site01.pod000.cdu000", `{"spec":{"type":"cdu"}}`)
	if r1.code != http.StatusCreated {
		t.Errorf("first: want 201, got %d", r1.code)
	}
	r2 := doReq(t, ts, http.MethodPut, "/v1/assets/site01.pod000.cdu000", `{"spec":{"type":"cdu"}}`)
	if r2.code != http.StatusOK {
		t.Errorf("second: want 200, got %d", r2.code)
	}
}

func TestPoints_UpstreamUnreachable(t *testing.T) {
	// Point a Server at a non-listening URL; fetchVM should fail
	// at the network level and the handler returns 502
	// upstream-unavailable.
	root := moduleRoot(t)
	dict, err := cpath.LoadDict(filepath.Join(root, "protocol"))
	if err != nil {
		t.Fatalf("LoadDict: %v", err)
	}
	st, err := NewFileStore(filepath.Join(t.TempDir(), "store.json"))
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	srv := NewServer(st, dict, "http://127.0.0.1:1") // dead port
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	r := doReq(t, ts, http.MethodGet, "/v1/points/site01.pod000.cdu000.fws.supply.temp", "")
	if r.code != http.StatusBadGateway {
		t.Errorf("want 502, got %d %s", r.code, r.body)
	}
	mustProblem(t, r.body, "upstream-unavailable")
}

func TestMetrics_UpstreamUnreachable(t *testing.T) {
	root := moduleRoot(t)
	dict, err := cpath.LoadDict(filepath.Join(root, "protocol"))
	if err != nil {
		t.Fatalf("LoadDict: %v", err)
	}
	st, err := NewFileStore(filepath.Join(t.TempDir(), "store.json"))
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	srv := NewServer(st, dict, "http://127.0.0.1:1")
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	r := doReq(t, ts, http.MethodGet, "/v1/metrics/query?query=up", "")
	if r.code != http.StatusBadGateway {
		t.Errorf("want 502, got %d %s", r.code, r.body)
	}
	mustProblem(t, r.body, "upstream-unavailable")
}

func TestPoints_BadVMResponse(t *testing.T) {
	// VM returns a malformed vector (not JSON): servePoint should
	// return 500 (parse error) — or 404 if status != "success".
	vm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"success","data":{"resultType":"vector","result":[]}}`)
	}))
	t.Cleanup(vm.Close)
	root := moduleRoot(t)
	dict, _ := cpath.LoadDict(filepath.Join(root, "protocol"))
	st, _ := NewFileStore(filepath.Join(t.TempDir(), "store.json"))
	srv := NewServer(st, dict, vm.URL)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	r := doReq(t, ts, http.MethodGet, "/v1/points/site01.pod000.cdu000.fws.supply.temp", "")
	if r.code != http.StatusNotFound {
		t.Errorf("empty VM result: want 404, got %d %s", r.code, r.body)
	}
}

func TestAssets_PutMethodNotAllowed(t *testing.T) {
	_, ts := newTestServer(t)
	r := doReq(t, ts, http.MethodPost, "/v1/assets/site01.pod000.cdu000", `{}`)
	if r.code != http.StatusMethodNotAllowed {
		t.Errorf("want 405, got %d", r.code)
	}
}

func TestAssets_GetParseError(t *testing.T) {
	_, ts := newTestServer(t)
	// Use a syntactically valid asset path shape but with a
	// non-existent intermediate type → cpath rejects.
	r := doReq(t, ts, http.MethodGet, "/v1/assets/site01.notatype000", "")
	if r.code != http.StatusBadRequest {
		t.Errorf("want 400, got %d", r.code)
	}
}

func TestRequestID_Passthrough(t *testing.T) {
	_, ts := newTestServer(t)
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/alarms", nil)
	req.Header.Set("X-Request-Id", "my-rid-123")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("X-Request-Id"); got != "my-rid-123" {
		t.Errorf("X-Request-Id = %q, want my-rid-123", got)
	}
}

func TestErrUpstreamStatus_ErrorString(t *testing.T) {
	e := errUpstreamStatus{status: 500, body: "boom"}
	if got := e.Error(); got != "vm status 500: boom" {
		t.Errorf("Error() = %q", got)
	}
}

func TestRequestIDFromContext_Empty(t *testing.T) {
	// Direct call without withRequestID wrapping → empty string.
	if got := RequestIDFromContext(context.Background()); got != "" {
		t.Errorf("empty ctx: got %q, want \"\"", got)
	}
}

func TestDedupTable_ExpiredEntry(t *testing.T) {
	// Inject a clock; an entry created at t=0 expires at t=ttl; a
	// lookup at t=ttl+1 returns nothing and removes the entry.
	now := time.Now()
	tbl := newDedupTable(time.Hour)
	tbl.now = func() time.Time { return now }
	tbl.remember("k", dedupEntry{status: 200, body: []byte("x")})
	// Advance past TTL.
	tbl.now = func() time.Time { return now.Add(2 * time.Hour) }
	if _, ok := tbl.lookup("k"); ok {
		t.Errorf("expired entry should not be found")
	}
	// After GC, the entry is gone.
	if _, ok := tbl.lookup("k"); ok {
		t.Errorf("expired entry should be removed on second lookup")
	}
}

func TestPoints_MalformedVMResponse(t *testing.T) {
	// VM returns non-JSON: servePoint should return 500 (parse fail).
	vm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "not json at all")
	}))
	t.Cleanup(vm.Close)
	root := moduleRoot(t)
	dict, _ := cpath.LoadDict(filepath.Join(root, "protocol"))
	st, _ := NewFileStore(filepath.Join(t.TempDir(), "store.json"))
	srv := NewServer(st, dict, vm.URL)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	r := doReq(t, ts, http.MethodGet, "/v1/points/site01.pod000.cdu000.fws.supply.temp", "")
	if r.code != http.StatusInternalServerError {
		t.Errorf("want 500 on bad VM JSON, got %d %s", r.code, r.body)
	}
}

func TestPoints_StatusNotSuccess(t *testing.T) {
	// VM returns status="error" → 404.
	vm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"status":"error","error":"nope"}`)
	}))
	t.Cleanup(vm.Close)
	root := moduleRoot(t)
	dict, _ := cpath.LoadDict(filepath.Join(root, "protocol"))
	st, _ := NewFileStore(filepath.Join(t.TempDir(), "store.json"))
	srv := NewServer(st, dict, vm.URL)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	r := doReq(t, ts, http.MethodGet, "/v1/points/site01.pod000.cdu000.fws.supply.temp", "")
	if r.code != http.StatusNotFound {
		t.Errorf("want 404 on VM error, got %d", r.code)
	}
}

func TestAlarms_BadPageTokenInt(t *testing.T) {
	// A syntactically-valid token that decodes to a non-int string
	// (after stripping the v1: prefix) is still rejected.
	_, ts := newTestServer(t)
	tok := encodePageToken("not-a-number")
	r := doReq(t, ts, http.MethodGet, "/v1/alarms?page_token="+tok, "")
	if r.code != http.StatusBadRequest {
		t.Errorf("want 400 for non-int token, got %d %s", r.code, r.body)
	}
}

// errStore is a minimal Store that fails on ListAlarms (and only
// on that call) so the alarms route's "store error" branch is
// covered. All other methods are unused / no-op.
type errStore struct {
	Store
	errListAlarms error
}

func (s *errStore) ListAlarms(_ context.Context) ([]Alarm, error) {
	return nil, s.errListAlarms
}

func TestAlarms_StoreError(t *testing.T) {
	root := moduleRoot(t)
	dict, _ := cpath.LoadDict(filepath.Join(root, "protocol"))
	srv := NewServer(&errStore{errListAlarms: errors.New("disk full")}, dict, "http://127.0.0.1:1")
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	r := doReq(t, ts, http.MethodGet, "/v1/alarms", "")
	if r.code != http.StatusInternalServerError {
		t.Errorf("want 500, got %d %s", r.code, r.body)
	}
}

// --- PRMT-083: 5xx scrub tests -----------------------------------------

// TestProblem5xxScrub_DetailAndLog verifies that writeInternalProblem
// hides the internal error text from the public problem detail while
// logging the full error server-side (truncated to maxInternalDetail).
// The public detail must reference the request_id so an operator can
// correlate the response to the server log line.
func TestProblem5xxScrub_DetailAndLog(t *testing.T) {
	var captured []string
	restore := SetInternalLogForTest(func(format string, args ...any) {
		captured = append(captured, fmt.Sprintf(format, args...))
	})
	defer restore()

	root := moduleRoot(t)
	dict, _ := cpath.LoadDict(filepath.Join(root, "protocol"))
	srv := NewServer(&errStore{errListAlarms: errors.New("disk full secret hostname=vm-7")}, dict, "http://127.0.0.1:1")
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	r := doReq(t, ts, http.MethodGet, "/v1/alarms", "")
	if r.code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d %s", r.code, r.body)
	}
	var p struct {
		Type      string `json:"type"`
		Title     string `json:"title"`
		Status    int    `json:"status"`
		Detail    string `json:"detail"`
		Instance  string `json:"instance"`
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal([]byte(r.body), &p); err != nil {
		t.Fatalf("problem decode: %v\nbody: %s", err, r.body)
	}
	// Public detail must NOT leak the internal error text.
	if strings.Contains(p.Detail, "disk full") || strings.Contains(p.Detail, "hostname") {
		t.Errorf("detail leaks internal error: %q", p.Detail)
	}
	// Public detail must reference the request_id for correlation.
	if !strings.Contains(p.Detail, p.RequestID) || p.RequestID == "" {
		t.Errorf("detail %q does not reference request_id %q", p.Detail, p.RequestID)
	}
	// Server-side log line must contain the full internal error.
	foundLog := false
	for _, line := range captured {
		if strings.Contains(line, "disk full secret hostname=vm-7") {
			foundLog = true
			break
		}
	}
	if !foundLog {
		t.Errorf("server log missing internal error; captured=%v", captured)
	}
}

// TestProblem5xxScrub_VMUpstream verifies the VM 502 path: detail
// must be generic, log must contain the upstream body. The fake VM
// returns 500 + a body; serveMetricsQuery maps that to 502 upstream-
// unavailable.
func TestProblem5xxScrub_VMUpstream(t *testing.T) {
	const upstreamBody = "upstream-secret-token=eyJabc"
	vm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, upstreamBody, http.StatusInternalServerError)
	}))
	t.Cleanup(vm.Close)

	var captured []string
	restore := SetInternalLogForTest(func(format string, args ...any) {
		captured = append(captured, fmt.Sprintf(format, args...))
	})
	defer restore()

	root := moduleRoot(t)
	dict, _ := cpath.LoadDict(filepath.Join(root, "protocol"))
	st, _ := NewFileStore(filepath.Join(t.TempDir(), "store.json"))
	srv := NewServer(st, dict, vm.URL)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	r := doReq(t, ts, http.MethodGet, "/v1/metrics/query?query=boom", "")
	if r.code != http.StatusBadGateway {
		t.Fatalf("want 502, got %d %s", r.code, r.body)
	}
	if strings.Contains(r.body, upstreamBody) {
		t.Errorf("public body leaks upstream body: %s", r.body)
	}
	if !strings.Contains(r.body, "internal error") {
		t.Errorf("public body missing scrub marker: %s", r.body)
	}
	foundLog := false
	for _, line := range captured {
		if strings.Contains(line, upstreamBody) {
			foundLog = true
			break
		}
	}
	if !foundLog {
		t.Errorf("server log missing upstream body; captured=%v", captured)
	}
}

// TestProblem5xxScrub_Truncate confirms that an internal detail
// longer than maxInternalDetail is truncated server-side but the
// public response still uses the generic marker (not the full err).
func TestProblem5xxScrub_Truncate(t *testing.T) {
	long := strings.Repeat("x", maxInternalDetail*2)

	var captured []string
	restore := SetInternalLogForTest(func(format string, args ...any) {
		captured = append(captured, fmt.Sprintf(format, args...))
	})
	defer restore()

	root := moduleRoot(t)
	dict, _ := cpath.LoadDict(filepath.Join(root, "protocol"))
	srv := NewServer(&errStore{errListAlarms: errors.New(long)}, dict, "http://127.0.0.1:1")
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	r := doReq(t, ts, http.MethodGet, "/v1/alarms", "")
	if r.code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d", r.code)
	}
	if strings.Contains(r.body, long) {
		t.Errorf("public body leaked long detail")
	}
	if len(captured) != 1 {
		t.Fatalf("expected one log line, got %d", len(captured))
	}
	if !strings.Contains(captured[0], "truncated") {
		t.Errorf("log line missing truncation marker: %s", captured[0])
	}
}

// TestProblem4xx_PreservesDetail confirms the 4xx path is NOT
// scrubbed — the caller-facing detail stays verbatim so the user
// can fix the request (DisallowUnknownFields / parse error, etc).
func TestProblem4xx_PreservesDetail(t *testing.T) {
	_, ts := newTestServer(t)
	r := doReq(t, ts, http.MethodPut, "/v1/assets/garbage..path",
		`{"spec":{"type":"cdu"}}`)
	if r.code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", r.code)
	}
	var p struct {
		Status    int    `json:"status"`
		Detail    string `json:"detail"`
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal([]byte(r.body), &p); err != nil {
		t.Fatalf("problem decode: %v", err)
	}
	if p.Status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", p.Status)
	}
	// Detail must still carry the actionable cause. bad-path detail
	// includes the cpath parse error text — verify a non-empty
	// detail that contains something path-related.
	if p.Detail == "" || strings.Contains(p.Detail, "internal error") {
		t.Errorf("4xx detail was scrubbed: %q", p.Detail)
	}
	if p.RequestID == "" {
		t.Errorf("4xx missing request_id: %s", r.body)
	}
}

// TestWriteProblem_4xxNotLogged confirms the 4xx path does NOT call
// internalLog (otherwise a malformed client request would generate
// per-request log noise).
func TestWriteProblem_4xxNotLogged(t *testing.T) {
	var captured []string
	restore := SetInternalLogForTest(func(format string, args ...any) {
		captured = append(captured, fmt.Sprintf(format, args...))
	})
	defer restore()

	_, ts := newTestServer(t)
	_ = doReq(t, ts, http.MethodGet, "/v1/alarms?severity=banana", "")
	if len(captured) != 0 {
		t.Errorf("4xx should not log to internalLog; got %v", captured)
	}
}

// --- helpers ------------------------------------------------------------

type httpResp struct {
	code int
	body string
}

func doReq(t *testing.T, ts *httptest.Server, method, path, body string) httpResp {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = bytes.NewReader([]byte(body))
	}
	req, err := http.NewRequest(method, ts.URL+path, rdr)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return httpResp{code: resp.StatusCode, body: string(b)}
}

func mustJSON(t *testing.T, body string, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(body), v); err != nil {
		t.Fatalf("unmarshal: %v\nbody: %s", err, body)
	}
}

func mustProblem(t *testing.T, body, wantTail string) {
	t.Helper()
	var p struct {
		Type      string `json:"type"`
		Title     string `json:"title"`
		Status    int    `json:"status"`
		Detail    string `json:"detail"`
		Instance  string `json:"instance"`
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal([]byte(body), &p); err != nil {
		t.Fatalf("not a problem: %v\nbody: %s", err, body)
	}
	if p.Type != "https://cios.dev/errors/"+wantTail {
		t.Errorf("type = %q, want tail %q", p.Type, wantTail)
	}
	if p.Status == 0 {
		t.Errorf("status missing in problem: %s", body)
	}
	if p.RequestID == "" {
		t.Errorf("request_id missing in problem: %s", body)
	}
}
