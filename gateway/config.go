// Package gateway is the M0 minimal CIOS gateway: it loads the
// protocol dictionary + per-device point maps, spins up one modbus
// driver per device, polls on a fixed interval, and writes the
// converted telemetry directly to VictoriaMetrics via the
// /api/v1/import/prometheus endpoint. The package owns the
// driver->dict->promproj glue and the run loop. NATS, the local
// WAL, and Set/control wiring are M1 concerns; they are not in
// this package by design (PRMT-010 §1).
package gateway

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/yurimeng/cios/pkg/cpath"
)

// Config is the on-disk shape of gateway.yaml. Every field is
// required unless the contract in PRMT-010 §4.1 says otherwise
// (only Interval has a default of 10s). NATS is the optional M1
// fan-out switch (PRMT-015): when nil, the gateway falls back to
// the M0 direct HTTP write to VMWriteURL. CMDBLifecycle fields
// (PRMT-097) are set from CLI flags in cmd/cios-gateway/main.go,
// not from YAML, so a config-file-only deployment is zero-regression
// (the poll goroutine is gated on the empty-URL contract).
type Config struct {
	Site        string        `yaml:"site"`
	ProtocolDir string        `yaml:"protocol_dir"`
	VMWriteURL  string        `yaml:"vm_write_url"`
	Interval    time.Duration `yaml:"interval"`
	Devices     []Device      `yaml:"devices"`
	NATS        *NATSConfig   `yaml:"nats,omitempty"`
	// CMDBLifecycleURL is the core base URL for the retired-asset
	// poll (PRMT-097). Empty → feature disabled, no goroutine
	// starts, behaviour identical to pre-PRMT-097.
	CMDBLifecycleURL string
	// CMDBLifecycleInterval is the poll period. Zero → 5m default.
	CMDBLifecycleInterval time.Duration
	// ControlListen is the optional local HTTP bind for POST /v1/control/set
	// (P722 southbound from core). Empty → control API disabled.
	// Must be loopback (M4 F1). Set from CLI in cmd/cios-gateway (not YAML).
	ControlListen string
	// ControlToken is the shared secret required on every control write
	// (Authorization: Bearer or X-CIOS-Control-Token). Required when
	// ControlListen is non-empty (M4 F1).
	ControlToken string
	// MetricsListen is an optional HTTP bind for GET /metrics
	// (DATA-RESILIENCE G5). Empty = disabled. Prefer 127.0.0.1:PORT.
	MetricsListen string
}

// NATSConfig holds optional NATS JetStream connection settings.
// If nil in Config, the gateway falls back to M0 direct HTTP write.
type NATSConfig struct {
	URL         string `yaml:"url"`           // e.g. nats://localhost:4222
	StreamName  string `yaml:"stream_name"`   // default: "CIOS_TLM"
	WALPath     string `yaml:"wal_path"`      // default: "/var/lib/cios/gateway.wal"
	WALMaxBytes int64  `yaml:"wal_max_bytes"` // 0 → use pkg/wal default; see LOCKED L65
}

// Device is one physical asset's collector binding. PointMap is
// resolved relative to the config file's directory (PRMT-010 §4.1:
// "相对配置文件所在目录"), so LoadConfig stashes the config-dir
// basename on each Device for the caller.
type Device struct {
	Asset    string `yaml:"asset"`
	PointMap string `yaml:"pointmap"`
	Endpoint string `yaml:"endpoint"`
	UnitID   string `yaml:"unit_id"`
	// PluginBinary is the path to the driver plugin binary
	// (PRMT-017 §4.7). When non-empty, gateway/run.go launches the
	// binary as an out-of-process go-plugin driver (spec-005 §1, M1
	// LOCKED L60) instead of constructing the in-process modbus
	// driver. Empty means M0-style in-process modbus, unchanged. The
	// path is resolved relative to the config file directory at
	// LoadConfig time, same convention as PointMap.
	PluginBinary string `yaml:"plugin_binary,omitempty"`
	// Protocol selects the southbound driver: "" or "modbus" → in-process
	// modbus TCP (today's behaviour); "snmp" → in-process SNMP v2c.
	Protocol string `yaml:"protocol,omitempty"`
	// Community is the SNMP v2c community (protocol=="snmp" only). Empty → "public".
	Community string `yaml:"community,omitempty"`
	// baseDir is filled in by LoadConfig; it is the directory of
	// the parent config file, so PointMap can be a relative path.
	baseDir string
}

// LoadConfig reads, parses, and validates a gateway.yaml. Site and
// VMWriteURL are required; Interval <= 0 falls back to 10s; Devices
// must be non-empty with each Asset passing Dict.ParseAssetPath and
// each Asset's first node-segment matching Site. The dictionary is
// loaded twice (once for validation, once at run time) — we do that
// here so a bad Site surfaces as a config error, not a startup
// surprise.
func LoadConfig(path string) (Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("gateway: read %s: %w", path, err)
	}
	var cfg Config
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return Config{}, fmt.Errorf("gateway: parse %s: %w", path, err)
	}

	if cfg.Site == "" {
		return Config{}, fmt.Errorf("gateway: site is empty")
	}
	if cfg.VMWriteURL == "" {
		return Config{}, fmt.Errorf("gateway: vm_write_url is empty")
	}
	if cfg.ProtocolDir == "" {
		return Config{}, fmt.Errorf("gateway: protocol_dir is empty")
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 10 * time.Second
	}
	if len(cfg.Devices) == 0 {
		return Config{}, fmt.Errorf("gateway: devices list is empty")
	}
	if cfg.NATS != nil {
		// PRMT-015 §4.4: when the NATS block is present, URL is
		// mandatory (an empty URL is almost certainly a config typo,
		// not a deliberate "disable" — the disable switch is the
		// nil-pointer, not the empty URL).
		if cfg.NATS.URL == "" {
			return Config{}, fmt.Errorf("gateway: nats.url is empty")
		}
		if cfg.NATS.StreamName == "" {
			cfg.NATS.StreamName = "CIOS_TLM"
		}
		if cfg.NATS.WALPath == "" {
			cfg.NATS.WALPath = "/var/lib/cios/gateway.wal"
		}
	}

	abs, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return Config{}, fmt.Errorf("gateway: config dir: %w", err)
	}

	// protocol_dir is relative to the config file (same convention
	// as pointmap). Resolve it once here so Run/LoadConfig callers
	// can hand a ready-to-go path to cpath.LoadDict. Resolve BEFORE
	// loading the dict for validation so the first read sees the
	// same path the runtime will use.
	if !filepath.IsAbs(cfg.ProtocolDir) {
		cfg.ProtocolDir = filepath.Join(abs, cfg.ProtocolDir)
	}

	// The dictionary drives Asset validation. Loading it here is
	// one extra file read but lets us reject typos in the config
	// before we have started a driver.
	dict, err := cpath.LoadDict(cfg.ProtocolDir)
	if err != nil {
		return Config{}, fmt.Errorf("gateway: load dictionary: %w", err)
	}

	for i := range cfg.Devices {
		dev := &cfg.Devices[i]
		if dev.Asset == "" {
			return Config{}, fmt.Errorf("gateway: devices[%d] asset is empty", i)
		}
		ap, err := dict.ParseAssetPath(dev.Asset)
		if err != nil {
			return Config{}, fmt.Errorf("gateway: devices[%d] asset: %w", i, err)
		}
		if ap.Site != cfg.Site {
			return Config{}, fmt.Errorf("gateway: devices[%d] asset site %q != config site %q",
				i, ap.Site, cfg.Site)
		}
		if dev.PointMap == "" {
			return Config{}, fmt.Errorf("gateway: devices[%d] pointmap is empty", i)
		}
		if dev.Endpoint == "" {
			return Config{}, fmt.Errorf("gateway: devices[%d] endpoint is empty", i)
		}
		// PRMT-023 §4.1: Protocol is the southbound driver selector.
		// "" or "modbus" → in-process modbus TCP (today's path);
		// "snmp" → in-process SNMP v2c. snmp+plugin_binary is reserved
		// for a later prompt (cios-snmp-driver go-plugin entry).
		if dev.Protocol == "" {
			dev.Protocol = "modbus"
		}
		switch dev.Protocol {
		case "modbus":
			// nothing extra; UnitID default below.
		case "snmp":
			if dev.PluginBinary != "" {
				return Config{}, fmt.Errorf("gateway: devices[%d] snmp driver does not support plugin_binary yet", i)
			}
			if dev.Community == "" {
				dev.Community = "public"
			}
		default:
			return Config{}, fmt.Errorf("gateway: devices[%d] unknown protocol %q", i, dev.Protocol)
		}
		if dev.UnitID == "" {
			dev.UnitID = "1"
		}
		// PluginBinary, when set, is resolved against the same
		// baseDir as PointMap so relative paths in gateway.yaml
		// ("./bin/cios-modbus-driver") work the same way (PRMT-017
		// §4.7). An absolute path is kept verbatim.
		if dev.PluginBinary != "" && !filepath.IsAbs(dev.PluginBinary) {
			dev.PluginBinary = filepath.Join(abs, dev.PluginBinary)
		}
		dev.baseDir = abs
	}

	return cfg, nil
}

// PointMapPath returns the absolute path of a device's point-map
// file. LoadConfig resolves the file's base directory at load time;
// the caller is expected to invoke LoadConfig before constructing
// Pipelines, so Device.baseDir is set when this is called.
func (d Device) PointMapPath() string {
	if filepath.IsAbs(d.PointMap) {
		return d.PointMap
	}
	return filepath.Join(d.baseDir, d.PointMap)
}
