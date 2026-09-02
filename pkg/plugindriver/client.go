package plugindriver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"

	"github.com/hashicorp/go-plugin"

	"github.com/yurimeng/cios/pkg/driver"
	driverproto "github.com/yurimeng/cios/proto"
)

// errNotSupportedWire is the string the plugin returns in any RPC
// error field when the underlying driver returned driver.ErrNotSupported.
// Sentinel matching travels as a string across the wire and is mapped
// back to the typed sentinel on the gateway side.
const errNotSupportedWire = "ErrNotSupported"

// Client launches a driver plugin subprocess and adapts the gRPC
// stub into the in-process driver.Driver contract. The struct is
// safe to share across goroutines as long as the gateway calls Kill
// exactly once at shutdown — the underlying *plugin.Client guarantees
// the subprocess is reaped on Kill and gRPC errors from then on are
// expected.
type Client struct {
	rpcClient *plugin.Client
	raw       driverproto.DriverServiceClient
}

// Compile-time assertion: *Client must satisfy driver.Driver. A
// signature drift in pkg/driver fails here.
var _ driver.Driver = (*Client)(nil)

// NewClient launches the plugin binary at binaryPath via go-plugin
// and returns a ready-to-call Client. ctx scopes only the launch +
// handshake; cancelling ctx after NewClient returns does NOT kill
// the subprocess — the caller owns lifecycle via Kill.
//
// PRMT-017 R2: this constructor is a convenience wrapper around
// NewClientFromCmd for callers that only need the default exec.Cmd
// (no extra args, no env tweaks). The modbus plugin requires
// -pointmap / -protocol-dir flags, so gateway/run.go uses
// NewClientFromCmd directly; this entry-point survives for drivers
// whose binaries take no flags.
func NewClient(ctx context.Context, binaryPath string) (*Client, error) {
	if binaryPath == "" {
		return nil, fmt.Errorf("plugindriver: empty plugin binary path")
	}
	return NewClientFromCmd(exec.CommandContext(ctx, binaryPath))
}

// NewClientFromCmd is the same as NewClient but lets the caller hand
// in a fully-prepared *exec.Cmd (extra args, env vars, etc.). The
// plugin binary's flag parsing happens here, so the test harness can
// inject -pointmap / -protocol-dir without NewClient knowing about
// them.
func NewClientFromCmd(cmd *exec.Cmd) (*Client, error) {
	if cmd == nil || cmd.Path == "" {
		return nil, fmt.Errorf("plugindriver: nil or empty cmd")
	}
	rpc := plugin.NewClient(&plugin.ClientConfig{
		HandshakeConfig:  HandshakeConfig,
		Plugins:          PluginMap,
		Cmd:              cmd,
		AllowedProtocols: []plugin.Protocol{plugin.ProtocolGRPC},
	})
	rpcc, err := rpc.Client()
	if err != nil {
		rpc.Kill()
		return nil, fmt.Errorf("plugindriver: handshake: %w", err)
	}
	raw, err := rpcc.Dispense(PluginKey)
	if err != nil {
		rpc.Kill()
		return nil, fmt.Errorf("plugindriver: dispense %q: %w", PluginKey, err)
	}
	stub, ok := raw.(driverproto.DriverServiceClient)
	if !ok {
		rpc.Kill()
		return nil, fmt.Errorf("plugindriver: unexpected dispense type %T", raw)
	}
	return &Client{rpcClient: rpc, raw: stub}, nil
}

// Kill terminates the plugin subprocess. Idempotent; safe to call
// from a deferred goroutine that fires on ctx.Done().
func (c *Client) Kill() {
	if c == nil || c.rpcClient == nil {
		return
	}
	c.rpcClient.Kill()
}

// wireError maps an RPC error-field string back to the typed error
// the in-process driver contract uses. Empty → nil; the sentinel
// string → driver.ErrNotSupported; anything else → a wrapped
// formatted error so the caller can log it. method names the RPC
// for the human-readable message; it is otherwise unused.
func wireError(method, errMsg string) error {
	if errMsg == "" {
		return nil
	}
	if errMsg == errNotSupportedWire {
		return driver.ErrNotSupported
	}
	return fmt.Errorf("plugin: %s: %s", method, errMsg)
}

// Init forwards to the plugin's Init RPC.
func (c *Client) Init(ctx context.Context, cfg driver.DriverConfig) error {
	resp, err := c.raw.Init(ctx, &driverproto.InitRequest{Cfg: DriverConfigToProto(cfg)})
	if err != nil {
		return fmt.Errorf("plugin: Init: %w", err)
	}
	return wireError("Init", resp.GetError())
}

// Discover forwards to the plugin's Discover RPC. The proto carries
// path+meta+type per candidate (the type field landed in PRMT-017
// R2; older plugins compiled against v1 simply leave it empty, which
// decodes to an empty Type string — matching the in-process zero
// value).
func (c *Client) Discover(ctx context.Context) ([]driver.AssetCandidate, error) {
	resp, err := c.raw.Discover(ctx, &driverproto.DiscoverRequest{})
	if err != nil {
		return nil, fmt.Errorf("plugin: Discover: %w", err)
	}
	if werr := wireError("Discover", resp.GetError()); werr != nil {
		return nil, werr
	}
	cands := resp.GetCandidates()
	if len(cands) == 0 {
		return nil, nil
	}
	out := make([]driver.AssetCandidate, 0, len(cands))
	for _, c := range cands {
		ac := driver.AssetCandidate{Type: c.GetType(), Serial: c.GetPath()}
		if meta := c.GetMeta(); len(meta) > 0 {
			ac.Hints = make(map[string]string, len(meta))
			for k, v := range meta {
				ac.Hints[k] = v
			}
		}
		out = append(out, ac)
	}
	return out, nil
}

// Collect forwards to the plugin's Collect RPC.
func (c *Client) Collect(ctx context.Context) ([]driver.Sample, error) {
	resp, err := c.raw.Collect(ctx, &driverproto.CollectRequest{})
	if err != nil {
		return nil, fmt.Errorf("plugin: Collect: %w", err)
	}
	if werr := wireError("Collect", resp.GetError()); werr != nil {
		return nil, werr
	}
	wire := resp.GetSamples()
	if len(wire) == 0 {
		return nil, nil
	}
	out := make([]driver.Sample, len(wire))
	for i, s := range wire {
		out[i] = SampleFromProto(s)
	}
	return out, nil
}

// Subscribe forwards to the plugin's Subscribe streaming RPC and
// fans the stream out onto ch. Returns when either the stream ends
// (io.EOF → nil) or ctx is cancelled (returns ctx.Err()). The
// caller, not Subscribe, owns ch — we never close it, because the
// in-process driver.Subscribe contract puts that on the consumer.
//
// Terminal error handling (PRMT-017 R2): the server signals errors
// via SubscribeResponse.error rather than a gRPC status, so we check
// the error field on every Recv first. A non-empty error is the
// stream's terminal message — we map it back through wireError so
// driver.ErrNotSupported (and any future typed sentinel) survives
// the round-trip and the caller's errors.Is checks still work.
func (c *Client) Subscribe(ctx context.Context, ch chan<- driver.Sample) error {
	stream, err := c.raw.Subscribe(ctx, &driverproto.SubscribeRequest{})
	if err != nil {
		return fmt.Errorf("plugin: Subscribe: %w", err)
	}
	for {
		msg, recvErr := stream.Recv()
		if errors.Is(recvErr, io.EOF) {
			return nil
		}
		if recvErr != nil {
			// ctx cancellation surfaces as a gRPC status error
			// rather than ctx.Err(); checking ctx first keeps the
			// caller's branch on Subscribe(ctx, ...) clean.
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			return fmt.Errorf("plugin: Subscribe recv: %w", recvErr)
		}
		if werr := wireError("Subscribe", msg.GetError()); werr != nil {
			return werr
		}
		if sm := msg.GetSample(); sm != nil {
			select {
			case ch <- SampleFromProto(sm):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
}

// Write forwards to the plugin's Write RPC.
func (c *Client) Write(ctx context.Context, cmd driver.ControlCommand) (driver.ControlResult, error) {
	resp, err := c.raw.Write(ctx, &driverproto.WriteRequest{Cmd: ControlCommandToProto(cmd)})
	if err != nil {
		return driver.ControlResult{}, fmt.Errorf("plugin: Write: %w", err)
	}
	if werr := wireError("Write", resp.GetError()); werr != nil {
		return ControlResultFromProto(resp.GetResult()), werr
	}
	return ControlResultFromProto(resp.GetResult()), nil
}

// Health forwards to the plugin's Health RPC. The in-process driver
// contract makes Health total — it returns DriverHealth, not (h, err)
// — so a transport-level error degrades to a Disconnected health
// snapshot with the error in Detail. That keeps gateway health
// dashboards from seeing nil-pointer surprises when a plugin
// process dies mid-flight.
func (c *Client) Health(ctx context.Context) driver.DriverHealth {
	resp, err := c.raw.Health(ctx, &driverproto.HealthRequest{})
	if err != nil {
		return driver.DriverHealth{Connected: false, Detail: fmt.Sprintf("plugin: Health: %v", err)}
	}
	return DriverHealthFromProto(resp.GetHealth())
}
