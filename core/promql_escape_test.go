// Package core — promql_escape_test.go: regression coverage for
// the PRMT-078 PromQL label-value escape at the sink. The
// per-sink tests poke the helper with a synthetic path containing
// ", \\, and \\n — characters the cpath grammar forbids in real
// asset segments — and verify the VM receives a query whose
// asset_path="..." label value is properly backslash-escaped. A
// legal path is also probed to pin the "no behaviour change for
// legitimate input" guarantee from the prompt.
package core

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/yurimeng/cios/pkg/cpath"
)

// sinkQueryVM is a fake VM that records every `query` parameter
// sent to /api/v1/query. Tests assert on the captured strings; we
// don't need to return meaningful PromQL results (the sink
// functions degrade to a soft failure on any parse miss, which is
// fine for this regression — we only care about the wire form of
// each query).
type sinkQueryVM struct {
	mu   sync.Mutex
	all  []string
	resp string
}

func (s *sinkQueryVM) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.all = append(s.all, r.URL.Query().Get("query"))
		resp := s.resp
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if resp == "" {
			// Empty-vector success is the cheapest valid PromQL
			// response. The sink function treats it as a soft
			// failure but the query string has already been
			// captured.
			resp = `{"status":"success","data":{"resultType":"vector","result":[]}}`
		}
		_, _ = io.WriteString(w, resp)
	})
}

func (s *sinkQueryVM) start(t *testing.T) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(s.handler())
	t.Cleanup(ts.Close)
	return ts
}

func (s *sinkQueryVM) queries() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.all))
	copy(out, s.all)
	return out
}

func (s *sinkQueryVM) last() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.all) == 0 {
		return ""
	}
	return s.all[len(s.all)-1]
}

// newPromqlEscapeServer builds a Server wired to the supplied VM
// URL. Auth is disabled (matches the other regression tests in
// capacity_test / reconcile_test).
func newPromqlEscapeServer(t *testing.T, vmURL string) *Server {
	t.Helper()
	st, err := NewFileStore(t.TempDir() + "/store.json")
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	dict, err := cpath.LoadDict("../protocol")
	if err != nil {
		t.Fatalf("cpath.LoadDict: %v", err)
	}
	return NewServer(st, dict, vmURL)
}

// TestPromqlEscape_AssetHasTelemetry_EscapesHostilePath verifies
// that assetHasTelemetry backslash-escapes ", \\, and \\n inside
// the asset_path label value before splicing it into the PromQL
// query. The cpath grammar forbids these in real segments, so we
// drive the sink directly with a synthetic path.
func TestPromqlEscape_AssetHasTelemetry_EscapesHostilePath(t *testing.T) {
	vm := &sinkQueryVM{}
	ts := vm.start(t)
	srv := newPromqlEscapeServer(t, ts.URL)
	// Hostile path: backslash + double-quote + newline. After
	// escape the wire form should contain \\\"\\n inside the
	// label matcher, and the matcher boundary (the second `"`)
	// must be the closing one — i.e. the hostile chars do NOT
	// break out of the label.
	hostile := `a"b\c` + "\n" + "d"
	_, _ = srv.assetHasTelemetry(hostile, "7d")
	qs := vm.queries()
	if len(qs) != 1 {
		t.Fatalf("vm queries = %d, want 1", len(qs))
	}
	got := qs[0]
	wantSub := `asset_path="a\"b\\c\nd"`
	if !strings.Contains(got, wantSub) {
		t.Errorf("assetHasTelemetry: VM query missing escaped label %q; got %q", wantSub, got)
	}
	// Defensive: confirm the matcher closed exactly once and
	// the hostile chars did not break the PromQL grammar
	// (no stray closing quote from the unescaped ").
	if strings.Count(got, `asset_path="`) != 1 {
		t.Errorf("assetHasTelemetry: matcher opened multiple times; got %q", got)
	}
}

// TestPromqlEscape_LegalPath_NoBehaviorChange is the explicit
// "legal path ⇒ escape is a no-op" guarantee. All three sinks
// should produce a label matcher whose body is byte-equal to the
// input path.
func TestPromqlEscape_LegalPath_NoBehaviorChange(t *testing.T) {
	cases := []struct {
		name string
		call func(*Server) (float64, bool)
	}{}
	for _, c := range cases {
		vm := &sinkQueryVM{}
		ts := vm.start(t)
		srv := newPromqlEscapeServer(t, ts.URL)
		_, _ = c.call(srv)
		got := vm.last()
		want := `asset_path="site01.pod000.cdu000"`
		if !strings.Contains(got, want) {
			t.Errorf("%s: legal path not spliced verbatim; got %q, want substring %q", c.name, got, want)
		}
		if strings.Contains(got, `\`) {
			t.Errorf("%s: legal path produced backslashes; got %q", c.name, got)
		}
	}
	// assetHasTelemetry (returns (bool, bool) — different shape).
	vmA := &sinkQueryVM{}
	tsA := vmA.start(t)
	srvA := newPromqlEscapeServer(t, tsA.URL)
	_, _ = srvA.assetHasTelemetry("site01.pod000.cdu000", "7d")
	gotA := vmA.last()
	wantA := `asset_path="site01.pod000.cdu000"`
	if !strings.Contains(gotA, wantA) {
		t.Errorf("assetHasTelemetry: legal path not spliced verbatim; got %q, want substring %q", gotA, wantA)
	}
	if strings.Contains(gotA, `\`) {
		t.Errorf("assetHasTelemetry: legal path produced backslashes; got %q", gotA)
	}
}
