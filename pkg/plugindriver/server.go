package plugindriver

import (
	"context"
	"errors"

	"github.com/yurimeng/cios/pkg/driver"
	driverproto "github.com/yurimeng/cios/proto"
)

// Server adapts a driver.Driver implementation to the gRPC
// DriverService surface. The plugin process constructs one with
// NewServer and hands it to plugin.Serve via GRPCDriverPlugin (see
// plugin.go).
type Server struct {
	driverproto.UnimplementedDriverServiceServer
	impl driver.Driver
}

// NewServer wraps impl as a Server. impl must be non-nil; a nil impl
// is a programmer error inside the plugin binary, and we let the
// nil-pointer panic surface immediately rather than papering over it
// with a runtime error on every RPC.
func NewServer(impl driver.Driver) *Server {
	return &Server{impl: impl}
}

// errorString collapses an in-process driver error to the wire
// representation. driver.ErrNotSupported becomes the magic sentinel
// the client recognises; everything else is just Error(). nil maps
// to the empty string (= success on the wire).
func errorString(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, driver.ErrNotSupported) {
		return errNotSupportedWire
	}
	return err.Error()
}

// Init forwards to impl.Init.
func (s *Server) Init(ctx context.Context, req *driverproto.InitRequest) (*driverproto.InitResponse, error) {
	cfg := DriverConfigFromProto(req.GetCfg())
	if err := s.impl.Init(ctx, cfg); err != nil {
		return &driverproto.InitResponse{Error: errorString(err)}, nil
	}
	return &driverproto.InitResponse{}, nil
}

// Discover forwards to impl.Discover. AssetCandidate.Serial maps to
// the proto's path field, AssetCandidate.Hints to meta, and
// AssetCandidate.Type to the v2 type field (PRMT-017 R2 fix).
func (s *Server) Discover(ctx context.Context, _ *driverproto.DiscoverRequest) (*driverproto.DiscoverResponse, error) {
	cands, err := s.impl.Discover(ctx)
	if err != nil {
		return &driverproto.DiscoverResponse{Error: errorString(err)}, nil
	}
	if len(cands) == 0 {
		return &driverproto.DiscoverResponse{}, nil
	}
	out := make([]*driverproto.AssetCandidateProto, 0, len(cands))
	for _, c := range cands {
		p := &driverproto.AssetCandidateProto{Path: c.Serial, Type: c.Type}
		if len(c.Hints) > 0 {
			p.Meta = make(map[string]string, len(c.Hints))
			for k, v := range c.Hints {
				p.Meta[k] = v
			}
		}
		out = append(out, p)
	}
	return &driverproto.DiscoverResponse{Candidates: out}, nil
}

// Collect forwards to impl.Collect.
func (s *Server) Collect(ctx context.Context, _ *driverproto.CollectRequest) (*driverproto.CollectResponse, error) {
	samples, err := s.impl.Collect(ctx)
	if err != nil {
		return &driverproto.CollectResponse{Error: errorString(err)}, nil
	}
	if len(samples) == 0 {
		return &driverproto.CollectResponse{}, nil
	}
	wire := make([]*driverproto.Sample, len(samples))
	for i, sm := range samples {
		wire[i] = SampleToProto(sm)
	}
	return &driverproto.CollectResponse{Samples: wire}, nil
}

// Subscribe forwards impl.Subscribe onto the stream. The local
// channel is bounded at 16 so a slow gRPC consumer back-pressures
// the source driver instead of unbounded queuing; that matches the
// in-process semantics, where the gateway's reader paces the
// driver.
//
// Terminal error handling (PRMT-017 R2): impl.Subscribe's return
// value is wired through the SubscribeResponse.error field rather
// than a raw gRPC status — that's the only path that preserves
// errors.Is(err, driver.ErrNotSupported) across the wire (the v1
// schema used gRPC status, which lost the typed sentinel). We send
// one final empty-sample response with the error string set, then
// return nil so the stream closes cleanly.
func (s *Server) Subscribe(_ *driverproto.SubscribeRequest, stream driverproto.DriverService_SubscribeServer) error {
	ctx := stream.Context()
	ch := make(chan driver.Sample, 16)
	errCh := make(chan error, 1)
	go func() {
		errCh <- s.impl.Subscribe(ctx, ch)
		close(ch)
	}()
	for sm := range ch {
		if err := stream.Send(&driverproto.SubscribeResponse{Sample: SampleToProto(sm)}); err != nil {
			return err
		}
	}
	if err := <-errCh; err != nil {
		// Terminal: send one trailing response carrying the error
		// string. The client peels it back to a typed error via
		// wireError, so driver.ErrNotSupported survives intact.
		return stream.Send(&driverproto.SubscribeResponse{Error: errorString(err)})
	}
	return nil
}

// Write forwards to impl.Write. The wire shape carries the
// ControlResult even when err is non-nil (so the gateway can show
// Accepted=false to the operator); errors go in the error field.
func (s *Server) Write(ctx context.Context, req *driverproto.WriteRequest) (*driverproto.WriteResponse, error) {
	cmd := ControlCommandFromProto(req.GetCmd())
	res, err := s.impl.Write(ctx, cmd)
	return &driverproto.WriteResponse{
		Result: ControlResultToProto(res),
		Error:  errorString(err),
	}, nil
}

// Health forwards to impl.Health. Health is total on the in-process
// contract, so there is no error path to surface here.
func (s *Server) Health(ctx context.Context, _ *driverproto.HealthRequest) (*driverproto.HealthResponse, error) {
	return &driverproto.HealthResponse{Health: DriverHealthToProto(s.impl.Health(ctx))}, nil
}
