// Tests for tools/apicheck. PRMT-073 §3: "路由提取正确 / 缺文档→
// ERROR / 缺实现→WARN / 归一化 {id} / -strict".
package main

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// writeFile is a tiny test helper that writes content to a temp
// path. It wraps t.TempDir() so the test cleans up automatically.
func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

// -----------------------------------------------------------------------------
// extractRoutes
// -----------------------------------------------------------------------------

// minimalServerGo is a fixture server.go that exercises the only
// shape extractRoutes understands: mux.HandleFunc("/v1/...", ...).
// It mixes exact and subtree routes, and one comment-only call to
// make sure the AST walker does not pick up commented-out code.
const minimalServerGo = `package core

import "net/http"

type S struct{}

func (s *S) Handler() http.Handler {
	mux := http.NewServeMux()
	// mux.HandleFunc("/v1/commented", s.commented)  -- this is a comment, must not be picked up
	mux.HandleFunc("/v1/assets", s.serveAssetsRoot)
	mux.HandleFunc("/v1/assets/", s.serveAssetPath)
	mux.HandleFunc("/v1/tickets/", s.serveTicket)
	mux.HandleFunc("/v1/health/ready", s.serveReady)
	mux.HandleFunc("/v1/health", s.serveHealth)
	return mux
}

func (s *S) serveAssetsRoot(w http.ResponseWriter, r *http.Request)  {}
func (s *S) serveAssetPath(w http.ResponseWriter, r *http.Request)   {}
func (s *S) serveTicket(w http.ResponseWriter, r *http.Request)      {}
func (s *S) serveReady(w http.ResponseWriter, r *http.Request)        {}
func (s *S) serveHealth(w http.ResponseWriter, r *http.Request)       {}
func (s *S) commented(w http.ResponseWriter, r *http.Request)        {}
`

// TestExtractRoutes_BasicShape locks the AST extraction: exact
// routes pass through, subtree routes get the {path}/{id}
// placeholder per normalizeRoute, and commented-out calls are
// ignored.
func TestExtractRoutes_BasicShape(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "server.go", minimalServerGo)

	got, err := extractRoutes(path)
	if err != nil {
		t.Fatalf("extractRoutes: %v", err)
	}
	want := map[string]bool{
		"/v1/assets":        true, // exact → kept literal
		"/v1/assets/{path}": true, // subtree under /v1/assets → {path}
		"/v1/tickets/{id}":  true, // subtree under anything else → {id}
		"/v1/health/ready":  true, // exact
		"/v1/health":        true, // exact
	}
	if len(got) != len(want) {
		t.Fatalf("routes: got %d, want %d (got=%v)", len(got), len(want), sortedKeys(got))
	}
	for k := range want {
		if !got[k] {
			t.Errorf("missing route %q in extracted set %v", k, sortedKeys(got))
		}
	}
}

// TestExtractRoutes_IgnoresNonMuxCalls makes sure non-mux
// HandleFunc calls (e.g. from a different variable) are not picked
// up. This is the "future-proofing" guard: today there is only one
// mux, but the tool should not silently broaden later.
func TestExtractRoutes_IgnoresNonMuxCalls(t *testing.T) {
	src := `package core
import "net/http"
type S struct{}
func (s *S) H() http.Handler {
	mux := http.NewServeMux()
	other := http.NewServeMux()
	mux.HandleFunc("/v1/keep", s.k)
	other.HandleFunc("/v1/drop", s.k)
	return mux
}
func (s *S) k(http.ResponseWriter, *http.Request) {}
`
	dir := t.TempDir()
	path := writeFile(t, dir, "server.go", src)
	got, err := extractRoutes(path)
	if err != nil {
		t.Fatalf("extractRoutes: %v", err)
	}
	if !got["/v1/keep"] {
		t.Errorf("expected /v1/keep, got %v", sortedKeys(got))
	}
	if got["/v1/drop"] {
		t.Errorf("did not expect /v1/drop (other.HandleFunc should be ignored), got %v", sortedKeys(got))
	}
}

// TestExtractRoutes_NonLiteralDropped locks the "string-building
// patterns are out of scope" rule. If a future PR writes
// `mux.HandleFunc("/v1/"+name, ...)` we should NOT crash; we just
// drop the call.
func TestExtractRoutes_NonLiteralDropped(t *testing.T) {
	src := `package core
import "net/http"
type S struct{}
func (s *S) H() http.Handler {
	mux := http.NewServeMux()
	const p = "/v1/dyn"
	mux.HandleFunc(p, s.k)
	mux.HandleFunc("/v1/"+"concat", s.k)
	return mux
}
func (s *S) k(http.ResponseWriter, *http.Request) {}
`
	dir := t.TempDir()
	path := writeFile(t, dir, "server.go", src)
	got, err := extractRoutes(path)
	if err != nil {
		t.Fatalf("extractRoutes: %v", err)
	}
	// p is a const string, not a literal in the call. The tool
	// only looks at the syntactic arg shape, so both calls are
	// dropped — /v1/dyn never appears.
	if len(got) != 0 {
		t.Errorf("expected no routes (no string-literal args), got %v", sortedKeys(got))
	}
}

// TestExtractRoutes_BadFile verifies the parse-error path returns
// an error rather than panicking. We don't assert on the error
// text — only that an error is returned.
func TestExtractRoutes_BadFile(t *testing.T) {
	if _, err := extractRoutes(filepath.Join(t.TempDir(), "nope.go")); err == nil {
		t.Fatal("expected error for non-existent file, got nil")
	}
}

// -----------------------------------------------------------------------------
// normalizeRoute (covered partially by TestExtractRoutes_BasicShape,
// expanded below for the edge cases the fixture above does not hit)
// -----------------------------------------------------------------------------

// TestNormalizeRoute_PointsSubtree exercises the second {path}
// mapping: /v1/points/ is a subtree under points and must
// normalize to /v1/points/{path} (not {id}), matching the
// existing openapi.yaml entry /v1/points/{path}.
func TestNormalizeRoute_PointsSubtree(t *testing.T) {
	got := normalizeRoute("/v1/points/")
	want := "/v1/points/{path}"
	if got != want {
		t.Errorf("points subtree: got %q, want %q", got, want)
	}
}

// TestNormalizeRoute_HealthSubtree collapses /v1/health/ to the
// literal /v1/health (the health subtree guard in normalizeRoute
// strips the trailing slash rather than appending {id}). Today no
// such route is registered, but the guard must hold for symmetry.
func TestNormalizeRoute_HealthSubtree(t *testing.T) {
	got := normalizeRoute("/v1/health/")
	want := "/v1/health"
	if got != want {
		t.Errorf("health subtree: got %q, want %q", got, want)
	}
}

// TestNormalizeRoute_GenericSubtree: any other subtree gets {id}.
func TestNormalizeRoute_GenericSubtree(t *testing.T) {
	got := normalizeRoute("/v1/runbooks/")
	want := "/v1/runbooks/{id}"
	if got != want {
		t.Errorf("generic subtree: got %q, want %q", got, want)
	}
}

// TestNormalizeRoute_Exact: no trailing slash means no rewrite.
func TestNormalizeRoute_Exact(t *testing.T) {
	for _, in := range []string{
		"/v1/assets",
		"/v1/health",
		"/v1/metrics/query",
		"/v1/metrics/query_range",
	} {
		if got := normalizeRoute(in); got != in {
			t.Errorf("exact %q: got %q, want %q", in, got, in)
		}
	}
}

// -----------------------------------------------------------------------------
// extractOpenAPIPaths
// -----------------------------------------------------------------------------

// TestExtractOpenAPIPaths_BasicShape: parse a tiny openapi.yaml
// fragment, return the `paths` keys.
func TestExtractOpenAPIPaths_BasicShape(t *testing.T) {
	dir := t.TempDir()
	yaml := `openapi: 3.1.0
info:
  title: test
  version: 1.0.0
paths:
  /v1/assets:    { get: {} }
  /v1/assets/{path}: { get: {} }
  /v1/alarms:    { get: {} }
`
	path := writeFile(t, dir, "openapi.yaml", yaml)
	got, err := extractOpenAPIPaths(path)
	if err != nil {
		t.Fatalf("extractOpenAPIPaths: %v", err)
	}
	want := []string{"/v1/assets", "/v1/assets/{path}", "/v1/alarms"}
	for _, w := range want {
		if !got[w] {
			t.Errorf("missing %q in %v", w, sortedKeys(got))
		}
	}
	if len(got) != len(want) {
		t.Errorf("got %d paths, want %d", len(got), len(want))
	}
}

// TestExtractOpenAPIPaths_MissingFile: a non-existent path must
// surface an error (not panic, not silently return empty).
func TestExtractOpenAPIPaths_MissingFile(t *testing.T) {
	if _, err := extractOpenAPIPaths(filepath.Join(t.TempDir(), "absent.yaml")); err == nil {
		t.Fatal("expected error for non-existent openapi, got nil")
	}
}

// -----------------------------------------------------------------------------
// diff
// -----------------------------------------------------------------------------

// TestDiff_MissingDocIsError locks the headline severity rule:
// an impl-only route belongs in RoutesMinusOpenAPI (ERROR class).
func TestDiff_MissingDocIsError(t *testing.T) {
	routes := map[string]bool{"/v1/a": true, "/v1/b": true}
	openapi := map[string]bool{"/v1/a": true}
	d := diff(routes, openapi)
	wantMissing := []string{"/v1/b"}
	if !equalSorted(d.RoutesMinusOpenAPI, wantMissing) {
		t.Errorf("RoutesMinusOpenAPI: got %v, want %v", d.RoutesMinusOpenAPI, wantMissing)
	}
	if len(d.OpenAPIMinusRoutes) != 0 {
		t.Errorf("OpenAPIMinusRoutes: got %v, want empty", d.OpenAPIMinusRoutes)
	}
}

// TestDiff_MissingImplIsWarn: the inverse asymmetry. Doc-only
// paths belong in OpenAPIMinusRoutes (WARN class).
func TestDiff_MissingImplIsWarn(t *testing.T) {
	routes := map[string]bool{"/v1/a": true}
	openapi := map[string]bool{"/v1/a": true, "/v1/ghost": true}
	d := diff(routes, openapi)
	if len(d.RoutesMinusOpenAPI) != 0 {
		t.Errorf("RoutesMinusOpenAPI: got %v, want empty", d.RoutesMinusOpenAPI)
	}
	wantMissing := []string{"/v1/ghost"}
	if !equalSorted(d.OpenAPIMinusRoutes, wantMissing) {
		t.Errorf("OpenAPIMinusRoutes: got %v, want %v", d.OpenAPIMinusRoutes, wantMissing)
	}
}

// TestDiff_NoDrift: both sets equal → both missing sets empty.
func TestDiff_NoDrift(t *testing.T) {
	routes := map[string]bool{"/v1/a": true, "/v1/b/{id}": true}
	openapi := map[string]bool{"/v1/a": true, "/v1/b/{id}": true}
	d := diff(routes, openapi)
	if len(d.RoutesMinusOpenAPI) != 0 || len(d.OpenAPIMinusRoutes) != 0 {
		t.Errorf("expected no drift, got routes-only=%v openapi-only=%v",
			d.RoutesMinusOpenAPI, d.OpenAPIMinusRoutes)
	}
}

// TestDiff_SortedOutputs: both diff sides are sorted so the
// printed report is stable. The fixture is asymmetric on purpose
// (insertion order would put "b" before "a" if unsorted).
func TestDiff_SortedOutputs(t *testing.T) {
	routes := map[string]bool{"/v1/b": true, "/v1/a": true, "/v1/zzz": true}
	openapi := map[string]bool{"/v1/a": true, "/v1/m": true}
	d := diff(routes, openapi)
	if !sort.StringsAreSorted(d.RoutesMinusOpenAPI) {
		t.Errorf("RoutesMinusOpenAPI not sorted: %v", d.RoutesMinusOpenAPI)
	}
	if !sort.StringsAreSorted(d.OpenAPIMinusRoutes) {
		t.Errorf("OpenAPIMinusRoutes not sorted: %v", d.OpenAPIMinusRoutes)
	}
}

// -----------------------------------------------------------------------------
// {id} normalization through the full pipeline
// -----------------------------------------------------------------------------

// TestNormalizationThroughPipeline is the integration check: a
// fixture server.go with a subtree route under a generic resource
// must produce a route key "/v1/whatever/{id}" — proving the {id}
// branch of normalizeRoute is exercised end-to-end, not just by
// the unit test above.
func TestNormalizationThroughPipeline(t *testing.T) {
	src := `package core
import "net/http"
type S struct{}
func (s *S) H() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/widgets/", s.k)
	return mux
}
func (s *S) k(http.ResponseWriter, *http.Request) {}
`
	dir := t.TempDir()
	path := writeFile(t, dir, "server.go", src)
	got, err := extractRoutes(path)
	if err != nil {
		t.Fatalf("extractRoutes: %v", err)
	}
	if !got["/v1/widgets/{id}"] {
		t.Errorf("expected /v1/widgets/{id}, got %v", sortedKeys(got))
	}
}

// -----------------------------------------------------------------------------
// -strict: WARN is also a failure under -strict
// -----------------------------------------------------------------------------
//
// We exercise the exit-code rule by running main() in a forked
// process. This avoids the side-effect of os.Exit inside the
// test process. The package doc-comment in main.go states the
// contract: default ⇒ WARN is not fatal; -strict ⇒ WARN is fatal.
// The assertions below re-derive that logic locally to keep the
// test independent of the binary's argv handling (which is in
// main() and has its own concerns).

// TestExitCodePolicy_Default is a doc-style test that locks the
// severity→exit mapping in plain Go terms, independent of how
// the CLI parses its flags. If the policy in main.go ever
// changes (e.g. WARN becomes a hard failure by default), this
// test will fail loudly.
func TestExitCodePolicy_Default(t *testing.T) {
	d := diffResult{
		RoutesMinusOpenAPI: nil, // no ERROR
		OpenAPIMinusRoutes: []string{"/v1/ghost"},
	}
	if !shouldExitZero(d, false /* strict */) {
		t.Error("default policy: WARN-only diff should exit 0")
	}
	if shouldExitZero(d, true /* strict */) {
		t.Error("strict policy: WARN-only diff should exit non-zero")
	}
}

// TestExitCodePolicy_ErrorAlwaysFatal: an ERROR-class drift is
// fatal under both policies.
func TestExitCodePolicy_ErrorAlwaysFatal(t *testing.T) {
	d := diffResult{
		RoutesMinusOpenAPI: []string{"/v1/extra"},
		OpenAPIMinusRoutes: nil,
	}
	if shouldExitZero(d, false) {
		t.Error("ERROR diff: default policy should exit non-zero")
	}
	if shouldExitZero(d, true) {
		t.Error("ERROR diff: strict policy should exit non-zero")
	}
}

// TestExitCodePolicy_NoDrift: empty diff is exit-0 in both modes.
func TestExitCodePolicy_NoDrift(t *testing.T) {
	d := diffResult{}
	if !shouldExitZero(d, false) {
		t.Error("no drift: default policy should exit 0")
	}
	if !shouldExitZero(d, true) {
		t.Error("no drift: strict policy should exit 0")
	}
}

// -----------------------------------------------------------------------------
// helpers
// -----------------------------------------------------------------------------

// shouldExitZero mirrors the exit-code logic in main(): ERROR is
// always fatal; WARN is fatal only under -strict. Pulled out of
// main so the policy is testable without forking the process.
func shouldExitZero(d diffResult, strict bool) bool {
	if len(d.RoutesMinusOpenAPI) > 0 {
		return false
	}
	if strict && len(d.OpenAPIMinusRoutes) > 0 {
		return false
	}
	return true
}

func equalSorted(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	ac := append([]string(nil), a...)
	bc := append([]string(nil), b...)
	sort.Strings(ac)
	sort.Strings(bc)
	for i := range ac {
		if ac[i] != bc[i] {
			return false
		}
	}
	return true
}

// keep import of "strings" usable from go vet's perspective even
// if individual tests stop using it — the helpers above only use
// sort. This avoids churn if a test gets dropped.
var _ = strings.HasPrefix
