// Package cli — maintenance_window.go: `cios maintenance window
// list|create|delete` against /v1/maintenance/windows (M2 E2.4 /
// PRMT-096). Mirrors the request/response shape used by other CLI
// commands in this package (alarm.go, pm.go): JSON-in/JSON-out
// pass-through for create + delete, JSON envelope decode + table
// render for list, and -o json/yaml for machine-readable output.
package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"time"
)

// maintenanceWindowRow is the wire shape returned by GET
// /v1/maintenance/windows. JSON tags mirror core.MaintenanceWindow
// so the create / list bodies round-trip without an extra decode
// step.
type maintenanceWindowRow struct {
	ID        string    `json:"id"`
	AssetPath string    `json:"asset_path"`
	StartsAt  time.Time `json:"starts_at"`
	EndsAt    time.Time `json:"ends_at"`
	Reason    string    `json:"reason"`
}

type listMaintenanceWindowsResponse struct {
	Items         []maintenanceWindowRow `json:"items"`
	NextPageToken string                 `json:"page_token"`
}

// maintenanceWindowCmd dispatches the `window` subcommand to its
// list / create / delete handlers. Mirrors the maintenanceCmd
// dispatcher in maintenance.go.
func maintenanceWindowCmd(g globalFlags, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: cios maintenance window <list|create|delete> ...")
		return 2
	}
	switch args[0] {
	case "list":
		return maintenanceWindowListCmd(g, args[1:], stdout, stderr)
	case "create":
		return maintenanceWindowCreateCmd(g, args[1:], stdout, stderr)
	case "delete":
		return maintenanceWindowDeleteCmd(g, args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "error: unknown maintenance window subcommand %q\n", args[0])
		return 2
	}
}

// maintenanceWindowListCmd lists /v1/maintenance/windows with
// optional --page-size. Mirrors the shape of `cios alarm list` /
// `cios pm list` (page_size on the wire, table by default,
// -o json / -o yaml for pass-through).
func maintenanceWindowListCmd(g globalFlags, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("maintenance window list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	pageSize := fs.Int("page-size", 0, "page size (default 100, max 1000)")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: cios maintenance window list [--page-size N]")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	var q url.Values
	if *pageSize > 0 {
		q = url.Values{"page_size": []string{strconv.Itoa(*pageSize)}}
	}
	c := NewClient(g.server)
	status, body, err := c.Do("GET", "/v1/maintenance/windows", q, nil)
	if err != nil {
		return writeTransportErr(err, stderr)
	}
	if status/100 != 2 {
		return writeHTTPStatus(status, body, stderr)
	}
	var got listMaintenanceWindowsResponse
	if err := json.Unmarshal(body, &got); err != nil {
		fmt.Fprintf(stderr, "error: decode: %s\n", err.Error())
		return 1
	}
	if g.output == "json" || g.output == "yaml" {
		_, err := stdout.Write(body)
		if err != nil {
			return 1
		}
		return 0
	}
	if len(got.Items) == 0 {
		fmt.Fprintln(stderr, "no maintenance windows")
		return 0
	}
	printMaintenanceWindowTable(stdout, &got)
	return 0
}

// maintenanceWindowCreateCmd POSTs a new window. Mirrors the
// `cios pm create` shape: required --asset-path, --starts-at,
// --ends-at, optional --reason. RFC3339 timestamps are validated
// client-side so a bad input never reaches the server (matches
// `maintenance upcoming`'s --before contract).
func maintenanceWindowCreateCmd(g globalFlags, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("maintenance window create", flag.ContinueOnError)
	fs.SetOutput(stderr)
	assetPath := fs.String("asset-path", "", "asset path (required, crn)")
	startsAt := fs.String("starts-at", "", "window start, RFC3339 (required)")
	endsAt := fs.String("ends-at", "", "window end, RFC3339 (required)")
	reason := fs.String("reason", "", "human-readable reason (optional)")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: cios maintenance window create --asset-path PATH --starts-at RFC3339 --ends-at RFC3339 [--reason TEXT]")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *assetPath == "" || *startsAt == "" || *endsAt == "" {
		fs.Usage()
		return 2
	}
	if _, err := time.Parse(time.RFC3339, *startsAt); err != nil {
		fmt.Fprintf(stderr, "error: --starts-at must be RFC3339: %s\n", err.Error())
		return 2
	}
	if _, err := time.Parse(time.RFC3339, *endsAt); err != nil {
		fmt.Fprintf(stderr, "error: --ends-at must be RFC3339: %s\n", err.Error())
		return 2
	}
	payload := map[string]string{
		"asset_path": *assetPath,
		"starts_at":  *startsAt,
		"ends_at":    *endsAt,
		"reason":     *reason,
	}
	c := NewClient(g.server)
	status, resp, err := c.Do("POST", "/v1/maintenance/windows", nil, payload)
	if err != nil {
		return writeTransportErr(err, stderr)
	}
	if status/100 != 2 {
		return writeHTTPStatus(status, resp, stderr)
	}
	if g.output == "json" || g.output == "yaml" {
		_, err := stdout.Write(resp)
		if err != nil {
			return 1
		}
		return 0
	}
	var created maintenanceWindowRow
	if err := json.Unmarshal(resp, &created); err != nil {
		fmt.Fprintf(stderr, "error: decode: %s\n", err.Error())
		return 1
	}
	fmt.Fprintf(stdout, "created %s asset=%s starts=%s ends=%s\n",
		created.ID, created.AssetPath,
		created.StartsAt.Format(time.RFC3339),
		created.EndsAt.Format(time.RFC3339))
	return 0
}

// maintenanceWindowDeleteCmd DELETEs a window by id. Mirrors the
// `cios pm delete` / `cios ticket close` shape.
func maintenanceWindowDeleteCmd(g globalFlags, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("maintenance window delete", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: cios maintenance window delete MW_ID")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	id := fs.Arg(0)
	c := NewClient(g.server)
	status, body, err := c.Do("DELETE", "/v1/maintenance/windows/"+id, nil, nil)
	if err != nil {
		return writeTransportErr(err, stderr)
	}
	if status/100 != 2 {
		return writeHTTPStatus(status, body, stderr)
	}
	fmt.Fprintf(stdout, "deleted %s\n", id)
	return 0
}

// printMaintenanceWindowTable renders the list as a plain-text
// table for humans. Columns mirror the upcoming view's style so
// the operator's muscle memory works across the two surfaces.
func printMaintenanceWindowTable(w io.Writer, r *listMaintenanceWindowsResponse) {
	fmt.Fprintln(w, "MAINTENANCE WINDOWS")
	fmt.Fprintf(w, "  %-19s  %-19s  %-32s  %s\n",
		"STARTS", "ENDS", "ASSET", "ID")
	for _, it := range r.Items {
		fmt.Fprintf(w, "  %-19s  %-19s  %-32s  %s\n",
			formatTime(it.StartsAt),
			formatTime(it.EndsAt),
			it.AssetPath,
			it.ID,
		)
	}
}
