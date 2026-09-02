package gateway

import (
	"strings"
	"testing"
	"time"
)

func TestPipelineHeartbeatLine(t *testing.T) {
	ts := time.UnixMilli(1720000000123).UTC()
	line := pipelineHeartbeatLine("sgp01", "sgp01.pod000.cdu000", "sgp01.pod000", "cdu", ts)
	if !strings.HasPrefix(line, "cios_pipeline_heartbeat{") {
		t.Fatal(line)
	}
	if !strings.Contains(line, `path="sgp01.pod000.cdu000"`) {
		t.Fatal(line)
	}
	if !strings.HasSuffix(line, " 1 1720000000123") {
		t.Fatal(line)
	}
}

func TestLeafType(t *testing.T) {
	if leafType("sgp01.pod000.cdu000") != "cdu" {
		t.Fatal(leafType("sgp01.pod000.cdu000"))
	}
}
