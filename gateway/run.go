// Run is the M0 gateway's main loop. It is intentionally a single
// sequential function: it loads the dictionary + per-device point
// maps, constructs one modbus driver per device, ticks on
// Config.Interval, batches the per-sample Prometheus lines, and
// POSTs the batch to VM. The package-level design is fail-soft
// at every layer (per the PRMT-009 driver contract and the spec):
// a single bad sample does not stop the batch, a single failing
// device does not stop the loop, a non-2xx from VM does not crash
// the process. WAL, retries, NATS fan-out, and Set are all M1.
package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os/exec"
	"strings"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/yurimeng/cios/pkg/cpath"
	"github.com/yurimeng/cios/pkg/driver"
	"github.com/yurimeng/cios/pkg/driver/modbus"
	"github.com/yurimeng/cios/pkg/driver/snmp"
	"github.com/yurimeng/cios/pkg/natspub"
	"github.com/yurimeng/cios/pkg/plugindriver"
	"github.com/yurimeng/cios/pkg/pointmap"
	"github.com/yurimeng/cios/pkg/resilmetrics"
	"github.com/yurimeng/cios/pkg/wal"
)

// retiredCache holds the set of asset paths whose lifecycle is
// "retired" per the core CMDB. PRMT-097: the gateway polls core
// for this set and skips retired assets in the collect loop.
// Spec-006 §1.1 mandates edge autonomy — when core is unreachable
// we keep the last known set (fail-open by design, see §2 of
// PRMT-097). The cache is an atomic.Value pointing to a fresh
// immutable map; the poll goroutine swaps the whole map and the
// read path takes a cheap Load().Contains() (no shared mutation,
// so the race detector is happy).
type retiredCache struct {
	v atomic.Value // map[string]struct{}
}

func newRetiredCache() *retiredCache {
	c := &retiredCache{}
	c.v.Store(map[string]struct{}{})
	return c
}

// contains reports whether the given asset path is in the cache.
// A nil pointer is treated as "no retired assets known" so the
// skip check stays a one-liner in the hot path.
func (c *retiredCache) contains(asset string) bool {
	if c == nil {
		return false
	}
	m := c.v.Load().(map[string]struct{})
	_, ok := m[asset]
	return ok
}

// replace atomically swaps in a fresh map. We never mutate the
// stored map in place so the read path is wait-free.
func (c *retiredCache) replace(next map[string]struct{}) {
	if next == nil {
		next = map[string]struct{}{}
	}
	c.v.Store(next)
}

// startRetiredPoll launches the background poll goroutine. It
// returns immediately with no side effects when url is empty, so
// the existing config-file-only deployments are zero-regression.
// On every tick the goroutine issues GET <url>/v1/assets?lifecycle=retired
// and atomically replaces the cache; a failure preserves the
// previous cache and logs a warning (fail-open). fetchCtx is
// derived from ctx so a shutdown interrupts the in-flight request.
func startRetiredPoll(ctx context.Context, url string, interval time.Duration) *retiredCache {
	cache := newRetiredCache()
	if url == "" {
		// Feature disabled: empty cache, no goroutine.
		return cache
	}
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	hc := &http.Client{Timeout: 5 * time.Second}
	tick := time.NewTicker(interval)
	go func() {
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				refreshRetiredCache(ctx, hc, url, cache)
			}
		}
	}()
	return cache
}

// refreshRetiredCache performs one fetch and atomically swaps the
// cache on success. Failures preserve the existing cache and log
// a single warn line — the collect loop never sees a half-empty
// or a transient error, satisfying the "core 挂 → 停采" red line.
func refreshRetiredCache(ctx context.Context, hc *http.Client, url string, cache *retiredCache) {
	endpoint := strings.TrimRight(url, "/") + "/v1/assets?lifecycle=retired"
	fetchCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		log.Printf("gateway: build retired-list GET: %v", err)
		return
	}
	resp, err := hc.Do(req)
	if err != nil {
		log.Printf("gateway: retired-list fetch failed (preserving cache): %v", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		log.Printf("gateway: retired-list status %d (preserving cache)", resp.StatusCode)
		return
	}
	var body struct {
		Items []struct {
			Path string `json:"path"`
		} `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		log.Printf("gateway: retired-list decode: %v", err)
		return
	}
	next := make(map[string]struct{}, len(body.Items))
	for _, it := range body.Items {
		if it.Path != "" {
			next[it.Path] = struct{}{}
		}
	}
	cache.replace(next)
}

// Run brings up the gateway and blocks until ctx is cancelled.
// On a normal ctx-cancel it returns nil. On startup errors
// (unloadable dictionary, no valid devices) it returns the first
// error encountered.
func Run(ctx context.Context, cfg Config) error {
	dict, err := cpath.LoadDict(cfg.ProtocolDir)
	if err != nil {
		return fmt.Errorf("gateway: load dictionary: %w", err)
	}
	units, err := pointmap.LoadUnits(cfg.ProtocolDir)
	if err != nil {
		return fmt.Errorf("gateway: load units: %w", err)
	}

	// Per-device bring-up. We accept "Init failed, keep going" as
	// the spec-009/PRMT-009 contract: the driver will reconnect on
	// its own during the first Collect. A pointmap that fails V1–V7
	// validation IS fatal because it means a deploy-time typo.
	var devs []gatewayDevice
	for i := range cfg.Devices {
		dcfg := cfg.Devices[i]
		pm, perrs := pointmap.Load(dcfg.PointMapPath(), dict, units)
		if len(perrs) > 0 {
			for _, e := range perrs {
				log.Printf("gateway: device %s pointmap invalid: %v", dcfg.Asset, e)
			}
			return fmt.Errorf("gateway: device %s pointmap validation failed", dcfg.Asset)
		}

		// PRMT-023 §4.4: build the protocol-agnostic Pipeline map once
		// per point, then dispatch on dcfg.Protocol to choose
		// modbus|snmp binding construction. LoadConfig has already
		// normalised dcfg.Protocol to "modbus"|"snmp"; an empty value
		// means LoadConfig was bypassed (e.g. unit tests building
		// Config directly) and must be treated as modbus to preserve
		// today's behaviour.
		pipes := make(map[string]*Pipeline, len(pm.Points))
		var (
			d  driver.Driver
			dc driver.DriverConfig
		)
		switch dcfg.Protocol {
		case "", "modbus":
			mb := make([]modbus.Binding, 0, len(pm.Points))
			for _, pd := range pm.Points {
				pl, b, err := NewPipeline(dcfg.Asset, pm, pd, dict, units)
				if err != nil {
					log.Printf("gateway: device %s point %s: %v", dcfg.Asset, pd.Point, err)
					return fmt.Errorf("gateway: pipeline build failed: %w", err)
				}
				mb = append(mb, b)
				pipes[pd.Point] = pl
			}
			d = modbus.New(mb)
			dc = driver.DriverConfig{
				Endpoint: dcfg.Endpoint,
				Options:  map[string]string{"unit_id": dcfg.UnitID},
			}
		case "snmp":
			sb := make([]snmp.Binding, 0, len(pm.Points))
			for _, pd := range pm.Points {
				pl, err := newPipelineCore(dcfg.Asset, pm, pd, dict, units)
				if err != nil {
					log.Printf("gateway: device %s point %s: %v", dcfg.Asset, pd.Point, err)
					return fmt.Errorf("gateway: pipeline build failed: %w", err)
				}
				b, err := snmpBindingFromProtocol(pd)
				if err != nil {
					log.Printf("gateway: device %s point %s: %v", dcfg.Asset, pd.Point, err)
					return fmt.Errorf("gateway: pipeline build failed: %w", err)
				}
				sb = append(sb, b)
				pipes[pd.Point] = pl
			}
			d = snmp.New(sb)
			dc = driver.DriverConfig{
				Endpoint: dcfg.Endpoint,
				Options:  map[string]string{"community": dcfg.Community},
			}
		}

		// PRMT-017 §4.8: when the device has plugin_binary set, the
		// gateway hosts the driver out-of-process via go-plugin
		// (spec-005 §1, LOCKED L60). The in-process modbus.Driver
		// built above is discarded; we replace it with a
		// plugindriver.Client that forwards the same six methods
		// over gRPC to the subprocess. The plugin binary needs the
		// pointmap and protocol-dir paths to construct its own
		// bindings table — DriverConfig only carries the runtime
		// endpoint, not the static binding table. snmp+plugin_binary
		// is rejected at LoadConfig time (PRMT-023 §4.1), so this
		// branch only ever fires for modbus.
		if dcfg.PluginBinary != "" {
			cmd := exec.CommandContext(ctx, dcfg.PluginBinary,
				"-pointmap", dcfg.PointMapPath(),
				"-protocol-dir", cfg.ProtocolDir,
			)
			pdrv, err := plugindriver.NewClientFromCmd(cmd)
			if err != nil {
				return fmt.Errorf("gateway: device %s plugin: %w", dcfg.Asset, err)
			}
			// Reap the subprocess when the gateway shuts down. A
			// single goroutine per device is fine — Kill is
			// idempotent on plugin.Client.
			go func() { <-ctx.Done(); pdrv.Kill() }()
			d = pdrv
		}

		if err := d.Init(ctx, dc); err != nil {
			// PRMT-009 contract: dial failure is recoverable; the
			// next Collect will retry. Log and continue.
			log.Printf("gateway: device %s Init: %v (continuing; Collect will retry)", dcfg.Asset, err)
		}
		devs = append(devs, gatewayDevice{cfg: dcfg, drv: d, pipes: pipes})
	}

	if len(devs) == 0 {
		return fmt.Errorf("gateway: no devices to run")
	}

	// P722: register full cpath → driver for southbound Set API.
	ctrlReg := newControlRegistry()
	for _, gd := range devs {
		for rel := range gd.pipes {
			ctrlReg.register(gd.cfg.Asset, rel, gd.drv)
		}
	}
	if err := startControlServer(ctx, cfg.ControlListen, cfg.ControlToken, ctrlReg); err != nil {
		return err
	}

	httpClient := &http.Client{Timeout: 5 * time.Second}

	// PRMT-015 §4.5: when cfg.NATS is set, bring up the JetStream
	// publisher and a local WAL fallback; otherwise the tick loop
	// uses the M0 direct-HTTP path. Either branch is fail-soft per
	// tick — startup failures are fatal, in-tick failures are
	// logged and the loop continues.
	var pub *natspub.Publisher
	if cfg.NATS != nil {
		// DATA-RESILIENCE G2: reconnect forever (default MaxReconnects≈60
		// permanently silenced publish after ~2 min broker outage).
		opts := natspub.ConnectOpts("cios-gateway", nil)
		nc, err := nats.Connect(cfg.NATS.URL, opts...)
		if err != nil {
			return fmt.Errorf("gateway: nats connect: %w", err)
		}
		defer nc.Close()
		js, err := nc.JetStream()
		if err != nil {
			return fmt.Errorf("gateway: jetstream: %w", err)
		}
		// Ensure the stream exists. AddStream returns "stream name
		// already in use" on the second boot, which we treat as
		// success (idempotent). Any other error is fatal.
		_, err = js.AddStream(&nats.StreamConfig{
			Name:     cfg.NATS.StreamName,
			Subjects: []string{"cios.tlm.>"},
			Storage:  nats.FileStorage,
			MaxAge:   7 * 24 * time.Hour,
			Replicas: 1,
		})
		if err != nil && !isStreamExistsErr(err) {
			return fmt.Errorf("gateway: add stream: %w", err)
		}
		w, err := wal.OpenWithMaxSize(cfg.NATS.WALPath, cfg.NATS.WALMaxBytes)
		if err != nil {
			return fmt.Errorf("gateway: open wal: %w", err)
		}
		defer w.Close()
		pub = natspub.New(js, w)
		// DATA-RESILIENCE G5: wire publish/WAL counters.
		gmet := &GatewayResilMetrics{}
		pub.OnPublishFail = func() { gmet.PublishFailures.Inc() }
		pub.OnWALFrame = func() {
			gmet.WALFrames.Inc()
			gmet.WALBytes.Set(pub.WALBytes())
		}
		pub.OnWALFull = func() {
			gmet.WALFullDrops.Inc()
			gmet.WALBytes.Set(pub.WALBytes())
			log.Printf("gateway: WARN WAL full — newest frame dropped (see runbooks/telemetry-data-resilience.md)")
		}
		if stopMetrics, merr := resilmetrics.Listen(cfg.MetricsListen, gmet.writePrometheus); merr != nil {
			return fmt.Errorf("gateway: metrics: %w", merr)
		} else {
			defer stopMetrics()
		}
	}

	// Tick loop. Ticker drops a tick if the previous one is still
	// in flight; that is what we want under VM outage so the next
	// batch fires the moment the network is back, instead of
	// queuing a backlog.
	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()
	// PRMT-097: opt-in retired-asset poll. Empty URL → cache stays
	// empty and the skip check in postBatch/publishBatch is a
	// single no-op map lookup, identical to pre-PRMT-097 behaviour.
	retired := startRetiredPoll(ctx, cfg.CMDBLifecycleURL, cfg.CMDBLifecycleInterval)
	for {
		select {
		case <-ctx.Done():
			return nil
		case t := <-ticker.C:
			if pub != nil {
				publishBatch(ctx, pub, cfg.Site, t, devs, retired)
			} else {
				postBatch(ctx, httpClient, cfg.VMWriteURL, t, devs, retired)
			}
		}
	}
}

// postBatch walks every device, runs one Collect, converts each
// sample to a Prometheus line, and POSTs the joined body to VM.
// Any single failure (Collect error -> nil by contract; per-sample
// Convert error -> skip; non-2xx -> drop batch) is logged and
// swallowed. The loop never exits on a per-tick failure.
// PRMT-097: when the device's asset path is in the retired cache
// the device is skipped (log only) — same fail-open contract as
// pre-PRMT-097 when the cache is empty.
func postBatch(ctx context.Context, hc *http.Client, url string, t time.Time, devs []gatewayDevice, retired *retiredCache) {
	var body strings.Builder
	for _, d := range devs {
		if retired.contains(d.cfg.Asset) {
			log.Printf("gateway: skipping retired %s", d.cfg.Asset)
			continue
		}
		samples, err := d.drv.Collect(ctx)
		if err != nil {
			log.Printf("gateway: %s Collect: %v", d.cfg.Asset, err)
			continue
		}
		wrote := 0
		for _, s := range samples {
			pl, ok := d.pipes[s.Point]
			if !ok {
				// Driver returned a sample we never registered.
				// This should not happen — the binding table is
				// built from the same pointmap — but we log and
				// skip rather than panic.
				log.Printf("gateway: %s no pipeline for sample %q", d.cfg.Asset, s.Point)
				continue
			}
			line, err := pl.Convert(s)
			if err != nil {
				log.Printf("gateway: %s Convert %q: %v", d.cfg.Asset, s.Point, err)
				continue
			}
			body.WriteString(line)
			body.WriteByte('\n')
			wrote++
		}
		// DATA-RESILIENCE G6: heartbeat only when this device produced
		// at least one sample line (keeps empty-pipe unit tests HTTP-free).
		if wrote > 0 {
			hb := pipelineHeartbeatLine(siteOf(d.cfg.Asset), d.cfg.Asset, topAssetOf(d.cfg.Asset), leafType(d.cfg.Asset), t)
			body.WriteString(hb)
			body.WriteByte('\n')
		}
	}
	if body.Len() == 0 {
		// Nothing to write this tick. Skip the HTTP round-trip.
		return
	}
	if hc == nil {
		log.Printf("gateway: POST %s: nil http client", url)
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader([]byte(body.String())))
	if err != nil {
		log.Printf("gateway: build POST %s: %v", url, err)
		return
	}
	req.Header.Set("Content-Type", "text/plain")
	resp, err := hc.Do(req)
	if err != nil {
		log.Printf("gateway: POST %s: %v", url, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		// Drain a bit of the body so the message is meaningful,
		// but cap it so a chatty server doesn't fill the log.
		buf := make([]byte, 256)
		n, _ := io.ReadFull(resp.Body, buf)
		log.Printf("gateway: POST %s: status %d, body: %q", url, resp.StatusCode, buf[:n])
		return
	}
	// Drain to allow connection reuse. The body is short and the
	// status is 2xx; we don't need to inspect it.
	_, _ = io.Copy(io.Discard, resp.Body)
}

// gatewayDevice is the package-private bundle of everything one
// device's collector needs. The struct lives in run.go (not at the
// top of the file) so the test file can stand up its own variant
// without colliding with the production type. drv is the
// driver.Driver interface (not *modbus.Driver) so the same struct
// holds either an in-process modbus driver or a plugindriver.Client
// (PRMT-017 §4.8).
type gatewayDevice struct {
	cfg   Device
	drv   driver.Driver
	pipes map[string]*Pipeline
}

// --- NATS publish path (PRMT-015 §4.5) -------------------------------------

// publishBatch walks every device, runs one Collect, converts each
// sample to a Prometheus line, and publishes the per-device batch
// to JetStream. Mirror of postBatch's collect+convert loop, but
// instead of a single HTTP POST it groups samples by device and
// emits one TelemetryBatch per device (because each device is a
// distinct top-level asset and therefore a distinct NATS subject).
//
// Failures are per-device logged and swallowed: a bad Collect or
// Convert never aborts the other devices, and a NATS publish
// failure is buffered by the WAL inside the Publisher.
func publishBatch(ctx context.Context, pub *natspub.Publisher, site string, t time.Time, devs []gatewayDevice, retired *retiredCache) {
	for _, d := range devs {
		if retired.contains(d.cfg.Asset) {
			log.Printf("gateway: skipping retired %s", d.cfg.Asset)
			continue
		}
		samples, err := d.drv.Collect(ctx)
		if err != nil {
			log.Printf("gateway: %s Collect: %v", d.cfg.Asset, err)
			continue
		}
		var lines []string
		for _, s := range samples {
			pl, ok := d.pipes[s.Point]
			if !ok {
				log.Printf("gateway: %s no pipeline for sample %q", d.cfg.Asset, s.Point)
				continue
			}
			line, err := pl.Convert(s)
			if err != nil {
				log.Printf("gateway: %s Convert %q: %v", d.cfg.Asset, s.Point, err)
				continue
			}
			lines = append(lines, line)
		}
		// DATA-RESILIENCE G6: always emit a heartbeat even when Collect
		// returned zero samples so absence in VM means "path dead".
		top := topAssetOf(d.cfg.Asset)
		lines = appendHeartbeat(lines, site, d.cfg.Asset, top, leafType(d.cfg.Asset), t)
		if len(lines) == 0 {
			continue
		}
		batch := natspub.TelemetryBatch{
			Site:      site,
			TopAsset:  top,
			Timestamp: t,
			Encoding:  "promtext",
			Lines:     lines,
		}
		if err := pub.Publish(ctx, batch); err != nil {
			log.Printf("gateway: publish %s: %v", batch.Subject(), err)
		}
	}
}

// topAssetOf returns the first two dot-separated segments of an
// asset path, e.g. "sgp01.pod002.cdu000" → "sgp01.pod002".
// If the path has fewer than 2 segments, returns the whole path.
func topAssetOf(asset string) string {
	parts := strings.SplitN(asset, ".", 3)
	if len(parts) < 2 {
		return asset
	}
	return parts[0] + "." + parts[1]
}

// isStreamExistsErr checks whether err is the JetStream
// "stream name already in use" reply. We string-match rather than
// reach for a typed sentinel because nats.go wraps server errors
// across an API boundary and the stable contract is the substring.
func isStreamExistsErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "stream name already in use")
}
