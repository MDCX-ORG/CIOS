// Package core — metrics.go: /v1/metrics/query[_range] is a white-
// listed pass-through to VictoriaMetrics' Prometheus-compatible
// endpoints; /v1/points/{path} issues a single instant query and
// extracts the first sample's value, ts, and quality. All VM
// reads go through here; nothing in this package caches.
package core

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/yurimeng/cios/pkg/promproj"
)

// vmUpstreamTimeout is the hard cap on a VM call from core. Per
// spec-004 §4: VM unreachable / timeout → 502 upstream-unavailable.
const vmUpstreamTimeout = 5 * time.Second

// whiteListParams for /v1/metrics/query[_range] (spec-004 §2):
//   - query      (mandatory for query; for query_range too)
//   - time       (query only)
//   - start,end,step (query_range only)
//
// Any other query parameter on the inbound request is silently
// dropped, matching spec-004 §2's "白名单透传".
var queryWhitelist = map[string]struct{}{
	"query": {},
	"time":  {},
	"start": {},
	"end":   {},
	"step":  {},
}

// serveMetricsQuery handles both /v1/metrics/query and
// /v1/metrics/query_range. The upstream path is taken from
// r.URL.Path: anything matching /v1/metrics/query goes to
// /api/v1/query, /v1/metrics/query_range → /api/v1/query_range.
func (s *Server) serveMetricsQuery(w http.ResponseWriter, r *http.Request) {
	rid := RequestIDFromContext(r.Context())
	upstream := s.vmURL + "/api/v1/query"
	if r.URL.Path == "/v1/metrics/query_range" {
		upstream = s.vmURL + "/api/v1/query_range"
	}
	// Whitelist inbound params.
	in := r.URL.Query()
	out := url.Values{}
	for k := range in {
		if _, ok := queryWhitelist[k]; ok {
			out.Set(k, in.Get(k))
		}
	}
	// Mandatory: query.
	if out.Get("query") == "" {
		writeProblem(w, http.StatusBadRequest, "bad-request",
			"query is required", "", r.URL.Path, rid)
		return
	}
	if err := s.proxyToVM(w, r, upstream, out); err != nil {
		writeInternalProblem(w, r.Context(), http.StatusBadGateway, "upstream-unavailable",
			"victoria metrics unavailable", err)
	}
}

// proxyToVM does the GET. For 2xx/4xx upstream responses the
// status + Content-Type + body are copied verbatim so the caller
// sees VM's response shape unchanged (spec-004 §4 "白名单透传").
// For 5xx upstream responses the body is NOT passed through — it
// is captured into errUpstreamStatus (for the server-side log) and
// the caller maps that to a scrubbed 502 problem. PRMT-083 §2.
func (s *Server) proxyToVM(w http.ResponseWriter, r *http.Request, upstream string, params url.Values) error {
	ctx, cancel := context.WithTimeout(r.Context(), vmUpstreamTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, upstream+"?"+params.Encode(), nil)
	if err != nil {
		return err
	}
	resp, err := s.vmClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 == 5 {
		// Don't leak upstream body to the public. Read it for
		// logging only; the caller writes a scrubbed 502 problem
		// via writeInternalProblem.
		buf, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		return errUpstreamStatus{status: resp.StatusCode, body: string(buf)}
	}
	for k, vs := range resp.Header {
		// Strip hop-by-hop + encoding headers per net/http spirit;
		// we just copy status, content-type, and the body.
		if k == "Content-Type" || k == "Content-Length" {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, err = io.Copy(w, resp.Body)
	return err
}

// servePointGet handles GET /v1/points/{path}. The path is validated
// with cpath.Dict.ParsePoint; we then ask promproj for the
// selector and run an instant query against VM. The first sample
// is unwrapped into {value, ts, quality}.
// Dispatch (GET vs PUT :set) lives in setctl.go servePoint.
func (s *Server) servePointGet(w http.ResponseWriter, r *http.Request, pt string, rid string) {
	p, err := s.d.ParsePoint(pt)
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "bad-path",
			"bad point path", err.Error(), r.URL.Path, rid)
		return
	}
	sel, err := promproj.Selector(p, s.d)
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "bad-request",
			"selector build", err.Error(), r.URL.Path, rid)
		return
	}
	// Instant query.
	out := url.Values{}
	out.Set("query", sel)
	body, err := s.fetchVM(r.Context(), s.vmURL+"/api/v1/query", out)
	if err != nil {
		writeInternalProblem(w, r.Context(), http.StatusBadGateway, "upstream-unavailable",
			"victoria metrics unavailable", err)
		return
	}
	// VM's instant-query response: {"data":{"resultType":"vector",
	// "result":[{"metric":{...labels...},"value":[<ts>, "<v>"]}]},...},
	// "status":"success"}.
	var vresp struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string            `json:"resultType"`
			Result     []json.RawMessage `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &vresp); err != nil {
		writeProblem(w, http.StatusInternalServerError, "bad-request",
			"vm response shape", err.Error(), r.URL.Path, rid)
		return
	}
	if vresp.Status != "success" || len(vresp.Data.Result) == 0 {
		writeProblem(w, http.StatusNotFound, "path-not-found",
			"no series for point", p.String(), r.URL.Path, rid)
		return
	}
	// Take the first series. It carries the labels (we need
	// quality) and the [ts, value] pair.
	first := vresp.Data.Result[0]
	var series struct {
		Metric map[string]string `json:"metric"`
		Value  []any             `json:"value"`
	}
	if err := json.Unmarshal(first, &series); err != nil {
		writeProblem(w, http.StatusInternalServerError, "bad-request",
			"vm series shape", err.Error(), r.URL.Path, rid)
		return
	}
	if len(series.Value) < 2 {
		writeProblem(w, http.StatusNotFound, "path-not-found",
			"empty value tuple", p.String(), r.URL.Path, rid)
		return
	}
	tsF, _ := series.Value[0].(float64)
	tsMs := int64(tsF * 1000)
	valStr, _ := series.Value[1].(string)
	val, err := strconv.ParseFloat(valStr, 64)
	if err != nil {
		writeInternalProblem(w, r.Context(), http.StatusInternalServerError, "bad-request",
			"vm value not float", errors.New(valStr))
		return
	}
	quality := series.Metric["quality"]
	if quality == "" {
		quality = "good"
	}
	writeJSON(w, http.StatusOK, pointResponse{
		Path:    p.String(),
		Value:   val,
		Ts:      time.UnixMilli(tsMs).UTC().Format(time.RFC3339),
		Quality: quality,
	})
}

type pointResponse struct {
	Path    string  `json:"path"`
	Value   float64 `json:"value"`
	Ts      string  `json:"ts"`
	Quality string  `json:"quality"`
}

// fetchVM is a small helper that returns the upstream body as
// bytes; HTTP-level errors are surfaced to the caller. We do NOT
// parse VM's response here — the caller decides how to interpret
// the shape (metrics.query passes it through, points.* parses it).
func (s *Server) fetchVM(ctx context.Context, upstream string, params url.Values) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, vmUpstreamTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, upstream+"?"+params.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.vmClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		// Not a transport error; surface as upstream HTTP failure
		// so the caller can decide whether to 404 or 502. We
		// return the body so error messages remain useful.
		buf, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		return buf, errUpstreamStatus{status: resp.StatusCode, body: string(buf)}
	}
	return io.ReadAll(io.LimitReader(resp.Body, 1<<16))
}

type errUpstreamStatus struct {
	status int
	body   string
}

func (e errUpstreamStatus) Error() string {
	return "vm status " + strconv.Itoa(e.status) + ": " + e.body
}
