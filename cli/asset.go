// Package cli — asset.go: `cios asset list|get|export|import` against /v1/assets.
//
// list auto-paginates (hard cap = 100 pages, §4.5).
// get returns one row in the table format
//
//	PATH TYPE RV CREATED UPDATED
//
// export serializes assets (filtered by --prefix) to CSV or YAML.
// import upserts assets from a CSV or YAML file (per-row PUT).
package cli

import (
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/yurimeng/cios/pkg/reqid"
)

const maxListPages = 100

// defaultPageSize mirrors core.DefaultPageSize — kept as a local
// constant because the cli package does not import core (cli is a
// separate leaf binary). Values must stay in lockstep with
// core/pagination.go; PRMT-070 forbids unilateral renumbering.
const defaultPageSize = 100

// assetCmd dispatches to `asset list` or `asset get`.
func assetCmd(g globalFlags, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: cios asset <list|get> ...")
		return 2
	}
	switch args[0] {
	case "list":
		return assetListCmd(g, args[1:], stdout, stderr)
	case "get":
		return assetGetCmd(g, args[1:], stdout, stderr)
	case "export":
		return assetExportCmd(g, args[1:], stdout, stderr)
	case "import":
		return assetImportCmd(g, args[1:], stdin, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "error: unknown asset subcommand %q\n", args[0])
		return 2
	}
}

// assetRow is the table row shape — JSON tags match the server's
// Asset so json/yaml modes pass the server bytes through verbatim.
// Type is derived from Spec["type"] (the server does not emit a top-
// level "type" field; it lives in the spec blob).
type assetRow struct {
	Path            string         `json:"path"`
	Type            string         `json:"-"`
	ResourceVersion int64          `json:"resource_version"`
	CreatedAt       string         `json:"created_at"`
	UpdatedAt       string         `json:"updated_at"`
	Spec            map[string]any `json:"spec"`
}

func assetRowFrom(a assetRow) assetRow {
	if a.Spec != nil {
		if t, ok := a.Spec["type"].(string); ok {
			a.Type = t
		}
	}
	return a
}

func assetListCmd(g globalFlags, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("asset list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	typeFlag := fs.String("type", "", "filter by leaf type")
	lifecycle := fs.String("lifecycle", "", "filter by Spec.lifecycle (planned/installed/active/maintenance/retired)")
	prefix := fs.String("prefix", "", "filter by path prefix (e.g. sgp01.pod002)")
	limit := fs.Int("limit", 0, "page size cap (server default 100, max 1000); 0 → use --page-size")
	filter := fs.String("filter", "", "cpath glob")
	pageSize := fs.Int("page-size", defaultPageSize, "page size")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: cios asset list [--type T] [--lifecycle L] [--prefix P] [--limit N] [--filter G] [--page-size N]")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	c := NewClient(g.server)
	items, status, err := listAll[assetRow](c, "/v1/assets",
		buildAssetListQuery(*typeFlag, *lifecycle, *prefix, *limit, *filter, *pageSize))
	if err != nil {
		return writeTransportErr(err, stderr)
	}
	if status/100 != 2 {
		// Non-problem error responses: surface status+body.
		fmt.Fprintf(stderr, "error: http %d\n", status)
		return 1
	}
	rows := make([]any, 0, len(items))
	for i := range items {
		rows = append(rows, assetRowFrom(items[i]))
	}
	if g.output == "json" || g.output == "yaml" {
		// Server-side items array (no pagination envelope).
		if g.output == "json" {
			buf, _ := json.Marshal(items)
			fmt.Fprintf(stdout, "%s\n", string(buf))
		} else {
			if err := Print(stdout, g.output, items, TableSpec{}); err != nil {
				fmt.Fprintln(stderr, "error: "+err.Error())
				return 2
			}
		}
		return 0
	}
	if len(items) == 0 {
		fmt.Fprintln(stderr, "no assets")
		return 0
	}
	tbl := TableSpec{
		Columns: []string{"PATH", "TYPE", "RV", "UPDATED"},
		Row: func(v any) []string {
			a := v.(assetRow)
			return []string{
				a.Path,
				a.Type,
				strconv.FormatInt(a.ResourceVersion, 10),
				formatRFC3339String(a.UpdatedAt),
			}
		},
	}
	if err := Print(stdout, g.output, rows, tbl); err != nil {
		fmt.Fprintln(stderr, "error: "+err.Error())
		return 2
	}
	return 0
}

func buildAssetListQuery(typeF, lifecycle, prefix string, limit int, filter string, pageSize int) url.Values {
	q := url.Values{}
	if typeF != "" {
		q.Set("type", typeF)
	}
	if lifecycle != "" {
		q.Set("lifecycle", lifecycle)
	}
	if prefix != "" {
		q.Set("prefix", prefix)
	}
	if limit > 0 {
		// --limit takes precedence over --page-size on the server
		// (PRMT-067: limit wins when both are set).
		q.Set("limit", strconv.Itoa(limit))
	} else {
		q.Set("page_size", strconv.Itoa(pageSize))
	}
	if filter != "" {
		q.Set("filter", filter)
	}
	return q
}

func assetGetCmd(g globalFlags, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("asset get", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprintln(stderr, "usage: cios asset get <path>") }
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) != 1 {
		fmt.Fprintln(stderr, "usage: cios asset get <path>")
		return 2
	}
	path := rest[0]
	c := NewClient(g.server)
	status, body, err := c.Do("GET", "/v1/assets/"+path, nil, nil)
	if err != nil {
		return writeTransportErr(err, stderr)
	}
	if status/100 != 2 {
		// Fall back: status + body for non-problem errors.
		fmt.Fprintf(stderr, "error: http %d: %s\n", status, string(body))
		return 1
	}
	if g.output == "json" || g.output == "yaml" {
		// Pass raw server bytes through after re-parsing so we can
		// strip a trailing newline for round-trip consistency.
		// Simpler: just write the body verbatim (server emits \n).
		if _, err := stdout.Write(body); err != nil {
			return 1
		}
		return 0
	}
	var a assetRow
	if err := json.Unmarshal(body, &a); err != nil {
		fmt.Fprintf(stderr, "error: decode: %s\n", err.Error())
		return 1
	}
	a = assetRowFrom(a)
	tbl := TableSpec{
		Columns: []string{"PATH", "TYPE", "RV", "CREATED", "UPDATED"},
		Row: func(v any) []string {
			x := v.(assetRow)
			return []string{
				x.Path,
				x.Type,
				strconv.FormatInt(x.ResourceVersion, 10),
				formatRFC3339String(x.CreatedAt),
				formatRFC3339String(x.UpdatedAt),
			}
		},
	}
	if err := Print(stdout, g.output, []any{a}, tbl); err != nil {
		fmt.Fprintln(stderr, "error: "+err.Error())
		return 2
	}
	return 0
}

// listAll follows next_page_token until empty. Returns the aggregated
// items and the status of the LAST page (so callers can detect a
// non-2xx that arrived mid-loop without losing the error path).
// A pagination overflow (>100 pages) returns err != nil.
func listAll[T any](c *Client, path string, baseQuery url.Values) ([]T, int, error) {
	all := make([]T, 0)
	token := ""
	for page := 0; page < maxListPages; page++ {
		q := url.Values{}
		for k, vs := range baseQuery {
			for _, v := range vs {
				q.Add(k, v)
			}
		}
		if token != "" {
			q.Set("page_token", token)
		}
		status, body, err := c.Do("GET", path, q, nil)
		if err != nil {
			// RFC 7807 or transport error mid-loop: per §4.5, abort
			// and surface to caller.
			return nil, status, err
		}
		if status/100 != 2 {
			return nil, status, fmt.Errorf("http %d", status)
		}
		var page struct {
			Items         []T    `json:"items"`
			NextPageToken string `json:"next_page_token"`
		}
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, status, fmt.Errorf("decode: %s", err.Error())
		}
		all = append(all, page.Items...)
		if page.NextPageToken == "" {
			return all, status, nil
		}
		token = page.NextPageToken
	}
	// Exceeded the hard cap.
	return nil, 0, fmt.Errorf("pagination overflow (>100 pages)")
}

// --- export --------------------------------------------------------------

// assetExportCmd handles `cios asset export [--prefix P] [--format csv|yaml]`.
//
// Pulls from /v1/assets (re-using the listAll helper + existing query
// params), sorts by path for stable diffs, then serialises to stdout.
// CSV flattens the spec map as `spec.<key>` columns; YAML emits the
// raw asset rows (path / resource_version / spec / timestamps).
func assetExportCmd(g globalFlags, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("asset export", flag.ContinueOnError)
	fs.SetOutput(stderr)
	prefix := fs.String("prefix", "", "filter by path prefix (e.g. sgp01.pod002)")
	format := fs.String("format", "csv", "output format: csv|yaml")
	pageSize := fs.Int("page-size", defaultPageSize, "page size")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: cios asset export [--prefix P] [--format csv|yaml] [--page-size N]")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	switch *format {
	case "csv", "yaml":
	default:
		fmt.Fprintf(stderr, "error: --format must be csv or yaml (got %q)\n", *format)
		return 2
	}
	q := url.Values{}
	if *prefix != "" {
		q.Set("prefix", *prefix)
	}
	q.Set("page_size", strconv.Itoa(*pageSize))
	c := NewClient(g.server)
	items, status, err := listAll[assetRow](c, "/v1/assets", q)
	if err != nil {
		return writeTransportErr(err, stderr)
	}
	if status/100 != 2 {
		fmt.Fprintf(stderr, "error: http %d\n", status)
		return 1
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Path < items[j].Path })
	switch *format {
	case "csv":
		if err := writeAssetCSV(stdout, items); err != nil {
			fmt.Fprintln(stderr, "error: "+err.Error())
			return 1
		}
	case "yaml":
		if err := writeAssetYAML(stdout, items); err != nil {
			fmt.Fprintln(stderr, "error: "+err.Error())
			return 1
		}
	}
	return 0
}

// writeAssetCSV writes a CSV with columns: path,type,lifecycle, then a
// stable sorted set of `spec.<key>` columns. Empty spec or missing
// fields render as the empty string. Header order is fixed so two
// exports diff cleanly.
func writeAssetCSV(w io.Writer, rows []assetRow) error {
	cw := csv.NewWriter(w)
	header := []string{"path", "type", "lifecycle"}
	specKeys := collectSpecKeys(rows)
	for _, k := range specKeys {
		header = append(header, "spec."+k)
	}
	if err := cw.Write(header); err != nil {
		return err
	}
	for _, r := range rows {
		line := []string{r.Path, specString(r.Spec, "type"), specString(r.Spec, "lifecycle")}
		for _, k := range specKeys {
			line = append(line, specString(r.Spec, k))
		}
		if err := cw.Write(line); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

// writeAssetYAML writes the rows as a YAML array of objects (same
// shape as the server's GET /v1/assets/{path} response).
func writeAssetYAML(w io.Writer, rows []assetRow) error {
	out := make([]assetRow, len(rows))
	copy(out, rows)
	buf, err := yaml.Marshal(out)
	if err != nil {
		return err
	}
	_, err = w.Write(buf)
	return err
}

// collectSpecKeys returns the sorted union of all spec keys across
// the rows so the CSV header is stable regardless of row order.
func collectSpecKeys(rows []assetRow) []string {
	seen := map[string]bool{}
	for _, r := range rows {
		for k := range r.Spec {
			seen[k] = true
		}
	}
	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// specString returns the string form of a spec key, or "" if missing.
// Numbers/bools marshal via fmt; nested objects marshal to JSON.
func specString(spec map[string]any, key string) string {
	if spec == nil {
		return ""
	}
	v, ok := spec[key]
	if !ok {
		return ""
	}
	switch x := v.(type) {
	case string:
		return x
	case bool:
		return strconv.FormatBool(x)
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64)
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	default:
		buf, err := json.Marshal(x)
		if err != nil {
			return ""
		}
		return string(buf)
	}
}

// --- import --------------------------------------------------------------

// assetImportCmd handles `cios asset import -f <file> [--dry-run]`.
//
// Parses CSV or YAML (auto-detected by extension), then PUTs each row
// against /v1/assets/{path} (idempotent upsert via existing API).
// Per-row failures are collected and reported; --dry-run never writes.
func assetImportCmd(g globalFlags, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("asset import", flag.ContinueOnError)
	fs.SetOutput(stderr)
	file := fs.String("f", "", "input file (csv|yaml; use - for stdin)")
	dryRun := fs.Bool("dry-run", false, "parse + validate only; do not PUT")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: cios asset import -f <file> [--dry-run]")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *file == "" {
		fmt.Fprintln(stderr, "error: -f <file> is required")
		return 2
	}
	raw, err := readApplyInput(*file, stdin)
	if err != nil {
		fmt.Fprintln(stderr, "error: "+err.Error())
		return 2
	}
	rows, err := parseImportInput(raw, *file)
	if err != nil {
		fmt.Fprintln(stderr, "error: "+err.Error())
		return 2
	}
	if *dryRun {
		fmt.Fprintf(stdout, "dry-run: %d row(s) parsed\n", len(rows))
		for _, r := range rows {
			fmt.Fprintf(stdout, "  %s (%s)\n", r.Path, specString(r.Spec, "type"))
		}
		return 0
	}
	c := NewClient(g.server)
	var ok, failed int
	var firstErr string
	for _, r := range rows {
		if r.Path == "" {
			failed++
			if firstErr == "" {
				firstErr = "row missing path"
			}
			fmt.Fprintf(stderr, "error: row missing path\n")
			continue
		}
		body := map[string]any{
			"spec":       r.Spec,
			"request_id": reqid.New(),
		}
		status, respBody, err := c.Do("PUT", "/v1/assets/"+r.Path, nil, body)
		if err != nil {
			failed++
			fmt.Fprintln(stderr, "error: "+err.Error())
			if firstErr == "" {
				firstErr = err.Error()
			}
			continue
		}
		if status/100 != 2 {
			failed++
			msg := strings.TrimSpace(string(respBody))
			if msg == "" {
				msg = fmt.Sprintf("http %d", status)
			}
			fmt.Fprintf(stderr, "error: %s: %s\n", r.Path, msg)
			if firstErr == "" {
				firstErr = fmt.Sprintf("%s: %s", r.Path, msg)
			}
			continue
		}
		ok++
	}
	fmt.Fprintf(stdout, "imported %d, failed %d\n", ok, failed)
	if failed > 0 {
		return 1
	}
	return 0
}

// parseImportInput decodes CSV or YAML based on the file extension.
// "-" (stdin) defaults to YAML (matches `apply` heuristic; tests pass
// raw CSV via a .csv tempfile to force the CSV path).
func parseImportInput(raw []byte, file string) ([]assetRow, error) {
	lower := strings.ToLower(file)
	switch {
	case file == "-":
		return parseImportYAML(raw)
	case strings.HasSuffix(lower, ".csv"):
		return parseImportCSV(raw)
	case strings.HasSuffix(lower, ".yaml"), strings.HasSuffix(lower, ".yml"):
		return parseImportYAML(raw)
	default:
		return nil, fmt.Errorf("unsupported input extension (want .csv/.yaml/.yml or -): %s", file)
	}
}

func parseImportCSV(raw []byte) ([]assetRow, error) {
	r := csv.NewReader(strings.NewReader(string(raw)))
	header, err := r.Read()
	if err != nil {
		return nil, fmt.Errorf("csv: read header: %w", err)
	}
	cols := make([]string, len(header))
	copy(cols, header)
	out := []assetRow{}
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("csv: %w", err)
		}
		row := assetRow{Spec: map[string]any{}}
		for i, cell := range rec {
			if i >= len(cols) {
				continue
			}
			col := cols[i]
			switch col {
			case "path":
				row.Path = cell
			default:
				if rest := strings.TrimPrefix(col, "spec."); rest != col {
					if cell != "" {
						row.Spec[rest] = decodeCSVSpecCell(cell)
					}
				}
				// Unknown columns (e.g. "type", "lifecycle" at top
				// level) are silently dropped — they're already
				// encoded as spec.type / spec.lifecycle.
			}
		}
		out = append(out, row)
	}
	return out, nil
}

// decodeCSVSpecCell restores a CSV cell into a typed value. Strings
// stay strings; cells that look like JSON objects/arrays get decoded
// back into nested maps/slices (round-trip with specString).
func decodeCSVSpecCell(cell string) any {
	c := strings.TrimSpace(cell)
	if c == "" {
		return ""
	}
	if c[0] == '{' || c[0] == '[' {
		var v any
		if err := json.Unmarshal([]byte(c), &v); err == nil {
			return v
		}
	}
	// Booleans / numbers — restore typed form so the spec round-trips.
	if c == "true" {
		return true
	}
	if c == "false" {
		return false
	}
	if n, err := strconv.ParseInt(c, 10, 64); err == nil {
		return n
	}
	if f, err := strconv.ParseFloat(c, 64); err == nil {
		return f
	}
	return c
}

func parseImportYAML(raw []byte) ([]assetRow, error) {
	var rows []assetRow
	if err := yaml.Unmarshal(raw, &rows); err != nil {
		return nil, fmt.Errorf("yaml: %w", err)
	}
	for i := range rows {
		if rows[i].Spec == nil {
			rows[i].Spec = map[string]any{}
		}
	}
	return rows, nil
}
