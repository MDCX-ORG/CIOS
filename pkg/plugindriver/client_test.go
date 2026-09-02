package plugindriver_test

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/yurimeng/cios/pkg/driver"
	"github.com/yurimeng/cios/pkg/driver/modbussim"
	"github.com/yurimeng/cios/pkg/plugindriver"
	driverproto "github.com/yurimeng/cios/proto"
)

// moduleRoot walks up from this file until it finds go.mod, so the
// test can locate the protocol/ dictionary + deploy/edge fixtures
// regardless of the working directory `go test` was launched from.
func moduleRoot(t *testing.T) string {
	t.Helper()
	_, here, _, _ := runtime.Caller(0)
	dir := filepath.Dir(here)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("module root not found from %s", here)
		}
		dir = parent
	}
}

// TestPluginRoundtrip is the spec-005 §4 + PRMT-017 §4.9 integration
// check: build the modbus plugin binary, launch it via the gateway-
// side plugindriver.Client, point it at a modbussim, and confirm the
// driver.Driver contract round-trips intact across the gRPC
// channel — same point names, same value count, no suspect samples,
// healthy after a successful Collect.
//
// We deliberately skip t.Parallel(): the test forks a real
// subprocess + opens a real loopback socket, so isolation already
// costs enough wall-clock that we don't want N copies fighting over
// the same kernel resources.
func TestPluginRoundtrip(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping plugin roundtrip in -short mode")
	}

	root := moduleRoot(t)
	pointmapPath := filepath.Join(root, "deploy", "edge", "pointmaps", "cdu-sim.yaml")
	protocolDir := filepath.Join(root, "protocol")
	for _, p := range []string{pointmapPath, protocolDir} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("fixture missing: %s: %v", p, err)
		}
	}

	// 1) Build the plugin binary into a tmp dir. `go build` reuses
	//    the module cache so this is fast (~1s on a warm cache).
	tmpDir := t.TempDir()
	binPath := filepath.Join(tmpDir, "cios-modbus-driver")
	buildCtx, buildCancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer buildCancel()
	buildCmd := exec.CommandContext(buildCtx, "go", "build", "-o", binPath, "./cmd/cios-modbus-driver")
	buildCmd.Dir = root
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("build plugin: %v\n%s", err, out)
	}

	// 2) Start a modbussim populated with the same register/value
	//    pairs cios-modbussim uses for the M0 demo (cdu-sim.yaml).
	holding := map[uint16]uint16{
		0x0020: 45,
	}
	input := map[uint16]uint16{
		0x0010: 235,
		0x0011: 198,
		0x0012: 1200,
		0x0030: 1,
		0x0031: 0,
	}
	sim := modbussim.New(modbussim.Config{
		Listen:  "127.0.0.1:0",
		UnitID:  1,
		Holding: holding,
		Input:   input,
	})
	addr, err := sim.Start()
	if err != nil {
		t.Fatalf("modbussim start: %v", err)
	}
	defer sim.Stop()

	// 3) Launch the plugin via NewClientFromCmd so we can inject the
	//    -pointmap and -protocol-dir flags the binary requires.
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binPath,
		"-pointmap", pointmapPath,
		"-protocol-dir", protocolDir,
	)
	client, err := plugindriver.NewClientFromCmd(cmd)
	if err != nil {
		t.Fatalf("NewClientFromCmd: %v", err)
	}
	defer client.Kill()

	// 4) Init: hand the runtime endpoint + unit_id over the wire.
	if err := client.Init(ctx, driver.DriverConfig{
		Endpoint: addr,
		Options:  map[string]string{"unit_id": "1"},
	}); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// 5) Collect: expect six samples (one per pointmap entry), all
	//    good quality, and the values to match the sim's registers.
	samples, err := client.Collect(ctx)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(samples) != 6 {
		t.Fatalf("Collect samples len = %d, want 6 (cdu-sim.yaml has 6 points)", len(samples))
	}
	wantValues := map[string]float64{
		"fws.supply.temp": 235,
		"fws.return.temp": 198,
		"fws.supply.flow": 1200,
		"tcs.opening":     45,
		"status":          1,
		"leak":            0,
	}
	for _, s := range samples {
		if s.Quality != driver.QualityGood {
			t.Errorf("sample %q quality = %q, want good", s.Point, s.Quality)
		}
		want, ok := wantValues[s.Point]
		if !ok {
			t.Errorf("unexpected sample point %q", s.Point)
			continue
		}
		if s.Value != want {
			t.Errorf("sample %q value = %v, want %v", s.Point, s.Value, want)
		}
		if s.Ts.IsZero() {
			t.Errorf("sample %q has zero Ts", s.Point)
		}
	}

	// 6) Health: a successful Collect must have stamped LastSuccess
	//    and left the driver Connected.
	h := client.Health(ctx)
	if !h.Connected {
		t.Errorf("Health Connected = false; detail=%q", h.Detail)
	}
	if h.LastSuccess.IsZero() {
		t.Errorf("Health LastSuccess is zero after a good Collect")
	}
}

// TestNonPluginInvocationExits verifies the spec-005 §1 safeguard
// from §4.6: launching the plugin binary outside the go-plugin
// handshake (no magic cookie) must print a usage hint and exit
// non-zero rather than fall through to plugin.Serve. A misconfigured
// systemd unit that runs the binary directly would otherwise stall
// forever waiting for a stdin protocol.
func TestNonPluginInvocationExits(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping non-plugin invocation test in -short mode")
	}

	root := moduleRoot(t)
	tmpDir := t.TempDir()
	binPath := filepath.Join(tmpDir, "cios-modbus-driver")
	buildCtx, buildCancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer buildCancel()
	buildCmd := exec.CommandContext(buildCtx, "go", "build", "-o", binPath, "./cmd/cios-modbus-driver")
	buildCmd.Dir = root
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("build plugin: %v\n%s", err, out)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Strip the magic cookie env var explicitly so the parent
	// process's env (if it happens to set CIOS_DRIVER_PLUGIN) does
	// not slip through.
	cmd := exec.CommandContext(ctx, binPath)
	cmd.Env = filteredEnv(os.Environ(), plugindriver.HandshakeConfig.MagicCookieKey)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected non-zero exit, got success; output: %s", out)
	}
	if !strings.Contains(string(out), "go-plugin") {
		t.Errorf("expected usage hint containing 'go-plugin', got: %s", out)
	}
}

// filteredEnv returns env with any KEY=val pair for the given key
// removed. Used by TestNonPluginInvocationExits to make sure we
// don't accidentally pass the magic cookie into the child.
func filteredEnv(env []string, key string) []string {
	prefix := key + "="
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// TestSamplesRoundTrip is a unit-level check on types.go: every
// Sample/DriverConfig/ControlCommand/ControlResult/DriverHealth
// must survive a toProto/fromProto round-trip with the same
// observable fields. We avoid Date.Now() inside the table by using
// a fixed UTC time so the test is hermetic and the equality check
// is meaningful.
func TestSamplesRoundTrip(t *testing.T) {
	fixedTs := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)

	t.Run("Sample", func(t *testing.T) {
		orig := driver.Sample{Point: "p", Value: 1.5, Ts: fixedTs, Quality: driver.QualityGood}
		got := plugindriver.SampleFromProto(plugindriver.SampleToProto(orig))
		if got.Point != orig.Point || got.Value != orig.Value || got.Quality != orig.Quality || !got.Ts.Equal(orig.Ts) {
			t.Errorf("Sample roundtrip mismatch: %+v vs %+v", got, orig)
		}
	})

	t.Run("DriverConfig", func(t *testing.T) {
		orig := driver.DriverConfig{Endpoint: "127.0.0.1:502", Options: map[string]string{"unit_id": "1"}}
		got := plugindriver.DriverConfigFromProto(plugindriver.DriverConfigToProto(orig))
		if got.Endpoint != orig.Endpoint || got.Options["unit_id"] != orig.Options["unit_id"] {
			t.Errorf("DriverConfig roundtrip mismatch: %+v vs %+v", got, orig)
		}
	})

	t.Run("ControlCommand", func(t *testing.T) {
		orig := driver.ControlCommand{Point: "p", Value: 42, RequestID: "r1", TTL: 5 * time.Second}
		got := plugindriver.ControlCommandFromProto(plugindriver.ControlCommandToProto(orig))
		if got.Point != orig.Point || got.Value != orig.Value || got.RequestID != orig.RequestID || got.TTL != orig.TTL {
			t.Errorf("ControlCommand roundtrip mismatch: %+v vs %+v", got, orig)
		}
	})

	t.Run("ControlResult", func(t *testing.T) {
		orig := driver.ControlResult{Accepted: true, Readback: 99, ReadbackTs: fixedTs}
		got := plugindriver.ControlResultFromProto(plugindriver.ControlResultToProto(orig))
		if got.Accepted != orig.Accepted || got.Readback != orig.Readback || !got.ReadbackTs.Equal(orig.ReadbackTs) {
			t.Errorf("ControlResult roundtrip mismatch: %+v vs %+v", got, orig)
		}
	})

	t.Run("DriverHealth", func(t *testing.T) {
		orig := driver.DriverHealth{Connected: true, LastSuccess: fixedTs, ErrorCount: 7, Detail: "ok"}
		got := plugindriver.DriverHealthFromProto(plugindriver.DriverHealthToProto(orig))
		if got.Connected != orig.Connected || got.ErrorCount != orig.ErrorCount || got.Detail != orig.Detail || !got.LastSuccess.Equal(orig.LastSuccess) {
			t.Errorf("DriverHealth roundtrip mismatch: %+v vs %+v", got, orig)
		}
	})
}

// fakeDriver is a minimal driver.Driver implementation used by the
// in-process RPC tests below. The behaviours it advertises are
// pinned per-test by the field functions — that keeps each
// subscribe/discover scenario from spilling into the next.
type fakeDriver struct {
	subscribe func(ctx context.Context, ch chan<- driver.Sample) error
	discover  func(ctx context.Context) ([]driver.AssetCandidate, error)
}

func (f *fakeDriver) Init(_ context.Context, _ driver.DriverConfig) error { return nil }
func (f *fakeDriver) Discover(ctx context.Context) ([]driver.AssetCandidate, error) {
	if f.discover != nil {
		return f.discover(ctx)
	}
	return nil, driver.ErrNotSupported
}
func (f *fakeDriver) Collect(_ context.Context) ([]driver.Sample, error) { return nil, nil }
func (f *fakeDriver) Subscribe(ctx context.Context, ch chan<- driver.Sample) error {
	if f.subscribe != nil {
		return f.subscribe(ctx, ch)
	}
	return driver.ErrNotSupported
}
func (f *fakeDriver) Write(_ context.Context, _ driver.ControlCommand) (driver.ControlResult, error) {
	return driver.ControlResult{}, nil
}
func (f *fakeDriver) Health(_ context.Context) driver.DriverHealth {
	return driver.DriverHealth{Connected: true}
}

// startInProcessServer wires a Server backed by impl onto a bufconn
// listener and returns the matching DriverServiceClient. The
// returned cleanup func tears everything down; t.Cleanup is the
// expected caller. We use bufconn (not a real socket) because
// PRMT-017 R2 only needs to verify the proto-level error/type
// plumbing, not anything socket-related.
func startInProcessServer(t *testing.T, impl driver.Driver) (driverproto.DriverServiceClient, func()) {
	t.Helper()
	lis := bufconn.Listen(1 << 16)
	gsrv := grpc.NewServer()
	driverproto.RegisterDriverServiceServer(gsrv, plugindriver.NewServer(impl))
	go func() {
		_ = gsrv.Serve(lis)
	}()
	dialer := func(_ context.Context, _ string) (net.Conn, error) { return lis.Dial() }
	conn, err := grpc.NewClient("passthrough://bufnet",
		grpc.WithContextDialer(dialer),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		gsrv.Stop()
		_ = lis.Close()
		t.Fatalf("dial bufconn: %v", err)
	}
	cleanup := func() {
		_ = conn.Close()
		gsrv.Stop()
		_ = lis.Close()
	}
	return driverproto.NewDriverServiceClient(conn), cleanup
}

// TestDiscoverTypeRoundtrip verifies the PRMT-017 R2 fix that adds
// AssetCandidateProto.type: AssetCandidate.Type now survives the
// proto round-trip instead of decoding back as empty. We drive the
// server-side Discover directly (rather than via the typed
// plugindriver.Client) because we only care about the wire
// representation here, not the gateway plumbing.
func TestDiscoverTypeRoundtrip(t *testing.T) {
	impl := &fakeDriver{
		discover: func(_ context.Context) ([]driver.AssetCandidate, error) {
			return []driver.AssetCandidate{
				{Type: "cdu", Serial: "abc-123", Hints: map[string]string{"firmware": "1.2.3"}},
				{Type: "ats", Serial: "xyz-789"},
			}, nil
		},
	}
	stub, cleanup := startInProcessServer(t, impl)
	t.Cleanup(cleanup)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := stub.Discover(ctx, &driverproto.DiscoverRequest{})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if got := len(resp.GetCandidates()); got != 2 {
		t.Fatalf("Candidates len = %d, want 2", got)
	}
	if got := resp.Candidates[0].GetType(); got != "cdu" {
		t.Errorf("Candidates[0].Type = %q, want %q", got, "cdu")
	}
	if got := resp.Candidates[1].GetType(); got != "ats" {
		t.Errorf("Candidates[1].Type = %q, want %q", got, "ats")
	}
	if got := resp.Candidates[0].GetMeta()["firmware"]; got != "1.2.3" {
		t.Errorf("Candidates[0].Meta[firmware] = %q, want 1.2.3", got)
	}
}

// TestDiscoverErrNotSupportedWire verifies that a fakeDriver
// returning driver.ErrNotSupported from Discover is observable via
// the wire's error field with the magic sentinel string. This is
// the precondition for client.Discover's wireError mapping to work.
func TestDiscoverErrNotSupportedWire(t *testing.T) {
	impl := &fakeDriver{} // default Discover → ErrNotSupported
	stub, cleanup := startInProcessServer(t, impl)
	t.Cleanup(cleanup)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := stub.Discover(ctx, &driverproto.DiscoverRequest{})
	if err != nil {
		t.Fatalf("Discover transport err: %v", err)
	}
	if resp.GetError() != "ErrNotSupported" {
		t.Errorf("Discover error field = %q, want %q", resp.GetError(), "ErrNotSupported")
	}
}

// TestSubscribeErrNotSupportedIsTyped is the headline PRMT-017 R2
// fix: a driver returning driver.ErrNotSupported from Subscribe must
// surface, on the client side, as an error matching
// errors.Is(err, driver.ErrNotSupported). The v1 schema lost the
// typed sentinel because it had no SubscribeResponse.error field;
// the v2 schema (this round) carries it through.
//
// We use the full plugindriver.Client path (not the bufconn shim)
// because the change touches both the server's Send-trailing-error
// behaviour and the client's per-message error inspection — both
// sides need to agree, and the in-process Server/Client pair is the
// cheapest way to drive both halves of that contract.
func TestSubscribeErrNotSupportedIsTyped(t *testing.T) {
	impl := &fakeDriver{} // default Subscribe → ErrNotSupported
	stub, cleanup := startInProcessServer(t, impl)
	t.Cleanup(cleanup)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := stub.Subscribe(ctx, &driverproto.SubscribeRequest{})
	if err != nil {
		t.Fatalf("Subscribe call: %v", err)
	}
	msg, err := stream.Recv()
	if err != nil {
		t.Fatalf("first Recv: %v", err)
	}
	if got := msg.GetError(); got != "ErrNotSupported" {
		t.Errorf("first message error field = %q, want %q", got, "ErrNotSupported")
	}
	// A second Recv should hit EOF because the server returned nil
	// after the trailing error response.
	if _, err := stream.Recv(); err == nil {
		t.Errorf("second Recv returned no err; want io.EOF or stream end")
	}
}

// TestClientSubscribeMapsErrNotSupported drives the client-side
// Subscribe wrapper end-to-end: it must return an error that
// errors.Is(err, driver.ErrNotSupported) matches. This is the
// behaviour callers rely on; everything below is plumbing.
func TestClientSubscribeMapsErrNotSupported(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in -short mode")
	}

	root := moduleRoot(t)
	tmpDir := t.TempDir()
	binPath := filepath.Join(tmpDir, "cios-modbus-driver")
	buildCtx, buildCancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer buildCancel()
	buildCmd := exec.CommandContext(buildCtx, "go", "build", "-o", binPath, "./cmd/cios-modbus-driver")
	buildCmd.Dir = root
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("build plugin: %v\n%s", err, out)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	pointmapPath := filepath.Join(root, "deploy", "edge", "pointmaps", "cdu-sim.yaml")
	cmd := exec.CommandContext(ctx, binPath,
		"-pointmap", pointmapPath,
		"-protocol-dir", filepath.Join(root, "protocol"),
	)
	client, err := plugindriver.NewClientFromCmd(cmd)
	if err != nil {
		t.Fatalf("NewClientFromCmd: %v", err)
	}
	t.Cleanup(client.Kill)

	// modbus.Driver.Subscribe is ErrNotSupported by contract, so we
	// can use the real plugin binary here without needing a fake.
	ch := make(chan driver.Sample, 1)
	err = client.Subscribe(ctx, ch)
	if err == nil {
		t.Fatalf("Subscribe returned nil err; want ErrNotSupported")
	}
	if !errors.Is(err, driver.ErrNotSupported) {
		t.Errorf("Subscribe err = %v; want errors.Is(driver.ErrNotSupported) == true", err)
	}
}
