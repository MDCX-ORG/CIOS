// Package cli — asset_test.go: tests for PRMT-068 asset export/import.
//
// Covers: CSV/YAML round-trip, dry-run (no PUT), import idempotent
// upsert, single-row failure continues + non-zero exit, nested spec
// flatten on export + restore on import.
package cli

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// --- export: CSV ---------------------------------------------------------

func TestAssetExportCSV(t *testing.T) {
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/assets", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"items":[`+
				`{"path":"sgp01.pod002.cdu000","resource_version":1,"spec":{"type":"cdu","lifecycle":"active","vendor":"acme"},"created_at":"","updated_at":""},`+
				`{"path":"sgp01.pod002.cdu001","resource_version":2,"spec":{"type":"cdu","lifecycle":"planned"},"created_at":"","updated_at":""}`+
				`],"next_page_token":""}`+"\n")
		})
	})
	defer srv.Close()
	code, out, errOut := runWithServer(t, srv, []string{"asset", "export", "--format", "csv"}, nil)
	if code != 0 {
		t.Fatalf("want 0, got %d stderr=%s", code, errOut)
	}
	r := csv.NewReader(strings.NewReader(out))
	records, err := r.ReadAll()
	if err != nil {
		t.Fatalf("csv parse: %v out=%q", err, out)
	}
	if len(records) != 3 {
		t.Fatalf("want 3 rows (header + 2 data), got %d out=%q", len(records), out)
	}
	header := records[0]
	// Header includes the spec columns (flattened) in sorted order.
	wantHeader := []string{"path", "type", "lifecycle", "spec.lifecycle", "spec.type", "spec.vendor"}
	if len(header) != len(wantHeader) {
		t.Fatalf("header len want %d got %d (%v)", len(wantHeader), len(header), header)
	}
	for i, w := range wantHeader {
		if header[i] != w {
			t.Fatalf("header[%d] want %q got %q", i, w, header[i])
		}
	}
	// Rows are sorted by path → cdu000 before cdu001.
	if records[1][0] != "sgp01.pod002.cdu000" || records[2][0] != "sgp01.pod002.cdu001" {
		t.Fatalf("sort order wrong: %v", []string{records[1][0], records[2][0]})
	}
	// Spec.type is encoded under spec.type column.
	for i, rec := range [][]string{records[1], records[2]} {
		if rec[4] != "cdu" {
			t.Fatalf("row %d spec.type want cdu got %q", i, rec[4])
		}
	}
	if records[1][5] != "acme" || records[2][5] != "" {
		t.Fatalf("spec.vendor wrong: %v", []string{records[1][5], records[2][5]})
	}
}

// --- export: YAML --------------------------------------------------------

func TestAssetExportYAML(t *testing.T) {
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/assets", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"items":[{"path":"a","resource_version":1,"spec":{"type":"cdu"},"created_at":"","updated_at":""}],"next_page_token":""}`+"\n")
		})
	})
	defer srv.Close()
	code, out, errOut := runWithServer(t, srv, []string{"asset", "export", "--format", "yaml"}, nil)
	if code != 0 {
		t.Fatalf("want 0, got %d stderr=%s", code, errOut)
	}
	var rows []assetRow
	if err := yaml.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("yaml: %v out=%q", err, out)
	}
	if len(rows) != 1 || rows[0].Path != "a" || rows[0].Spec["type"] != "cdu" {
		t.Fatalf("unexpected rows: %+v", rows)
	}
}

// --- export: prefix pass-through + stable sort ----------------------------

func TestAssetExportPrefixAndSort(t *testing.T) {
	var seenPrefix string
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/assets", func(w http.ResponseWriter, r *http.Request) {
			seenPrefix = r.URL.Query().Get("prefix")
			// Server returns in reverse order; client must sort.
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"items":[`+
				`{"path":"z","resource_version":1,"spec":{"type":"x"},"created_at":"","updated_at":""},`+
				`{"path":"a","resource_version":1,"spec":{"type":"x"},"created_at":"","updated_at":""}`+
				`],"next_page_token":""}`+"\n")
		})
	})
	defer srv.Close()
	// 1. prefix pass-through.
	code, _, errOut := runWithServer(t, srv, []string{"asset", "export", "--prefix", "sgp01", "--format", "yaml"}, nil)
	if code != 0 {
		t.Fatalf("want 0, got %d stderr=%s", code, errOut)
	}
	if seenPrefix != "sgp01" {
		t.Fatalf("want prefix=sgp01, got %q", seenPrefix)
	}
	// 2. stable sort.
	_, out, _ := runWithServer(t, srv, []string{"asset", "export", "--format", "yaml"}, nil)
	var rows []assetRow
	if err := yaml.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("yaml: %v out=%s", err, out)
	}
	if len(rows) != 2 || rows[0].Path != "a" || rows[1].Path != "z" {
		t.Fatalf("sort order wrong: %v", []string{rows[0].Path, rows[1].Path})
	}
}

// --- export: bad format / RFC 7807 ----------------------------------------

func TestAssetExportBadFormat(t *testing.T) {
	srv := newFakeCore(t)
	defer srv.Close()
	code, _, errOut := runWithServer(t, srv, []string{"asset", "export", "--format", "xml"}, nil)
	if code != 2 {
		t.Fatalf("want 2, got %d stderr=%s", code, errOut)
	}
}

func TestAssetExportRFC7807(t *testing.T) {
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/assets", func(w http.ResponseWriter, r *http.Request) {
			writeProblem(w, http.StatusInternalServerError, "store-error", "store error", "disk", r.URL.Path, "rid-e")
		})
	})
	defer srv.Close()
	code, _, errOut := runWithServer(t, srv, []string{"asset", "export"}, nil)
	if code != 1 {
		t.Fatalf("want 1, got %d stderr=%s", code, errOut)
	}
	if !strings.Contains(errOut, "request_id=rid-e") {
		t.Fatalf("stderr=%q", errOut)
	}
}

// --- import: CSV upsert ---------------------------------------------------

func TestAssetImportCSVSuccess(t *testing.T) {
	var putCount int
	var seenBodies []map[string]any
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/assets/", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPut {
				writeProblem(w, http.StatusMethodNotAllowed, "bad-request", "method", "", r.URL.Path, "rid")
				return
			}
			putCount++
			raw, _ := io.ReadAll(r.Body)
			var b map[string]any
			_ = json.Unmarshal(raw, &b)
			seenBodies = append(seenBodies, b)
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"path":"x","resource_version":1,"spec":{},"created_at":"","updated_at":""}`+"\n")
		})
	})
	defer srv.Close()
	dir := t.TempDir()
	csvPath := filepath.Join(dir, "assets.csv")
	csvBody := "path,type,spec.lifecycle,spec.vendor\n" +
		"sgp01.pod002.cdu000,cdu,active,acme\n" +
		"sgp01.pod002.cdu001,cdu,planned,\n"
	if err := os.WriteFile(csvPath, []byte(csvBody), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := runWithServer(t, srv, []string{"asset", "import", "-f", csvPath}, nil)
	if code != 0 {
		t.Fatalf("want 0, got %d stderr=%s out=%s", code, errOut, out)
	}
	if putCount != 2 {
		t.Fatalf("want 2 PUTs, got %d", putCount)
	}
	// Verify spec.lifecycle / spec.type survived flatten→restore.
	for i, want := range []string{"active", "planned"} {
		spec, _ := seenBodies[i]["spec"].(map[string]any)
		if spec["lifecycle"] != want {
			t.Fatalf("row %d lifecycle=%v want %s", i, spec["lifecycle"], want)
		}
	}
	if !strings.Contains(out, "imported 2, failed 0") {
		t.Fatalf("summary missing: %q", out)
	}
}

// --- import: idempotent upsert (server returns 2xx on repeat) -------------

func TestAssetImportIdempotent(t *testing.T) {
	var putCount int
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/assets/p1", func(w http.ResponseWriter, r *http.Request) {
			putCount++
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"path":"p1","resource_version":`+
				strings.Repeat("0", putCount)+`,"spec":{},"created_at":"","updated_at":""}`+"\n")
		})
	})
	defer srv.Close()
	dir := t.TempDir()
	p := filepath.Join(dir, "a.csv")
	if err := os.WriteFile(p, []byte("path,type\np1,cdu\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		code, _, errOut := runWithServer(t, srv, []string{"asset", "import", "-f", p}, nil)
		if code != 0 {
			t.Fatalf("iter %d: exit=%d stderr=%s", i, code, errOut)
		}
	}
	if putCount != 2 {
		t.Fatalf("want 2 PUTs, got %d", putCount)
	}
}

// --- import: dry-run does not write --------------------------------------

func TestAssetImportDryRun(t *testing.T) {
	var putCount int
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/assets/", func(w http.ResponseWriter, r *http.Request) {
			putCount++
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"path":"x","resource_version":1,"spec":{},"created_at":"","updated_at":""}`+"\n")
		})
	})
	defer srv.Close()
	dir := t.TempDir()
	p := filepath.Join(dir, "a.csv")
	if err := os.WriteFile(p, []byte("path,type\np1,cdu\np2,cdu\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := runWithServer(t, srv, []string{"asset", "import", "-f", p, "--dry-run"}, nil)
	if code != 0 {
		t.Fatalf("want 0, got %d stderr=%s", code, errOut)
	}
	if putCount != 0 {
		t.Fatalf("dry-run must not write, got %d PUTs", putCount)
	}
	if !strings.Contains(out, "dry-run: 2 row(s) parsed") {
		t.Fatalf("dry-run summary missing: %q", out)
	}
	if !strings.Contains(out, "p1") || !strings.Contains(out, "p2") {
		t.Fatalf("paths not printed: %q", out)
	}
}

// --- import: single row failure continues + non-zero exit -----------------

func TestAssetImportPartialFailure(t *testing.T) {
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/assets/p1", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"path":"p1","resource_version":1,"spec":{},"created_at":"","updated_at":""}`+"\n")
		})
		mux.HandleFunc("/v1/assets/p2", func(w http.ResponseWriter, r *http.Request) {
			writeProblem(w, http.StatusBadRequest, "bad-path", "bad asset path", "syntax", r.URL.Path, "rid-p2")
		})
		mux.HandleFunc("/v1/assets/p3", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"path":"p3","resource_version":1,"spec":{},"created_at":"","updated_at":""}`+"\n")
		})
	})
	defer srv.Close()
	dir := t.TempDir()
	p := filepath.Join(dir, "a.csv")
	if err := os.WriteFile(p, []byte("path,type\np1,cdu\np2,bad\np3,cdu\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := runWithServer(t, srv, []string{"asset", "import", "-f", p}, nil)
	if code != 1 {
		t.Fatalf("want 1 (any failure), got %d stderr=%s", code, errOut)
	}
	if !strings.Contains(out, "imported 2, failed 1") {
		t.Fatalf("summary wrong: %q", out)
	}
	if !strings.Contains(errOut, "bad asset path") {
		t.Fatalf("p2 failure not surfaced: %q", errOut)
	}
}

// --- import: missing -f ---------------------------------------------------

func TestAssetImportMissingFile(t *testing.T) {
	srv := newFakeCore(t)
	defer srv.Close()
	code, _, errOut := runWithServer(t, srv, []string{"asset", "import", "-f", "/no/such.csv"}, nil)
	if code != 2 {
		t.Fatalf("want 2, got %d stderr=%s", code, errOut)
	}
}

func TestAssetImportMissingFlag(t *testing.T) {
	srv := newFakeCore(t)
	defer srv.Close()
	code, _, errOut := runWithServer(t, srv, []string{"asset", "import"}, nil)
	if code != 2 {
		t.Fatalf("want 2, got %d stderr=%s", code, errOut)
	}
}

func TestAssetImportBadExtension(t *testing.T) {
	srv := newFakeCore(t)
	defer srv.Close()
	dir := t.TempDir()
	p := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, errOut := runWithServer(t, srv, []string{"asset", "import", "-f", p}, nil)
	if code != 2 {
		t.Fatalf("want 2, got %d stderr=%s", code, errOut)
	}
}

// --- import: YAML path ----------------------------------------------------

func TestAssetImportYAML(t *testing.T) {
	var putCount int
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/assets/y1", func(w http.ResponseWriter, r *http.Request) {
			putCount++
			raw, _ := io.ReadAll(r.Body)
			var b map[string]any
			_ = json.Unmarshal(raw, &b)
			spec, _ := b["spec"].(map[string]any)
			if spec["type"] != "cdu" {
				t.Errorf("spec.type want cdu, got %v", spec["type"])
			}
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"path":"y1","resource_version":1,"spec":{},"created_at":"","updated_at":""}`+"\n")
		})
	})
	defer srv.Close()
	dir := t.TempDir()
	p := filepath.Join(dir, "a.yaml")
	doc := "- path: y1\n  spec:\n    type: cdu\n    lifecycle: active\n"
	if err := os.WriteFile(p, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := runWithServer(t, srv, []string{"asset", "import", "-f", p}, nil)
	if code != 0 {
		t.Fatalf("want 0, got %d stderr=%s", code, errOut)
	}
	if putCount != 1 {
		t.Fatalf("want 1 PUT, got %d", putCount)
	}
	if !strings.Contains(out, "imported 1, failed 0") {
		t.Fatalf("summary: %q", out)
	}
}

// --- CSV round-trip with nested spec --------------------------------------

func TestAssetCSVRoundTripNestedSpec(t *testing.T) {
	// Server returns a row with nested spec; we export → import → and
	// the import-side PUT body must contain the same nested key.
	var putSpec map[string]any
	srv := newFakeCoreWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/v1/assets", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			// Nested object inside spec.
			io.WriteString(w, `{"items":[{"path":"p","resource_version":1,"spec":{"type":"cdu","location":{"site":"sgp01","row":2},"lifecycle":"active"},"created_at":"","updated_at":""}],"next_page_token":""}`+"\n")
		})
		mux.HandleFunc("/v1/assets/p", func(w http.ResponseWriter, r *http.Request) {
			raw, _ := io.ReadAll(r.Body)
			var b map[string]any
			_ = json.Unmarshal(raw, &b)
			putSpec, _ = b["spec"].(map[string]any)
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"path":"p","resource_version":2,"spec":{},"created_at":"","updated_at":""}`+"\n")
		})
	})
	defer srv.Close()
	// 1. Export to CSV.
	_, csvOut, errOut := runWithServer(t, srv, []string{"asset", "export", "--format", "csv"}, nil)
	if errOut != "" {
		t.Fatalf("export stderr=%s", errOut)
	}
	// 2. The nested location column should be JSON-encoded inside the CSV.
	if !strings.Contains(csvOut, "spec.location") {
		t.Fatalf("csv missing spec.location column: %q", csvOut)
	}
	// CSV escaping doubles internal quotes — check for either form.
	if !strings.Contains(csvOut, `"site":"sgp01"`) && !strings.Contains(csvOut, `""site"":""sgp01""`) {
		t.Fatalf("csv missing nested JSON: %q", csvOut)
	}
	// 3. Write CSV to a file, import it.
	dir := t.TempDir()
	p := filepath.Join(dir, "rt.csv")
	if err := os.WriteFile(p, []byte(csvOut), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, errOut := runWithServer(t, srv, []string{"asset", "import", "-f", p}, nil)
	if code != 0 {
		t.Fatalf("import exit=%d stderr=%s", code, errOut)
	}
	if putSpec == nil {
		t.Fatalf("no PUT body captured")
	}
	// The nested key must round-trip.
	loc, ok := putSpec["location"].(map[string]any)
	if !ok {
		t.Fatalf("nested location lost: %v", putSpec["location"])
	}
	if loc["site"] != "sgp01" || loc["row"].(float64) != 2 {
		t.Fatalf("nested fields wrong: %v", loc)
	}
}

// silence unused import warnings for tools that don't get exercised.
var _ = bytes.NewBuffer
