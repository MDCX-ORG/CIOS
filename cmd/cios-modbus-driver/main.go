// Command cios-modbus-driver is the spec-005 M1 modbus driver
// hosted as a hashicorp/go-plugin subprocess (LOCKED L60). The
// gateway launches one of these per device that has plugin_binary
// set in its config; the gateway sends the per-device DriverConfig
// (endpoint + unit_id) at Init time and then drives Collect / Write
// / Health over gRPC.
//
// Configuration shape: the gRPC DriverConfig is the same as in
// process, but the binding table — which the in-process driver
// receives via modbus.New(bindings) — is NOT part of the wire
// contract. The plugin process therefore takes two flags from the
// gateway at launch time:
//
//	-pointmap     path to the per-device pointmap YAML
//	-protocol-dir path to the protocol/ dictionary root
//
// Both are loaded once on startup, validated through the same
// pkg/pointmap loader the gateway uses, then translated to a
// []modbus.Binding by the local pointDefToBinding helper (the
// gateway's bindingFromProtocol is package-internal to gateway/,
// so we reimplement the minimal mapping here).
//
// Launching this binary outside of go-plugin (no magic cookie set)
// prints a usage line and exits 1; the go-plugin library tolerates
// that path internally, but the magic-cookie miss makes the binary
// useful as a one-shot diagnostic rather than silently failing.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/hashicorp/go-plugin"

	"github.com/yurimeng/cios/pkg/cpath"
	"github.com/yurimeng/cios/pkg/driver/modbus"
	"github.com/yurimeng/cios/pkg/modbusbind"
	"github.com/yurimeng/cios/pkg/plugindriver"
	"github.com/yurimeng/cios/pkg/pointmap"
)

func main() {
	pointmapPath := flag.String("pointmap", "", "absolute path to the device pointmap YAML (required)")
	protocolDir := flag.String("protocol-dir", "", "absolute path to the protocol/ dictionary root (required)")
	flag.Parse()

	// Refuse to run when invoked outside go-plugin. The magic cookie
	// is set by hashicorp/go-plugin in the subprocess environment;
	// its absence means a human (or the wrong launcher) ran the
	// binary. We print a usage hint and exit non-zero rather than
	// fall through to plugin.Serve, which would otherwise sit on
	// stderr forever waiting for a parent that never arrives.
	if os.Getenv(plugindriver.HandshakeConfig.MagicCookieKey) != plugindriver.HandshakeConfig.MagicCookieValue {
		fmt.Fprintf(os.Stderr,
			"cios-modbus-driver: this binary is a go-plugin host process and must be launched by cios-gateway.\n"+
				"usage: cios-gateway loads it via gateway.yaml device.plugin_binary; do not run directly.\n")
		os.Exit(1)
	}

	if *pointmapPath == "" {
		fmt.Fprintln(os.Stderr, "cios-modbus-driver: -pointmap is required")
		os.Exit(1)
	}
	if *protocolDir == "" {
		fmt.Fprintln(os.Stderr, "cios-modbus-driver: -protocol-dir is required")
		os.Exit(1)
	}

	dict, err := cpath.LoadDict(*protocolDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cios-modbus-driver: load dictionary: %v\n", err)
		os.Exit(1)
	}
	units, err := pointmap.LoadUnits(*protocolDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cios-modbus-driver: load units: %v\n", err)
		os.Exit(1)
	}
	pm, perrs := pointmap.Load(*pointmapPath, dict, units)
	if len(perrs) > 0 {
		for _, e := range perrs {
			fmt.Fprintf(os.Stderr, "cios-modbus-driver: pointmap %s: %v\n", *pointmapPath, e)
		}
		os.Exit(1)
	}

	bindings := make([]modbus.Binding, 0, len(pm.Points))
	for _, pd := range pm.Points {
		b, err := modbusbind.BuildFromPointDef(pd)
		if err != nil {
			fmt.Fprintf(os.Stderr, "cios-modbus-driver: point %s: %v\n", pd.Point, err)
			os.Exit(1)
		}
		bindings = append(bindings, b)
	}

	drv := modbus.New(bindings)

	plugin.Serve(&plugin.ServeConfig{
		HandshakeConfig: plugindriver.HandshakeConfig,
		Plugins: map[string]plugin.Plugin{
			plugindriver.PluginKey: &plugindriver.GRPCDriverPlugin{Impl: drv},
		},
		GRPCServer: plugin.DefaultGRPCServer,
	})
}

// pointDefToBinding moved to pkg/modbusbind.BuildFromPointDef in
// PRMT-030 §A. The plugin process now imports the shared package
// instead of carrying its own byte-equivalent copy.
