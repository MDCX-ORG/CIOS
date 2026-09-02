package plugindriver

import (
	"context"

	"github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"

	"github.com/yurimeng/cios/pkg/driver"
	driverproto "github.com/yurimeng/cios/proto"
)

// PluginKey is the map key both sides use to address the driver
// plugin in plugin.ServeConfig.Plugins / plugin.ClientConfig.Plugins.
// A single binary serves a single driver, so one key is enough.
const PluginKey = "cios_driver"

// HandshakeConfig is the go-plugin handshake. The magic cookie pair
// is a sanity check: a plugin binary launched outside the gateway
// (`./bin/cios-modbus-driver` from a shell, say) sees a missing /
// wrong cookie and exits without speaking gRPC, so we don't ship a
// binary that silently serves on whatever loopback port go-plugin
// assigns. ProtocolVersion is bumped when the wire types change.
var HandshakeConfig = plugin.HandshakeConfig{
	ProtocolVersion:  1,
	MagicCookieKey:   "CIOS_DRIVER_PLUGIN",
	MagicCookieValue: "cios-driver-v1",
}

// GRPCDriverPlugin satisfies plugin.GRPCPlugin. The plugin process
// constructs one with Impl != nil and hands it to plugin.Serve; the
// gateway-side plugin.Client receives a zero-value GRPCDriverPlugin
// via PluginMap and uses only its GRPCClient method, so Impl stays
// nil on the client.
type GRPCDriverPlugin struct {
	plugin.Plugin
	Impl driver.Driver
}

// GRPCServer is called by go-plugin on the plugin side to register
// the driver service onto the shared gRPC server. We delegate to a
// Server wrapper so the registration logic lives next to its
// implementation (server.go).
func (p *GRPCDriverPlugin) GRPCServer(_ *plugin.GRPCBroker, s *grpc.Server) error {
	driverproto.RegisterDriverServiceServer(s, NewServer(p.Impl))
	return nil
}

// GRPCClient is called by go-plugin on the gateway side to build the
// in-process driver.Driver proxy. We return the raw gRPC stub here;
// the higher-level Client (client.go) wraps it together with the
// plugin.Client so the gateway has a single Kill() entry point.
func (p *GRPCDriverPlugin) GRPCClient(_ context.Context, _ *plugin.GRPCBroker, c *grpc.ClientConn) (interface{}, error) {
	return driverproto.NewDriverServiceClient(c), nil
}

// PluginMap is the registry both go-plugin sides need. The map is
// keyed by PluginKey; the value is a zero GRPCDriverPlugin because
// the client never needs Impl.
var PluginMap = map[string]plugin.Plugin{
	PluginKey: &GRPCDriverPlugin{},
}
