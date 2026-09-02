// Package cli — spare.go: `cios spare list|get|adjust` against
// /v1/spares (M2 E2.5 P541 / PRMT-048). Mirrors `cli/ticket.go`:
// list = paginated GET, get = single GET (with low_stock +
// recent_txns), adjust = POST ...:adjust. create is omitted
// from the CLI per the prompt — the canonical seeding path is
// the apply command, and the catalog is intentionally small.
package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/url"
	"strconv"
)

// spareRow is the table row shape for the list endpoint. JSON
// tags mirror SparePart so json/yaml pass-through works.
type spareRow struct {
	ID       string `json:"id"`
	SKU      string `json:"sku"`
	Name     string `json:"name"`
	Qty      int    `json:"qty"`
	MinQty   int    `json:"min_qty"`
	Location string `json:"location"`
}

// spareWithDerivedRow mirrors sparePartWithDerived on the wire;
// the recent_txns and low_stock fields are extra columns the
// `get` command renders.
type spareWithDerivedRow struct {
	ID       string `json:"id"`
	SKU      string `json:"sku"`
	Name     string `json:"name"`
	Qty      int    `json:"qty"`
	MinQty   int    `json:"min_qty"`
	Location string `json:"location"`
	LowStock bool   `json:"low_stock"`
	Recent   []struct {
		ID       string `json:"id"`
		SpareID  string `json:"spare_id"`
		Delta    int    `json:"delta"`
		TicketID string `json:"ticket_id,omitempty"`
		At       string `json:"at"`
	} `json:"recent_txns,omitempty"`
}

func spareCmd(g globalFlags, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: cios spare <list|get|adjust> ...")
		return 2
	}
	switch args[0] {
	case "list":
		return spareListCmd(g, args[1:], stdout, stderr)
	case "get":
		return spareGetCmd(g, args[1:], stdout, stderr)
	case "adjust":
		return spareAdjustCmd(g, args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "error: unknown spare subcommand %q\n", args[0])
		return 2
	}
}

func spareListCmd(g globalFlags, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("spare list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	pageSize := fs.Int("page-size", 100, "page size")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: cios spare list [--page-size N]")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	q := url.Values{}
	q.Set("page_size", strconv.Itoa(*pageSize))
	c := NewClient(g.server)
	items, status, err := listAll[spareRow](c, "/v1/spares", q)
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
		fmt.Fprintln(stderr, "no spares")
		return 0
	}
	rows := make([]any, 0, len(items))
	for _, s := range items {
		rows = append(rows, s)
	}
	tbl := TableSpec{
		Columns: []string{"ID", "SKU", "NAME", "QTY", "MIN", "LOCATION", "LOW?"},
		Row: func(v any) []string {
			r := v.(spareRow)
			return []string{
				r.ID,
				r.SKU,
				r.Name,
				strconv.Itoa(r.Qty),
				strconv.Itoa(r.MinQty),
				r.Location,
				lowStockMark(r.Qty, r.MinQty),
			}
		},
	}
	if err := Print(stdout, g.output, rows, tbl); err != nil {
		fmt.Fprintln(stderr, "error: "+err.Error())
		return 2
	}
	return 0
}

func lowStockMark(qty, min int) string {
	if qty < min {
		return "LOW"
	}
	return ""
}

func spareGetCmd(g globalFlags, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("spare get", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprintln(stderr, "usage: cios spare get <id>") }
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) != 1 {
		fmt.Fprintln(stderr, "usage: cios spare get <id>")
		return 2
	}
	id := rest[0]
	c := NewClient(g.server)
	status, body, err := c.Do("GET", "/v1/spares/"+id, nil, nil)
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
	var got spareWithDerivedRow
	if err := json.Unmarshal(body, &got); err != nil {
		fmt.Fprintf(stderr, "error: decode: %s\n", err.Error())
		return 1
	}
	printSpareDetail(stdout, &got)
	return 0
}

func printSpareDetail(w io.Writer, r *spareWithDerivedRow) {
	fmt.Fprintf(w, "ID       %s\n", r.ID)
	fmt.Fprintf(w, "SKU      %s\n", r.SKU)
	fmt.Fprintf(w, "NAME     %s\n", r.Name)
	fmt.Fprintf(w, "QTY      %d  (min %d)\n", r.Qty, r.MinQty)
	if r.Location != "" {
		fmt.Fprintf(w, "LOCATION %s\n", r.Location)
	}
	if r.LowStock {
		fmt.Fprintln(w, "LOW      yes")
	}
	if len(r.Recent) > 0 {
		fmt.Fprintln(w, "RECENT")
		for _, t := range r.Recent {
			fmt.Fprintf(w, "  %s  delta=%+d  ticket=%s\n", t.At, t.Delta, t.TicketID)
		}
	}
}

func spareAdjustCmd(g globalFlags, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("spare adjust", flag.ContinueOnError)
	fs.SetOutput(stderr)
	delta := fs.Int("delta", 0, "stock delta (positive = inbound, negative = outbound)")
	ticketID := fs.String("ticket", "", "ticket id (optional; for outbound consumption)")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: cios spare adjust <id> --delta N [--ticket tk_...]")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) != 1 {
		fmt.Fprintln(stderr, "usage: cios spare adjust <id> --delta N [--ticket tk_...]")
		return 2
	}
	if *delta == 0 {
		fmt.Fprintln(stderr, "error: --delta must be non-zero")
		return 2
	}
	id := rest[0]
	body := map[string]any{"delta": *delta}
	if *ticketID != "" {
		body["ticket_id"] = *ticketID
	}
	c := NewClient(g.server)
	status, respBody, err := c.Do("POST", "/v1/spares/"+id+":adjust", nil, body)
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
	var got spareWithDerivedRow
	if err := json.Unmarshal(respBody, &got); err != nil {
		fmt.Fprintf(stderr, "error: decode: %s\n", err.Error())
		return 1
	}
	fmt.Fprintf(stdout, "%s adjusted: qty=%d\n", got.ID, got.Qty)
	return 0
}
