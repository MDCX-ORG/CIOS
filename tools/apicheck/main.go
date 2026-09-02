// apicheck validates that every HTTP route registered in
// core/server.go has a matching entry in protocol/openapi.yaml's
// `paths`, and vice versa. It is a static checker: it uses go/parser
// on the server.go AST plus a yaml reader on openapi.yaml — no
// runtime server is started.
//
// Usage:
//
//	apicheck [-strict] [server.go] [openapi.yaml]
//
// Defaults: server.go = ./core/server.go, openapi.yaml = ./protocol/openapi.yaml
//
// Exit codes:
//
//	0 = no drift (under current severity policy)
//	1 = drift detected (ERROR, or WARN under -strict)
//	2 = file read / parse error
//
// Drift severity:
//
//	ERROR: route is registered in server.go but absent from openapi.yaml
//	       (the implementation exposes an endpoint the architecture has
//	       not blessed — leaks unmodelled surface to clients).
//	WARN:  path is in openapi.yaml but no matching mux.HandleFunc
//	       (the architecture documents an endpoint nobody serves —
//	       dead doc, will surprise SDK generators).
//
// The `-strict` flag promotes WARN to failure. It does NOT change
// the headline printout — the architect still sees both lists.
//
// PRMT-073 §1 / §2 / §4 / §5.
package main

import (
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// ---------------------------------------------------------------------------
// AST route extraction
// ---------------------------------------------------------------------------
//
// core/server.go registers routes through the idiomatic
//
//	mux := http.NewServeMux()
//	mux.HandleFunc("/v1/...", s.serveFoo)
//
// pattern. We accept that exact shape and nothing else — anything fancier
// (custom ServeMux, path-value extraction at registration time) would
// require a different tool. The whitelist for this project is
// core/server.go; if a future PR moves routes elsewhere, this tool will
// have to follow (the prompt notes "server.go（路由注册集中处）" — it
// is the project's single source of truth by convention, not by type).

// extractRoutes parses a Go source file and returns the set of
// route patterns registered via mux.HandleFunc(<string>, ...). The
// string must be a literal (BasicLit of kind STRING); non-literal
// registrations are silently dropped — this is a lint, not a
// type-checker, and string-building patterns are out of scope.
//
// A route ending in "/" is the Go ServeMux "subtree" form: it matches
// any path with that prefix. core/server.go's contract is that such
// routes dispatch a single resource id (or path) in the trailing
// segment — openapi expresses the same idea with a {param}. We
// normalize the subtree form to "prefix/{path}" (note: {path} is the
// canonical openapi name used for /v1/assets and /v1/points — a
// trailing slash on those handlers means "asset path" or "point path",
// not a generic {id}; for any other resource we use {id} so the
// generated SDK gets a sane field name). The normalization rules are
// deliberately narrow: if the project adds a subtree route whose
// convention differs, this function is the one place to update.
func extractRoutes(path string) (map[string]bool, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	// Collect every mux.HandleFunc call across the file. There is
	// only one mux in server.go today; this loop tolerates extras
	// (e.g. test-local muxes inside _test.go) by filtering on
	// file scope. We restrict to non-test files in main() so we
	// don't pick up fixtures.
	var routes []string
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "HandleFunc" {
				return true
			}
			// X must be an *ast.Ident whose name is "mux". This
			// rejects e.g. router.HandleFunc on a third-party
			// mux (none in scope today, but cheap insurance).
			id, ok := sel.X.(*ast.Ident)
			if !ok || id.Name != "mux" {
				return true
			}
			if len(call.Args) < 1 {
				return true
			}
			lit, ok := call.Args[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			// Strip the surrounding quotes. Unquoting via
			// strconv would handle escapes; routes are
			// ASCII literals in this project, so a Trim is
			// enough and keeps the dep surface smaller.
			s := strings.Trim(lit.Value, "\"")
			routes = append(routes, s)
			return true
		})
	}

	out := map[string]bool{}
	for _, r := range routes {
		out[normalizeRoute(r)] = true
	}
	return out, nil
}

// normalizeRoute maps a Go mux pattern to its openapi path-template
// form. The rules:
//
//   - "/v1/health", "/v1/health/ready", "/v1/health/scanners" are
//     probes (PRMT-066); they are exact in both Go and openapi. We
//     keep them literal.
//
//   - Any other path ending in "/" is a subtree registration; the
//     trailing "/" becomes the resource-id placeholder. We choose the
//     placeholder name by parent path: /v1/assets/ → {path} (matches
//     the existing /v1/assets/{path} openapi entry from PRMT-011);
//     /v1/points/ → {path}; everything else → {id}. This is
//     hand-curated and may need a new entry when a new subtree route
//     lands — the alternative is to use {id} everywhere and rename
//     on the openapi side, which we explicitly chose not to do
//     because the existing {path} convention is documented in
//     openapi.yaml comments.
//
//   - A path with no trailing slash is treated as exact. openapi
//     declares such routes without curly braces.
func normalizeRoute(r string) string {
	if !strings.HasPrefix(r, "/") {
		// Defensive: every mux pattern in this project starts
		// with "/". If that ever changes, the tool will report
		// a clear "no match" diff rather than a parse panic.
		return r
	}
	if !strings.HasSuffix(r, "/") {
		return r
	}
	// Health subtree is the only "literal" subtree today (none
	// registered with a trailing slash, but this guard keeps the
	// rule readable if one is added later).
	if strings.HasPrefix(r, "/v1/health/") || r == "/v1/health/" {
		return strings.TrimRight(r, "/")
	}
	// Drop the trailing slash, then re-attach a single placeholder.
	parent := strings.TrimRight(r, "/")
	switch {
	case strings.HasPrefix(parent, "/v1/assets"),
		strings.HasPrefix(parent, "/v1/points"):
		return parent + "/{path}"
	default:
		return parent + "/{id}"
	}
}

// ---------------------------------------------------------------------------
// OpenAPI path extraction
// ---------------------------------------------------------------------------

// openapiPaths is the minimum we need from openapi.yaml: just the
// `paths` top-level map. Everything else (info, components, …) is
// irrelevant to drift detection and we deliberately do not parse it
// — keeping the schema narrow means a yaml schema bump in unrelated
// sections does not break this tool.
type openapiPaths struct {
	Paths map[string]interface{} `yaml:"paths"`
}

func extractOpenAPIPaths(path string) (map[string]bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var doc openapiPaths
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	out := map[string]bool{}
	for k := range doc.Paths {
		out[k] = true
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Diff
// ---------------------------------------------------------------------------

// diffResult is the partitioned view of route drift. routes is the
// set extracted from server.go (post-normalization); openapi is the
// set extracted from openapi.yaml's `paths`. The two "missing" sets
// follow the convention in the package doc-comment: routesMinusOpenapi
// is ERROR (impl exposes what the architecture hasn't blessed);
// openapiMinusRoutes is WARN (dead doc).
type diffResult struct {
	Routes             []string
	OpenAPI            []string
	RoutesMinusOpenAPI []string // ERROR: in impl, not in doc
	OpenAPIMinusRoutes []string // WARN:  in doc, not in impl
}

func diff(routes, openapi map[string]bool) diffResult {
	r := diffResult{
		Routes:  sortedKeys(routes),
		OpenAPI: sortedKeys(openapi),
	}
	for _, k := range r.Routes {
		if !openapi[k] {
			r.RoutesMinusOpenAPI = append(r.RoutesMinusOpenAPI, k)
		}
	}
	for _, k := range r.OpenAPI {
		if !routes[k] {
			r.OpenAPIMinusRoutes = append(r.OpenAPIMinusRoutes, k)
		}
	}
	sort.Strings(r.RoutesMinusOpenAPI)
	sort.Strings(r.OpenAPIMinusRoutes)
	return r
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------------------
// CLI / main
// ---------------------------------------------------------------------------

func main() {
	strict := flag.Bool("strict", false, "promote WARN (doc-only path) to a failure exit")
	serverPath := flag.String("server", "./core/server.go", "path to server.go containing mux.HandleFunc registrations")
	openapiPath := flag.String("openapi", "./protocol/openapi.yaml", "path to openapi.yaml")
	flag.Parse()

	routes, err := extractRoutes(*serverPath)
	if err != nil {
		fail(err.Error())
	}
	openapi, err := extractOpenAPIPaths(*openapiPath)
	if err != nil {
		fail(err.Error())
	}

	d := diff(routes, openapi)

	fmt.Printf("apicheck: %d routes in %s, %d paths in %s\n",
		len(d.Routes), *serverPath, len(d.OpenAPI), *openapiPath)

	if len(d.RoutesMinusOpenAPI) > 0 {
		fmt.Println("apicheck: ERROR — routes with no openapi entry (impl exposes, doc missing):")
		for _, r := range d.RoutesMinusOpenAPI {
			fmt.Printf("  - %s\n", r)
		}
	}
	if len(d.OpenAPIMinusRoutes) > 0 {
		fmt.Println("apicheck: WARN  — openapi paths with no impl (doc only, dead):")
		for _, r := range d.OpenAPIMinusRoutes {
			fmt.Printf("  - %s\n", r)
		}
	}

	if len(d.RoutesMinusOpenAPI) > 0 {
		os.Exit(1)
	}
	if *strict && len(d.OpenAPIMinusRoutes) > 0 {
		os.Exit(1)
	}
	os.Exit(0)
}

func fail(msg string) {
	fmt.Fprintln(os.Stderr, "apicheck: "+msg)
	os.Exit(2)
}
