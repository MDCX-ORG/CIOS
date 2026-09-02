// Package cli — query.go: `cios query <point-path>` (GET /v1/points/{path})
// and `cios metric query <promql> [--time RFC3339]` (GET /v1/metrics/query).
//
// Output formats follow §4.4.
package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// queryCmd handles `cios query <point-path>`.
func queryCmd(g globalFlags, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("query", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprintln(stderr, "usage: cios query <point-path>") }
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) != 1 {
		fmt.Fprintln(stderr, "usage: cios query <point-path>")
		return 2
	}
	path := rest[0]
	c := NewClient(g.server)
	status, body, err := c.Do("GET", "/v1/points/"+path, nil, nil)
	if err != nil {
		return writeTransportErr(err, stderr)
	}
	if status/100 != 2 {
		// Non-problem error: try to decode a problem body if present,
		// otherwise fall back to status+body.
		var p Problem
		if jerr := json.Unmarshal(body, &p); jerr == nil && p.Title != "" {
			fmt.Fprintf(stderr, "error: %s\n", p.Error())
		} else {
			fmt.Fprintf(stderr, "error: http %d: %s\n", status, strings.TrimSpace(string(body)))
		}
		return 1
	}
	if g.output == "json" || g.output == "yaml" {
		if _, err := stdout.Write(body); err != nil {
			return 1
		}
		return 0
	}
	// Table mode: single line "<value> <unit?> (<quality>, <ts>)".
	var resp struct {
		Path    string  `json:"path"`
		Value   float64 `json:"value"`
		Unit    string  `json:"unit"`
		Ts      string  `json:"ts"`
		Quality string  `json:"quality"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		fmt.Fprintf(stderr, "error: decode: %s\n", err.Error())
		return 1
	}
	if resp.Unit != "" {
		fmt.Fprintf(stdout, "%s %s (%s, %s)\n", formatValue(resp.Value), resp.Unit, resp.Quality, resp.Ts)
	} else {
		fmt.Fprintf(stdout, "%s (%s, %s)\n", formatValue(resp.Value), resp.Quality, resp.Ts)
	}
	return 0
}

// formatValue preserves server-side numeric precision (§4.4). Use
// strconv.FormatFloat with -1 precision (shortest round-trippable).
func formatValue(v float64) string {
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// metricCmd dispatches to `metric query`.
func metricCmd(g globalFlags, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: cios metric query <promql> [--time RFC3339]")
		return 2
	}
	switch args[0] {
	case "query":
		return metricQueryCmd(g, args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "error: unknown metric subcommand %q\n", args[0])
		return 2
	}
}

// vmSeries is one entry of VM's /api/v1/query result array.
type vmSeries struct {
	Metric map[string]string `json:"metric"`
	Value  []any             `json:"value"`
}

func metricQueryCmd(g globalFlags, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("metric query", flag.ContinueOnError)
	fs.SetOutput(stderr)
	timeFlag := fs.String("time", "", "RFC3339 evaluation time")
	fs.Usage = func() { fmt.Fprintln(stderr, "usage: cios metric query <promql> [--time RFC3339]") }
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) != 1 {
		fmt.Fprintln(stderr, "usage: cios metric query <promql> [--time RFC3339]")
		return 2
	}
	promql := rest[0]
	q := url.Values{}
	q.Set("query", promql)
	if *timeFlag != "" {
		q.Set("time", *timeFlag)
	}
	c := NewClient(g.server)
	status, body, err := c.Do("GET", "/v1/metrics/query", q, nil)
	if err != nil {
		return writeTransportErr(err, stderr)
	}
	if status/100 != 2 {
		var p Problem
		if jerr := json.Unmarshal(body, &p); jerr == nil && p.Title != "" {
			fmt.Fprintf(stderr, "error: %s\n", p.Error())
		} else {
			fmt.Fprintf(stderr, "error: http %d: %s\n", status, strings.TrimSpace(string(body)))
		}
		return 1
	}
	if g.output == "json" || g.output == "yaml" {
		if _, err := stdout.Write(body); err != nil {
			return 1
		}
		return 0
	}
	var resp struct {
		Data struct {
			ResultType string     `json:"resultType"`
			Result     []vmSeries `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		fmt.Fprintf(stderr, "error: decode: %s\n", err.Error())
		return 1
	}
	if len(resp.Data.Result) == 0 {
		fmt.Fprintln(stderr, "no metric results")
		return 0
	}
	rows := make([]any, 0, len(resp.Data.Result))
	for _, s := range resp.Data.Result {
		rows = append(rows, s)
	}
	tbl := TableSpec{
		Columns: []string{"METRIC", "LABELS", "VALUE", "TS"},
		Row: func(v any) []string {
			s := v.(vmSeries)
			name := s.Metric["__name__"]
			if name == "" {
				name = "<unnamed>"
			}
			var valStr, tsStr string
			if len(s.Value) >= 2 {
				if vs, ok := s.Value[1].(string); ok {
					valStr = vs
				}
				// VM emits [ts, "v"] where ts is a JSON number (float
				// epoch seconds). Accept either string or float.
				switch v := s.Value[0].(type) {
				case string:
					tsStr = metricTimeString(v)
				case float64:
					tsStr = time.Unix(int64(v), 0).UTC().Format(time.RFC3339)
				}
			}
			return []string{name, metricLabelString(s.Metric), valStr, tsStr}
		},
	}
	if err := Print(stdout, g.output, rows, tbl); err != nil {
		fmt.Fprintln(stderr, "error: "+err.Error())
		return 2
	}
	return 0
}
