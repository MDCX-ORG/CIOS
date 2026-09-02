package core

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestHTTPControlSink_OK(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/control/set" || r.Method != http.MethodPost {
			t.Errorf("path/method %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		if r.Header.Get("X-CIOS-Control-Token") != "secret" {
			t.Errorf("X-CIOS-Control-Token = %q", r.Header.Get("X-CIOS-Control-Token"))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"accepted": true, "readback": 7.5})
	}))
	defer ts.Close()

	sink := HTTPControlSink{BaseURL: ts.URL, Token: "secret"}
	res, err := sink.DispatchControl(context.Background(), ControlDispatch{
		Path: "sgp01.pod000.cdu000.tcs.opening", Value: 7.5, AuditID: "a1", TTL: 10 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Accepted || res.Readback != 7.5 {
		t.Fatalf("%+v", res)
	}
}
