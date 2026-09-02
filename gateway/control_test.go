package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/yurimeng/cios/pkg/driver"
)

type stubCtrlDriver struct {
	last driver.ControlCommand
	ok   bool
}

func (s *stubCtrlDriver) Init(context.Context, driver.DriverConfig) error { return nil }
func (s *stubCtrlDriver) Discover(context.Context) ([]driver.AssetCandidate, error) {
	return nil, driver.ErrNotSupported
}
func (s *stubCtrlDriver) Collect(context.Context) ([]driver.Sample, error) { return nil, nil }
func (s *stubCtrlDriver) Subscribe(context.Context, chan<- driver.Sample) error {
	return driver.ErrNotSupported
}
func (s *stubCtrlDriver) Write(_ context.Context, cmd driver.ControlCommand) (driver.ControlResult, error) {
	s.last = cmd
	return driver.ControlResult{Accepted: s.ok, Readback: cmd.Value}, nil
}
func (s *stubCtrlDriver) Health(context.Context) driver.DriverHealth {
	return driver.DriverHealth{Connected: true}
}

func TestControlRegistry_WriteRelativePoint(t *testing.T) {
	reg := newControlRegistry()
	d := &stubCtrlDriver{ok: true}
	reg.register("sgp01.pod000.cdu000", "tcs.opening", d)
	res, err := reg.write(context.Background(), "sgp01.pod000.cdu000.tcs.opening", 55, "rid1", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Accepted || d.last.Point != "tcs.opening" || d.last.Value != 55 {
		t.Fatalf("res=%+v last=%+v", res, d.last)
	}
	_, err = reg.write(context.Background(), "missing.path", 1, "r", time.Second)
	if err == nil {
		t.Fatal("want unknown point error")
	}
}

func TestControlListenLoopback(t *testing.T) {
	if err := controlListenLoopback("127.0.0.1:8092"); err != nil {
		t.Fatal(err)
	}
	if err := controlListenLoopback("localhost:1"); err != nil {
		t.Fatal(err)
	}
	if err := controlListenLoopback("0.0.0.0:8092"); err == nil {
		t.Fatal("want reject all-interfaces")
	}
	if err := controlListenLoopback(":8092"); err == nil {
		t.Fatal("want reject empty host")
	}
}

func TestControlServer_RequiresToken(t *testing.T) {
	reg := newControlRegistry()
	d := &stubCtrlDriver{ok: true}
	reg.register("sgp01.pod000.cdu000", "tcs.opening", d)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	port := freeTCPPort(t)
	addr := "127.0.0.1:" + port
	const tok = "lab-control-secret"
	if err := startControlServer(ctx, addr, tok, reg); err != nil {
		t.Fatal(err)
	}
	waitControlHealth(t, addr)

	body, _ := json.Marshal(map[string]any{
		"path": "sgp01.pod000.cdu000.tcs.opening", "value": 12.0, "ttl_seconds": 10, "request_id": "t1",
	})

	// No token → 401
	resp, err := http.Post("http://"+addr+"/v1/control/set", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token: status=%d want 401", resp.StatusCode)
	}

	// Bearer OK
	req, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/v1/control/set", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("bearer status=%d", resp.StatusCode)
	}
	var out controlSetResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if !out.Accepted || out.Readback != 12 {
		t.Fatalf("out=%+v", out)
	}
	if d.last.Point != "tcs.opening" {
		t.Fatalf("driver point=%q", d.last.Point)
	}
}

func TestControlServer_RefuseNoTokenConfig(t *testing.T) {
	err := startControlServer(context.Background(), "127.0.0.1:1", "", newControlRegistry())
	if err == nil {
		t.Fatal("want error when token empty")
	}
}

func TestControlServer_RefuseNonLoopback(t *testing.T) {
	err := startControlServer(context.Background(), "0.0.0.0:8092", "tok", newControlRegistry())
	if err == nil {
		t.Fatal("want loopback error")
	}
}

func waitControlHealth(t *testing.T, addr string) {
	t.Helper()
	for i := 0; i < 40; i++ {
		resp, err := http.Get("http://" + addr + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("control server not ready")
}

func freeTCPPort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return port
}
