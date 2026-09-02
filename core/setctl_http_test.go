package core

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/yurimeng/cios/pkg/cpath"
)

// TestServePointSet_HTTP exercises P722 policy over the real HTTP surface
// (no southbound driver yet — Accepted + audit only).
func TestServePointSet_HTTP(t *testing.T) {
	dict, err := cpath.LoadDict(filepath.Join(moduleRoot(t), "protocol"))
	if err != nil {
		t.Fatalf("LoadDict: %v", err)
	}
	st, err := NewFileStore(filepath.Join(t.TempDir(), "s.json"))
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServerWithStore(st, dict, "http://127.0.0.1:9", nil)
	h := srv.Handler()

	// ro default: path not on L108
	body, _ := json.Marshal(SetRequest{Value: 1, TTLSeconds: 30})
	req := httptest.NewRequest(http.MethodPut, "/v1/points/sgp01.pod000.cdu000.fws.supply.flow:set", bytes.NewReader(body))
	req = asPrincipal(req, ciOperator)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("ro point: status=%d body=%s", rec.Code, rec.Body.String())
	}

	// class A: two-phase (PRMT-235) — no immediate execution
	pathA := "sgp01.pod000.chiller000.compressor000.status"
	req = httptest.NewRequest(http.MethodPut, "/v1/points/"+pathA+":set", bytes.NewReader(body))
	req = asPrincipal(req, ciOperator)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("A pending: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var pendResp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &pendResp); err != nil {
		t.Fatal(err)
	}
	if pendResp["status"] != "pending" || pendResp["risk_class"] != "a" {
		t.Fatalf("pend resp=%v", pendResp)
	}
	pendingID, _ := pendResp["pending_id"].(string)
	if pendingID == "" {
		t.Fatalf("no pending_id: %v", pendResp)
	}

	// second bearer approves → executed
	req = httptest.NewRequest(http.MethodPost, "/v1/control/"+pendingID+":approve", nil)
	req = asPrincipal(req, ciAdmin)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("approve: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["risk_class"] != "a" || resp["dispatched"] != false {
		t.Fatalf("resp=%v", resp)
	}
	audits, err := st.ListSetAudits(context.Background())
	if err != nil {
		t.Fatalf("ListSetAudits: %v", err)
	}
	if len(audits) != 2 {
		t.Fatalf("audits=%+v", audits)
	}
	execRows := 0
	for _, a := range audits {
		if a.Second == ciAdmin {
			execRows++
			if a.Actor != ciOperator || a.Path != pathA {
				t.Fatalf("exec audit=%+v", a)
			}
		}
	}
	if execRows != 1 {
		t.Fatalf("exec rows=%d audits=%+v", execRows, audits)
	}

	// class B needs readback
	pathB := "sgp01.pod000.cdu000.valve000.opening"
	bodyB, _ := json.Marshal(SetRequest{Value: 40, TTLSeconds: 10, RequireReadback: true})
	req = httptest.NewRequest(http.MethodPut, "/v1/points/"+pathB+":set", bytes.NewReader(bodyB))
	req = asPrincipal(req, ciOperator)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("B: status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestServePointSet_DispatchesToSink(t *testing.T) {
	dict, err := cpath.LoadDict(filepath.Join(moduleRoot(t), "protocol"))
	if err != nil {
		t.Fatal(err)
	}
	st, err := NewFileStore(filepath.Join(t.TempDir(), "s.json"))
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServerWithStore(st, dict, "http://127.0.0.1:9", nil)
	sink := &RecordingControlSink{
		Result: &ControlDispatchResult{
			Accepted: true, Readback: 42, Detail: "ok",
			ReadbackTs: time.Now().UTC(),
		},
	}
	srv.SetControlSink(sink)
	h := srv.Handler()

	pathA := "sgp01.pod000.chiller000.compressor000.status"
	bodyA, _ := json.Marshal(SetRequest{Value: 1, TTLSeconds: 30})
	req := httptest.NewRequest(http.MethodPut, "/v1/points/"+pathA+":set", bytes.NewReader(bodyA))
	req = asPrincipal(req, ciOperator)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("set: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var pendResp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &pendResp)
	if pendResp["status"] != "pending" {
		t.Fatalf("want pending, resp=%v", pendResp)
	}
	sink.mu.Lock()
	if n := len(sink.Cmds); n != 0 {
		sink.mu.Unlock()
		t.Fatalf("sink called before approval: %d", n)
	}
	sink.mu.Unlock()

	pendingID, _ := pendResp["pending_id"].(string)
	req = httptest.NewRequest(http.MethodPost, "/v1/control/"+pendingID+":approve", nil)
	req = asPrincipal(req, ciAdmin)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("approve: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["dispatched"] != true {
		t.Fatalf("dispatched=%v resp=%v", resp["dispatched"], resp)
	}
	if resp["readback_value"] != float64(42) {
		t.Fatalf("readback_value=%v", resp["readback_value"])
	}
	sink.mu.Lock()
	n := len(sink.Cmds)
	sink.mu.Unlock()
	if n != 1 {
		t.Fatalf("sink cmds=%d", n)
	}
}

func TestServePointSet_WrongMethod(t *testing.T) {
	dict, err := cpath.LoadDict(filepath.Join(moduleRoot(t), "protocol"))
	if err != nil {
		t.Fatal(err)
	}
	st, err := NewFileStore(filepath.Join(t.TempDir(), "s.json"))
	if err != nil {
		t.Fatal(err)
	}
	h := NewServerWithStore(st, dict, "http://127.0.0.1:9", nil).Handler()
	req := httptest.NewRequest(http.MethodPost, "/v1/points/sgp01.pod000.cdu000.pump.rpm:set", bytes.NewReader([]byte(`{}`)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status=%d", rec.Code)
	}
}
