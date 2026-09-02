// Command cios-gateway is the M0 minimal CIOS gateway entry point.
// It loads a gateway.yaml, runs the collect->convert->VM-import
// loop, and exits cleanly on SIGINT/SIGTERM. There are no other
// flags; everything else lives in the config file so a process
// restart is the only way to reconfigure (L52, M0 architectural
// intent).
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/yurimeng/cios/gateway"
)

func main() {
	configPath := flag.String("config", "", "path to gateway.yaml (required)")
	cmdbURL := flag.String("cmdb-lifecycle-url", "",
		"core base URL for retired-asset poll (PRMT-097). Empty = disabled.")
	cmdbInterval := flag.Duration("cmdb-lifecycle-interval", 5*time.Minute,
		"poll interval for retired-asset list (default 5m).")
	controlListen := flag.String("control-listen", "",
		"optional loopback bind for P722 POST /v1/control/set (e.g. 127.0.0.1:8092); empty = disabled")
	controlToken := flag.String("control-token", "",
		"shared secret for control API (required if -control-listen set); empty → env CIOS_GATEWAY_CONTROL_TOKEN")
	metricsListen := flag.String("metrics-listen", "",
		"optional HTTP bind for GET /metrics resilience counters (e.g. 127.0.0.1:9102); empty → env CIOS_GATEWAY_METRICS_LISTEN")
	flag.Parse()
	if *configPath == "" {
		fmt.Fprintln(os.Stderr, "cios-gateway: -config is required")
		os.Exit(2)
	}
	cfg, err := gateway.LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cios-gateway: load config: %v\n", err)
		os.Exit(1)
	}
	cfg.CMDBLifecycleURL = *cmdbURL
	cfg.CMDBLifecycleInterval = *cmdbInterval
	cfg.ControlListen = *controlListen
	tok := strings.TrimSpace(*controlToken)
	if tok == "" {
		tok = strings.TrimSpace(os.Getenv("CIOS_GATEWAY_CONTROL_TOKEN"))
	}
	cfg.ControlToken = tok
	maddr := strings.TrimSpace(*metricsListen)
	if maddr == "" {
		maddr = strings.TrimSpace(os.Getenv("CIOS_GATEWAY_METRICS_LISTEN"))
	}
	cfg.MetricsListen = maddr

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := gateway.Run(ctx, cfg); err != nil {
		log.Printf("cios-gateway: %v", err)
		os.Exit(1)
	}
}
