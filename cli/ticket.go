// Package cli — ticket.go: `cios ticket list|get|open|ack|resolve|close`
// against /v1/tickets. Mirrors `cli/alarm.go` (list) and `cli/asset.go`
// (get). The four state-machine subcommands (open/ack/resolve/close)
// are all thin POST-to-transition wrappers.
package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/url"
	"strconv"
)

// ticketRow is the table row shape. JSON tags match the server's
// Ticket so json/yaml modes pass the server bytes through verbatim.
type ticketRow struct {
	ID         string `json:"id"`
	AssetPath  string `json:"asset_path"`
	Title      string `json:"title"`
	Severity   string `json:"severity"`
	State      string `json:"state"`
	Assignee   string `json:"assignee"`
	OpenedAt   string `json:"opened_at"`
	AckedAt    string `json:"acked_at,omitempty"`
	ResolvedAt string `json:"resolved_at,omitempty"`
	ClosedAt   string `json:"closed_at,omitempty"`
}

func ticketCmd(g globalFlags, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: cios ticket <list|get|open|ack|resolve|close> ...")
		return 2
	}
	switch args[0] {
	case "list":
		return ticketListCmd(g, args[1:], stdout, stderr)
	case "get":
		return ticketGetCmd(g, args[1:], stdout, stderr)
	case "open":
		return ticketOpenCmd(g, args[1:], stdout, stderr)
	case "ack":
		return ticketTransitionCmd(g, args[1:], "acknowledged", stdout, stderr)
	case "resolve":
		return ticketTransitionCmd(g, args[1:], "resolved", stdout, stderr)
	case "close":
		return ticketTransitionCmd(g, args[1:], "closed", stdout, stderr)
	case "note":
		return ticketNoteCmd(g, args[1:], stdout, stderr)
	case "assign":
		return ticketAssignCmd(g, args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "error: unknown ticket subcommand %q\n", args[0])
		return 2
	}
}

func ticketListCmd(g globalFlags, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("ticket list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	severity := fs.String("severity", "", "severity filter (critical|major|minor|info)")
	state := fs.String("state", "", "state filter (open|acknowledged|resolved|closed)")
	filter := fs.String("filter", "", "cpath glob")
	pageSize := fs.Int("page-size", 100, "page size")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: cios ticket list [--severity S] [--state S] [--filter G] [--page-size N]")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	q := url.Values{}
	q.Set("page_size", strconv.Itoa(*pageSize))
	if *severity != "" {
		q.Set("severity", *severity)
	}
	if *state != "" {
		q.Set("state", *state)
	}
	if *filter != "" {
		q.Set("filter", *filter)
	}
	c := NewClient(g.server)
	items, status, err := listAll[ticketRow](c, "/v1/tickets", q)
	if err != nil {
		if err.Error() == "pagination overflow (>100 pages)" {
			fmt.Fprintf(stderr, "error: %s\n", err.Error())
			return 1
		}
		return writeTransportErr(err, stderr)
	}
	if status/100 != 2 {
		fmt.Fprintf(stderr, "error: http %d\n", status)
		return 1
	}
	if g.output == "json" || g.output == "yaml" {
		if err := Print(stdout, g.output, items, TableSpec{}); err != nil {
			fmt.Fprintln(stderr, "error: "+err.Error())
			return 2
		}
		return 0
	}
	if len(items) == 0 {
		fmt.Fprintln(stderr, "no tickets")
		return 0
	}
	rows := make([]any, 0, len(items))
	for _, t := range items {
		rows = append(rows, t)
	}
	tbl := TableSpec{
		Columns: []string{"ID", "STATE", "SEVERITY", "ASSET", "TITLE"},
		Row: func(v any) []string {
			t := v.(ticketRow)
			return []string{
				t.ID,
				t.State,
				t.Severity,
				t.AssetPath,
				t.Title,
			}
		},
	}
	if err := Print(stdout, g.output, rows, tbl); err != nil {
		fmt.Fprintln(stderr, "error: "+err.Error())
		return 2
	}
	return 0
}

func ticketGetCmd(g globalFlags, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("ticket get", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprintln(stderr, "usage: cios ticket get <id>") }
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) != 1 {
		fmt.Fprintln(stderr, "usage: cios ticket get <id>")
		return 2
	}
	id := rest[0]
	c := NewClient(g.server)
	status, body, err := c.Do("GET", "/v1/tickets/"+id, nil, nil)
	if err != nil {
		return writeTransportErr(err, stderr)
	}
	if status/100 != 2 {
		return writeHTTPStatus(status, body, stderr)
	}
	if g.output == "json" || g.output == "yaml" {
		_, err := stdout.Write(body)
		if err != nil {
			return 1
		}
		return 0
	}
	var t ticketRow
	if err := json.Unmarshal(body, &t); err != nil {
		fmt.Fprintf(stderr, "error: decode: %s\n", err.Error())
		return 1
	}
	tbl := TableSpec{
		Columns: []string{"ID", "STATE", "SEVERITY", "ASSET", "TITLE", "OPENED", "ACKED", "RESOLVED", "CLOSED"},
		Row: func(v any) []string {
			t := v.(ticketRow)
			return []string{
				t.ID,
				t.State,
				t.Severity,
				t.AssetPath,
				t.Title,
				formatRFC3339String(t.OpenedAt),
				formatRFC3339String(t.AckedAt),
				formatRFC3339String(t.ResolvedAt),
				formatRFC3339String(t.ClosedAt),
			}
		},
	}
	if err := Print(stdout, g.output, []any{t}, tbl); err != nil {
		fmt.Fprintln(stderr, "error: "+err.Error())
		return 2
	}
	return 0
}

func ticketOpenCmd(g globalFlags, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("ticket open", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asset := fs.String("asset", "", "asset path (required)")
	title := fs.String("title", "", "title (required)")
	severity := fs.String("severity", "", "severity (required; critical|major|minor|info)")
	assignee := fs.String("assignee", "", "assignee (optional)")
	alarmID := fs.String("alarm-id", "", "alarm id this ticket was opened from (optional)")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: cios ticket open --asset <crn> --title <t> --severity <s> [--assignee a] [--alarm-id id]")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *asset == "" || *title == "" || *severity == "" {
		fmt.Fprintln(stderr, "usage: cios ticket open --asset <crn> --title <t> --severity <s> [--assignee a]")
		return 2
	}
	body := map[string]any{
		"asset_path": *asset,
		"title":      *title,
		"severity":   *severity,
	}
	if *assignee != "" {
		body["assignee"] = *assignee
	}
	if *alarmID != "" {
		body["alarm_id"] = *alarmID
	}
	c := NewClient(g.server)
	status, respBody, err := c.Do("POST", "/v1/tickets", nil, body)
	if err != nil {
		return writeTransportErr(err, stderr)
	}
	if status/100 != 2 {
		return writeHTTPStatus(status, respBody, stderr)
	}
	if g.output == "json" || g.output == "yaml" {
		_, err := stdout.Write(respBody)
		if err != nil {
			return 1
		}
		return 0
	}
	var t ticketRow
	if err := json.Unmarshal(respBody, &t); err != nil {
		fmt.Fprintf(stderr, "error: decode: %s\n", err.Error())
		return 1
	}
	fmt.Fprintf(stdout, "opened %s (state=open, severity=%s)\n", t.ID, t.Severity)
	return 0
}

// ticketTransitionCmd handles ack/resolve/close — all are POST to
// /v1/tickets/{id}:transition with a static "to" body.
func ticketTransitionCmd(g globalFlags, args []string, to string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("ticket "+to, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprintf(stderr, "usage: cios ticket %s <id>\n", to) }
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) != 1 {
		fmt.Fprintf(stderr, "usage: cios ticket %s <id>\n", to)
		return 2
	}
	id := rest[0]
	c := NewClient(g.server)
	body := map[string]string{"to": to}
	status, respBody, err := c.Do("POST", "/v1/tickets/"+id+":transition", nil, body)
	if err != nil {
		return writeTransportErr(err, stderr)
	}
	if status/100 != 2 {
		return writeHTTPStatus(status, respBody, stderr)
	}
	if g.output == "json" || g.output == "yaml" {
		_, err := stdout.Write(respBody)
		if err != nil {
			return 1
		}
		return 0
	}
	var t ticketRow
	if err := json.Unmarshal(respBody, &t); err != nil {
		fmt.Fprintf(stderr, "error: decode: %s\n", err.Error())
		return 1
	}
	fmt.Fprintf(stdout, "%s %s (state=%s)\n", to, t.ID, t.State)
	return 0
}

// ticketNoteCmd handles `cios ticket note <id> <body>`. POSTs to
// /v1/tickets/{id}:note. PRMT-060 §3.
func ticketNoteCmd(g globalFlags, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("ticket note", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprintln(stderr, "usage: cios ticket note <id> <body>") }
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) != 2 {
		fmt.Fprintln(stderr, "usage: cios ticket note <id> <body>")
		return 2
	}
	id, body := rest[0], rest[1]
	c := NewClient(g.server)
	status, respBody, err := c.Do("POST", "/v1/tickets/"+id+":note", nil, map[string]string{"body": body})
	if err != nil {
		return writeTransportErr(err, stderr)
	}
	if status/100 != 2 {
		return writeHTTPStatus(status, respBody, stderr)
	}
	if g.output == "json" || g.output == "yaml" {
		_, err := stdout.Write(respBody)
		if err != nil {
			return 1
		}
		return 0
	}
	fmt.Fprintf(stdout, "noted %s\n", id)
	return 0
}

// ticketAssignCmd handles `cios ticket assign <id> <user>`. POSTs to
// /v1/tickets/{id}:assign. Empty user means unassign. PRMT-060 §3.
func ticketAssignCmd(g globalFlags, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("ticket assign", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprintln(stderr, "usage: cios ticket assign <id> <user>") }
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) != 2 {
		fmt.Fprintln(stderr, "usage: cios ticket assign <id> <user>")
		return 2
	}
	id, user := rest[0], rest[1]
	c := NewClient(g.server)
	status, respBody, err := c.Do("POST", "/v1/tickets/"+id+":assign", nil, map[string]string{"assignee": user})
	if err != nil {
		return writeTransportErr(err, stderr)
	}
	if status/100 != 2 {
		return writeHTTPStatus(status, respBody, stderr)
	}
	if g.output == "json" || g.output == "yaml" {
		_, err := stdout.Write(respBody)
		if err != nil {
			return 1
		}
		return 0
	}
	if user == "" {
		fmt.Fprintf(stdout, "unassigned %s\n", id)
	} else {
		fmt.Fprintf(stdout, "assigned %s to %s\n", id, user)
	}
	return 0
}
