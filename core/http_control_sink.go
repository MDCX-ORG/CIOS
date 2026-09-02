// Package core — http_control_sink.go: POST accepted Sets to gateway
// control API (P722). Wired from cmd/cios-core -control-url.
package core

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// HTTPControlSink dispatches to gateway POST {base}/v1/control/set.
type HTTPControlSink struct {
	BaseURL    string // e.g. http://127.0.0.1:8092
	Token      string // shared secret (M4 F1); sent as Bearer + X-CIOS-Control-Token
	HTTPClient *http.Client
}

// DispatchControl implements ControlSink.
func (h HTTPControlSink) DispatchControl(ctx context.Context, cmd ControlDispatch) (ControlDispatchResult, error) {
	base := strings.TrimRight(strings.TrimSpace(h.BaseURL), "/")
	if base == "" {
		return ControlDispatchResult{Accepted: false, Detail: "empty control url"}, nil
	}
	client := h.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	body, err := json.Marshal(map[string]any{
		"path":        cmd.Path,
		"value":       cmd.Value,
		"request_id":  cmd.AuditID,
		"ttl_seconds": int(cmd.TTL / time.Second),
	})
	if err != nil {
		return ControlDispatchResult{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/control/set", bytes.NewReader(body))
	if err != nil {
		return ControlDispatchResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if tok := strings.TrimSpace(h.Token); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("X-CIOS-Control-Token", tok)
	}
	resp, err := client.Do(req)
	if err != nil {
		return ControlDispatchResult{}, fmt.Errorf("control post: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	var out struct {
		Accepted bool    `json:"accepted"`
		Readback float64 `json:"readback"`
		Detail   string  `json:"detail"`
	}
	_ = json.Unmarshal(raw, &out)
	if resp.StatusCode/100 != 2 && resp.StatusCode != http.StatusConflict {
		detail := out.Detail
		if detail == "" {
			detail = string(raw)
		}
		return ControlDispatchResult{Accepted: false, Detail: detail},
			fmt.Errorf("control status %d: %s", resp.StatusCode, detail)
	}
	return ControlDispatchResult{
		Accepted: out.Accepted,
		Readback: out.Readback,
		Detail:   out.Detail,
	}, nil
}
