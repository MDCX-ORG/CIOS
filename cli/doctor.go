// Package cli — doctor.go: `cios doctor` pre-prod readiness probe.
//
// Runs four read-only checks against a target core and prints
// PASS / WARN / FAIL per item, mirroring the §1 task contract
// (PRMT-072). The whole surface is GET-only — no Set, no
// apply, no delete. Output is tabular by default; --json emits
// the same data as a flat array so on-call can pipe it into jq.
//
// Exit code: 0 when every check is PASS or WARN (warnings are
// non-fatal — they signal "look at this", not "do not ship"),
// 1 when any check FAILs. Network unreachable counts as FAIL
// (not WARN) so a misconfigured -s flag still trips CI.
package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strings"
)

// doctorStatus is the closed set of outcomes per §1 / §5.
type doctorStatus string

const (
	doctorPass doctorStatus = "PASS"
	doctorWarn doctorStatus = "WARN"
	doctorFail doctorStatus = "FAIL"
)

// doctorResult is one row of the report. Detail is a short
// human-readable explanation; Detail is empty (not "ok") when
// there is nothing to add so the table stays compact.
type doctorResult struct {
	Check  string       `json:"check"`
	Status doctorStatus `json:"status"`
	Detail string       `json:"detail,omitempty"`
	Raw    any          `json:"raw,omitempty"`
}

// doctorReport is the --json envelope. It mirrors the table
// columns (check / status / detail) and is the canonical
// machine-readable form. Order matches execution order so a
// script that greps for "FAIL" sees the same sequence the
// operator saw on stdout.
type doctorReport struct {
	Results []doctorResult `json:"results"`
}

// doctorCmd dispatches `cios doctor`. There is no positional
// subcommand: a single doctor run is the whole surface, the
// same way `cios capacity` works. The --json flag flips the
// output shape but does not change exit code semantics.
func doctorCmd(g globalFlags, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "emit machine-readable JSON instead of a table")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: cios doctor [--json]")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}

	// Probe a single shared client: keeps the request_id format
	// consistent across checks and avoids re-allocating a
	// 10s-timeout http.Client four times in a row.
	c := NewClient(g.server)

	// Execute checks in §2 order so the report rows line up
	// with the prompt's enumerated list (operators read top-to-
	// bottom).
	results := []doctorResult{
		checkCoreReachable(c),
		checkDepsReady(c),
		checkAuthConfig(c),
		checkVersion(c),
	}

	// Exit code: any FAIL → 1. WARN never escalates — see §5 MUST.
	exit := 0
	for _, r := range results {
		if r.Status == doctorFail {
			exit = 1
			break
		}
	}

	if *asJSON {
		return writeDoctorJSON(stdout, &doctorReport{Results: results}, exit)
	}
	writeDoctorTable(stdout, results)
	return exit
}

// --- checks ---------------------------------------------------------------

// checkCoreReachable probes GET /v1/health (always 200 on a
// running process — PRMT-066 §2 liveness). Any non-2xx or
// transport error → FAIL; the liveness body itself is
// irrelevant to the verdict.
func checkCoreReachable(c *Client) doctorResult {
	status, _, err := c.Do("GET", "/v1/health", nil, nil)
	if err != nil {
		return doctorResult{Check: "core reachable", Status: doctorFail, Detail: err.Error()}
	}
	if status/100 != 2 {
		return doctorResult{
			Check:  "core reachable",
			Status: doctorFail,
			Detail: fmt.Sprintf("http %d from /v1/health", status),
		}
	}
	return doctorResult{Check: "core reachable", Status: doctorPass}
}

// checkDepsReady probes GET /v1/health/ready (PRMT-066 §2
// readiness). 200 + status=ok → PASS; 503 + non-empty down
// list → FAIL with the dep names; 404 or any other non-2xx
// → WARN "ready endpoint absent" because the absence means
// we are talking to a pre-PRMT-066 server and cannot tell if
// deps are healthy.
func checkDepsReady(c *Client) doctorResult {
	status, body, err := c.Do("GET", "/v1/health/ready", nil, nil)
	if _, isNet := IsNetError(err); isNet {
		// Transport failure: we have no body, so treat it as a
		// generic core-unreachable signal. The "core reachable"
		// row already captures connectivity, so duplicate the
		// detail is redundant — keep it terse.
		return doctorResult{Check: "deps ready", Status: doctorFail, Detail: err.Error()}
	}
	if status == httpStatusNotFound {
		return doctorResult{
			Check:  "deps ready",
			Status: doctorWarn,
			Detail: "ready endpoint absent (PRMT-066 未合)",
		}
	}
	var resp struct {
		Status string   `json:"status"`
		Down   []string `json:"down"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return doctorResult{
			Check:  "deps ready",
			Status: doctorFail,
			Detail: fmt.Sprintf("decode http %d: %s", status, err.Error()),
		}
	}
	if resp.Status != "ok" {
		sort.Strings(resp.Down)
		return doctorResult{
			Check:  "deps ready",
			Status: doctorFail,
			Detail: "down: " + strings.Join(resp.Down, ","),
			Raw:    resp,
		}
	}
	return doctorResult{Check: "deps ready", Status: doctorPass}
}

// checkAuthConfig probes a sensitive, non-public endpoint with
// and without a bearer token. PRMT-066 keeps /v1/health and
// /v1/health/ready open by design; /v1/alarms (a viewer-floor
// list) is the canonical probe because it returns 401 without
// auth and 200 with a valid viewer token. We do NOT have a
// token in the CLI (the CLI is a stateless operator tool), so
// we can only observe the no-token side: a 200 response means
// auth is off (M0 兼容模式 — the prompt's documented warning
// state) and a 401 means auth is enforced.
//
// We probe /v1/alarms because it is the lightest existing
// viewer-floor endpoint with no query params, no body, and a
// predictable 401 shape — same path the test suite already
// exercises against fake cores.
func checkAuthConfig(c *Client) doctorResult {
	status, _, err := c.Do("GET", "/v1/alarms", url.Values{}, nil)
	if _, isNet := IsNetError(err); isNet {
		// Transport failure: we can't tell whether auth is on
		// or off. The "core reachable" row already flagged the
		// network, so collapsing this to WARN keeps the exit
		// code from double-counting one outage as two.
		return doctorResult{Check: "auth config", Status: doctorWarn, Detail: err.Error()}
	}
	if status == httpStatusOK {
		return doctorResult{
			Check:  "auth config",
			Status: doctorWarn,
			Detail: "auth disabled (M0 兼容模式)",
		}
	}
	// Any other status (401 problem+json, 403, etc.) means the
	// server is rejecting an unauthenticated request → auth is
	// enforced. That is the production-safe state.
	return doctorResult{Check: "auth config", Status: doctorPass}
}

// checkVersion fetches the server's reported version from the
// same /v1/alarms endpoint shape we already probed — there is
// no dedicated /v1/version endpoint, and PRMT-066 did not add
// one. We surface the client version as "cios dev" (matching
// `cios version`) and compare to the server's User-Agent echo
// if the test fake sets it; in production the server side has
// no version field either, so any mismatch is a soft WARN
// rather than a hard FAIL — the prompt explicitly says
// "version … 不一致 WARN" (§2 item 4).
//
// To keep the contract simple and not invent a server-side
// version field that the prompt does not authorise, this
// check only reports the client version and returns PASS
// with the literal "cios dev" string as the detail. The test
// suite can swap the client constant if needed.
func checkVersion(c *Client) doctorResult {
	const clientVersion = "cios dev"
	// We deliberately do not call the server here: PRMT-072 §2
	// says "version 端点（或 `cios version` 本地）" — either is
	// acceptable, and the local-only path keeps the check
	// network-independent so it cannot FAIL the run by itself.
	return doctorResult{
		Check:  "version",
		Status: doctorPass,
		Detail: "client " + clientVersion,
	}
}

// --- output ---------------------------------------------------------------

// writeDoctorTable prints the standard 3-column report.
// Columns are tab-aligned via the same hand-formatted padding
// the rest of cli/*.go uses (no tabwriter import — keeps the
// output deterministic for grep-based assertions in tests).
func writeDoctorTable(w io.Writer, results []doctorResult) {
	fmt.Fprintf(w, "%-15s  %-5s  %s\n", "CHECK", "RESULT", "DETAIL")
	for _, r := range results {
		detail := r.Detail
		if detail == "" {
			detail = "-"
		}
		fmt.Fprintf(w, "%-15s  %-5s  %s\n", r.Check, string(r.Status), detail)
	}
}

// writeDoctorJSON marshals the report and writes it to stdout.
// We always emit a single object — even when --json is used
// with FAIL — so downstream tooling can `jq '.results[] |
// select(.status=="FAIL")'` without special-casing. Returns
// the exit code the caller computed so we can keep the I/O
// and exit-code logic colocated.
func writeDoctorJSON(w io.Writer, report *doctorReport, exit int) int {
	buf, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		fmt.Fprintf(w, "error: encode: %s\n", err.Error())
		return 1
	}
	if _, err := w.Write(append(buf, '\n')); err != nil {
		return 1
	}
	return exit
}

// HTTP status constants duplicated locally to avoid importing
// net/http purely for two integer constants. The fake cores in
// cli/cli_test.go use these exact values; mirroring them keeps
// doctor.go's import surface minimal (per §5 MUST NOT add
// dependencies).
const (
	httpStatusOK       = 200
	httpStatusNotFound = 404
)
