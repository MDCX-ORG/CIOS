// Package cli — output.go: three-mode rendering for cios commands.
//
// Modes:
//   - json:     json.MarshalIndent
//   - yaml:     gopkg.in/yaml.v3
//   - table:    text/tabwriter
//
// The table format relies on TableSpec.Columns (uppercase header) +
// TableSpec.Row (single-row renderer). For ad-hoc renderers like the
// query command (no header, single line) the same mechanism is used
// with empty Columns.
package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"gopkg.in/yaml.v3"
)

// TableSpec drives table rendering. Columns are emitted in order;
// Row must return len(Columns) cells.
type TableSpec struct {
	Columns []string
	Row     func(item any) []string
}

// Print renders v in the requested mode. Empty Columns + empty items
// in table mode writes nothing (callers decide whether to also emit
// a stderr sentinel like "no assets").
func Print(w io.Writer, mode string, v any, table TableSpec) error {
	switch mode {
	case "json":
		buf, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return fmt.Errorf("output: json: %w", err)
		}
		if _, err := io.WriteString(w, string(buf)+"\n"); err != nil {
			return err
		}
		return nil
	case "yaml":
		buf, err := yaml.Marshal(v)
		if err != nil {
			return fmt.Errorf("output: yaml: %w", err)
		}
		_, err = io.WriteString(w, string(buf))
		return err
	case "table":
		return renderTable(w, v, table)
	default:
		return fmt.Errorf("output: unknown mode %q", mode)
	}
}

// renderTable writes header + rows. Empty Columns → no header. Empty
// v slice → no output at all (the caller is responsible for the
// "no items" stderr sentinel).
func renderTable(w io.Writer, v any, table TableSpec) error {
	tw := tabwriter.NewWriter(w, 2, 2, 2, ' ', 0)
	if len(table.Columns) > 0 {
		if _, err := fmt.Fprintln(tw, strings.Join(table.Columns, "\t")); err != nil {
			return err
		}
	}
	items, ok := v.([]any)
	if !ok {
		return nil
	}
	for _, it := range items {
		row := table.Row(it)
		if len(row) != len(table.Columns) {
			return fmt.Errorf("output: row width %d != header %d", len(row), len(table.Columns))
		}
		if _, err := fmt.Fprintln(tw, strings.Join(row, "\t")); err != nil {
			return err
		}
	}
	return tw.Flush()
}

// formatTime trims nanoseconds and renders RFC3339 in UTC.
func formatTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

// formatRFC3339String passes through if it parses; otherwise returns
// the raw string. Used for fields the server already formatted.
func formatRFC3339String(s string) string {
	if s == "" {
		return ""
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.UTC().Format(time.RFC3339)
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC().Format(time.RFC3339)
	}
	return s
}

// metricLabelString joins metric labels (excluding __name__) sorted
// by key as `k="v"`, comma-separated. Empty if no labels.
func metricLabelString(metric map[string]string) string {
	keys := make([]string, 0, len(metric))
	for k := range metric {
		if k == "__name__" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf(`%s=%q`, k, metric[k]))
	}
	return strings.Join(parts, ", ")
}

// metricTimeString parses VM's float epoch seconds into RFC3339 UTC.
func metricTimeString(s string) string {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return s
	}
	sec := int64(f)
	return time.Unix(sec, 0).UTC().Format(time.RFC3339)
}
