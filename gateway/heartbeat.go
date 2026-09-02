// Package gateway — heartbeat.go: pipeline freshness samples (DATA-RESILIENCE G6).
//
// Each device batch carries a cios_pipeline_heartbeat line so VM can
// answer "was this asset heard in the last N minutes?" without
// inventing sensor values. Not protocol-dictionary projected —
// intentionally outside cios_<quantity>_<unit> naming so cios-alarm
// skips it as an unknown quantity while VM still stores it.
package gateway

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// pipelineHeartbeatLine builds one Prometheus exposition line for the
// asset. Value is always 1; timestamp is wall-clock millis of the tick.
func pipelineHeartbeatLine(site, asset, topAsset, assetType string, ts time.Time) string {
	if assetType == "" {
		assetType = leafType(asset)
	}
	if topAsset == "" {
		topAsset = topAssetOf(asset)
	}
	if site == "" {
		site = siteOf(asset)
	}
	var b strings.Builder
	b.WriteString("cios_pipeline_heartbeat{")
	b.WriteString(`site="`)
	b.WriteString(escapePromLabel(site))
	b.WriteString(`",path="`)
	b.WriteString(escapePromLabel(asset))
	b.WriteString(`",top_asset="`)
	b.WriteString(escapePromLabel(topAsset))
	b.WriteString(`",asset_type="`)
	b.WriteString(escapePromLabel(assetType))
	b.WriteString(`"} 1 `)
	b.WriteString(strconv.FormatInt(ts.UTC().UnixMilli(), 10))
	return b.String()
}

func leafType(asset string) string {
	seg := asset
	if i := strings.LastIndex(asset, "."); i >= 0 {
		seg = asset[i+1:]
	}
	// strip trailing digits: cdu000 → cdu
	i := len(seg)
	for i > 0 && seg[i-1] >= '0' && seg[i-1] <= '9' {
		i--
	}
	if i == 0 {
		return seg
	}
	return seg[:i]
}

func siteOf(asset string) string {
	if i := strings.Index(asset, "."); i > 0 {
		return asset[:i]
	}
	return asset
}

func escapePromLabel(s string) string {
	// Minimal escape for \ and " (Prometheus label rules).
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	return s
}

// appendHeartbeat adds the heartbeat line if not already present.
func appendHeartbeat(lines []string, site, asset, topAsset, assetType string, ts time.Time) []string {
	line := pipelineHeartbeatLine(site, asset, topAsset, assetType, ts)
	for _, l := range lines {
		if strings.HasPrefix(l, "cios_pipeline_heartbeat{") && strings.Contains(l, fmt.Sprintf(`path="%s"`, escapePromLabel(asset))) {
			return lines
		}
	}
	return append(lines, line)
}
