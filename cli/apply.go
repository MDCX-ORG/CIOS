// Package cli — apply.go: `cios apply -f <file>` (PUT /v1/assets/{path})
// and `cios delete <path> [--cascade]`.
//
// The apply input file is YAML of shape:
//
//	kind: Asset
//	metadata: { path: <cpath>, request_id?: <id> }
//	spec:     { type: <leaf-type>, ... }
//
// Any local error (missing file, invalid YAML, missing path, bad kind)
// is a usage/input error → exit 2 (no request sent).
package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/yurimeng/cios/pkg/reqid"
)

// applyDoc is the YAML input shape.
type applyDoc struct {
	Kind     string         `yaml:"kind"`
	Metadata map[string]any `yaml:"metadata"`
	Spec     map[string]any `yaml:"spec"`
}

// applyCmd handles `cios apply -f <file>`. -f - reads stdin.
func applyCmd(g globalFlags, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("apply", flag.ContinueOnError)
	fs.SetOutput(stderr)
	file := fs.String("f", "", "YAML file (use - for stdin)")
	fs.Usage = func() { fmt.Fprintln(stderr, "usage: cios apply -f <file.yaml>") }
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
	var doc applyDoc
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		fmt.Fprintln(stderr, "error: yaml: "+err.Error())
		return 2
	}
	if doc.Kind != "" && doc.Kind != "Asset" {
		fmt.Fprintf(stderr, "error: kind must be Asset (got %q)\n", doc.Kind)
		return 2
	}
	path := metadataString(doc.Metadata, "path")
	if path == "" {
		fmt.Fprintln(stderr, "error: metadata.path is required")
		return 2
	}
	requestID, _ := doc.Metadata["request_id"].(string)
	if requestID == "" {
		requestID = reqid.New()
	}
	body := map[string]any{
		"spec":       doc.Spec,
		"request_id": requestID,
	}
	c := NewClient(g.server)
	status, respBody, err := c.Do("PUT", "/v1/assets/"+path, nil, body)
	if err != nil {
		return writeTransportErr(err, stderr)
	}
	if status/100 != 2 {
		return writeHTTPStatus(status, respBody, stderr)
	}
	type assetResp struct {
		Path            string         `json:"path"`
		ResourceVersion int64          `json:"resource_version"`
		Spec            map[string]any `json:"spec"`
		CreatedAt       string         `json:"created_at"`
		UpdatedAt       string         `json:"updated_at"`
	}
	var asset assetResp
	// For json/yaml we re-emit the whole object (table mode prints the
	// fixed string per §4.1).
	switch g.output {
	case "json", "yaml":
		// Parse into the same struct as table mode so json.MarshalIndent
		// and yaml.Marshal both produce the canonical object.
		var parsed assetResp
		if err := json.Unmarshal(respBody, &parsed); err != nil {
			fmt.Fprintf(stderr, "error: decode: %s\n", err.Error())
			return 1
		}
		if err := Print(stdout, g.output, parsed, TableSpec{}); err != nil {
			fmt.Fprintln(stderr, "error: "+err.Error())
			return 2
		}
	default:
		_ = json.Unmarshal(respBody, &asset)
		fmt.Fprintf(stdout, "applied %s (rv=%d)\n", asset.Path, asset.ResourceVersion)
	}
	return 0
}

// deleteCmd handles `cios delete <path> [--cascade]`.
func deleteCmd(g globalFlags, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("delete", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cascade := fs.Bool("cascade", false, "delete subtree")
	fs.Usage = func() { fmt.Fprintln(stderr, "usage: cios delete <path> [--cascade]") }
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) != 1 {
		fmt.Fprintln(stderr, "usage: cios delete <path> [--cascade]")
		return 2
	}
	path := rest[0]
	q := url.Values{}
	if *cascade {
		q.Set("cascade", "true")
	}
	c := NewClient(g.server)
	status, respBody, err := c.Do("DELETE", "/v1/assets/"+path, q, nil)
	if err != nil {
		return writeTransportErr(err, stderr)
	}
	if status/100 != 2 {
		return writeHTTPStatus(status, respBody, stderr)
	}
	// 200 OK with {"deleted": N} — surface to stderr for the operator.
	fmt.Fprintf(stdout, "deleted %s\n", path)
	return 0
}

// writeTransportErr emits the §5 MUST stderr format for *NetError and
// any other non-problem transport-level failure. Returns 1.
func writeTransportErr(err error, stderr io.Writer) int {
	if ne, ok := IsNetError(err); ok {
		fmt.Fprintf(stderr, "error: %s\n", ne.Error())
		return 1
	}
	if p, ok := IsProblem(err); ok {
		fmt.Fprintf(stderr, "error: %s\n", p.Error())
		return 1
	}
	fmt.Fprintf(stderr, "error: %s\n", err.Error())
	return 1
}

// writeHTTPStatus handles the err=nil, status!=2xx path. The server
// may have emitted a problem (already consumed and returned as err),
// or it may have returned a non-problem error body — in that case we
// fall back to status+body as a generic error.
func writeHTTPStatus(status int, body []byte, stderr io.Writer) int {
	if len(body) > 0 {
		fmt.Fprintf(stderr, "error: http %d: %s\n", status, strings.TrimSpace(string(body)))
	} else {
		fmt.Fprintf(stderr, "error: http %d\n", status)
	}
	return 1
}

// readApplyInput reads from file ("-" → stdin).
func readApplyInput(file string, stdin io.Reader) ([]byte, error) {
	if file == "-" {
		return io.ReadAll(stdin)
	}
	return os.ReadFile(file)
}

func metadataString(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	v, ok := m[key]
	if !ok {
		return ""
	}
	s, _ := v.(string)
	return s
}
