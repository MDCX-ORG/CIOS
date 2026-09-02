// cios-core: the M0 site API server. Listens on -listen (default
// 127.0.0.1:8080), persists to -store (default ./cios-core.json) or
// PostgreSQL when -pg-dsn / CIOS_PG_DSN is set, validates paths
// against the protocol dictionary at -protocol, and proxies
// /v1/metrics/* to the VictoriaMetrics at -vm. Optional
// -seed-alarms ingests a YAML of alarms at boot. Optional -rbac
// enables bearer-token + RBAC on /v1 (PRMT-019).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/pprof"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/nats-io/nats.go"
	"gopkg.in/yaml.v3"

	"github.com/yurimeng/cios/core"
	"github.com/yurimeng/cios/pkg/cpath"
	"github.com/yurimeng/cios/pkg/mtls"
)

func main() {
	var (
		listen          = flag.String("listen", "127.0.0.1:8080", "HTTP listen address")
		storePath       = flag.String("store", "cios-core.json", "JSON file backing the store (used when no -pg-dsn / CIOS_PG_DSN)")
		protocolDir     = flag.String("protocol", "", "protocol/ directory (required)")
		vmURL           = flag.String("vm", "http://127.0.0.1:8428", "VictoriaMetrics base URL")
		seedAlarms      = flag.String("seed-alarms", "", "optional YAML of seed alarms")
		pgDSN           = flag.String("pg-dsn", "", "PostgreSQL DSN; empty → env CIOS_PG_DSN; still empty → fileStore")
		migrations      = flag.String("migrations", "./migrations", "SQL migrations directory (used when DSN is set)")
		rbacPath        = flag.String("rbac", "", "RBAC config YAML path; empty → no auth (M0)")
		allowPublicBind = flag.Bool("allow-public-bind", false,
			"allow -listen on non-loopback addresses (compose/cross-container; default off keeps loopback-only semantics)")
		allowNoAuth = flag.Bool("allow-no-auth", false,
			"explicitly run with NO auth when -rbac is empty (fail-closed otherwise)")
		ticketWebhookURL = flag.String("ticket-webhook-url", "",
			"POST URL for ticket lifecycle CloudEvents (spec-008 §5); empty → no-op (PRMT-035)")
		ticketWebhookURLs = flag.String("ticket-webhook-urls", "",
			"comma-separated webhook channel URLs for ticket CE fan-out (PRMT-200 / P644 v0); merged with -ticket-webhook-url")
		ticketSMTPHost = flag.String("ticket-smtp-host", "",
			"SMTP host for ticket email channel (P783/L105); empty → no-op (or env CIOS_TICKET_SMTP_HOST)")
		ticketSMTPPort = flag.Int("ticket-smtp-port", 0,
			"SMTP port (default 587; or env CIOS_TICKET_SMTP_PORT)")
		ticketSMTPFrom = flag.String("ticket-smtp-from", "",
			"SMTP From address (or env CIOS_TICKET_SMTP_FROM)")
		ticketSMTPTo = flag.String("ticket-smtp-to", "",
			"comma-separated recipient list (or env CIOS_TICKET_SMTP_TO)")
		ticketSMTPUser = flag.String("ticket-smtp-user", "",
			"optional SMTP PLAIN username (or env CIOS_TICKET_SMTP_USER)")
		ticketSMTPPass = flag.String("ticket-smtp-pass", "",
			"optional SMTP PLAIN password (or env CIOS_TICKET_SMTP_PASS)")
		slaScanInterval = flag.Duration("sla-scan-interval", 60*time.Second,
			"how often the SLA scanner ticks; default 60s; <=0 falls back to 60s (PRMT-036)")
		reportDir = flag.String("report-dir", "",
			"ops report output directory; empty → scheduler disabled (PRMT-042)")
		reportInterval = flag.Duration("report-interval", 24*time.Hour,
			"how often the report scheduler ticks; default 24h; <=0 falls back to 24h (PRMT-042)")
		reportKeep = flag.Int("report-keep", 30,
			"max number of ops reports to retain on disk; default 30; <=0 keeps all (PRMT-064)")
		pmScanInterval = flag.Duration("pm-scan-interval", 1*time.Hour,
			"how often the PM scanner ticks; default 1h; <=0 falls back to 60m (PRMT-043)")
		runbookDir = flag.String("runbook-dir", "",
			"directory of *.md runbook files for /v1/runbooks/{key}; empty → endpoint 404 (PRMT-044)")
		inspectionPhotoDir = flag.String("inspection-photo-dir", "",
			"directory for /v1/inspections/form/{id}/photo uploads; empty → endpoint 503 disabled (PRMT-063)")
		inspectionPhotoMax = flag.Int64("inspection-photo-max", 8<<20,
			"per-file size cap (bytes) for inspection photo uploads; default 8 MiB (PRMT-063)")
		inspectionScanInterval = flag.Duration("inspection-scan-interval", 1*time.Hour,
			"how often the inspection scanner ticks; default 1h; <=0 falls back to 60m (PRMT-049)")
		spareScanInterval = flag.Duration("spare-scan-interval", 1*time.Hour,
			"how often the spare low-stock scanner ticks; default 1h; <=0 falls back to 60m (PRMT-054)")
		usageScanInterval = flag.Duration("usage-scan-interval", 0,
			"Usage daily+monthly recompute interval (0 = disabled; e.g. 1h). PRMT-197/198 / L102")
		natsURL = flag.String("nats-url", "",
			"NATS URL for UsageEventSink (empty → env CIOS_NATS_URL; still empty → noop sink). PRMT-198")
		reconcileScanInterval = flag.Duration("reconcile-scan-interval", 0,
			"how often the CMDB/telemetry drift scanner ticks; default 0=off; matches report-scheduler 'empty=off' (PRMT-057)")
		reconcileWindow = flag.String("reconcile-window", "7d",
			"trailing window for the reconcile drift probe; default 7d (PRMT-057)")
		mtlsMode = flag.String("mtls-mode", "",
			"component mTLS: off|require (empty → env CIOS_MTLS_MODE, default off). P793")
		tlsCert = flag.String("tls-cert", "",
			"server cert PEM when mtls-mode=require (or env CIOS_CORE_TLS_CERT)")
		tlsKey = flag.String("tls-key", "",
			"server key PEM when mtls-mode=require (or env CIOS_CORE_TLS_KEY)")
		tlsClientCA = flag.String("tls-client-ca", "",
			"client CA PEM when mtls-mode=require (or env CIOS_CORE_TLS_CLIENT_CA)")
		controlURL = flag.String("control-url", "",
			"gateway control base URL for P722 Set dispatch (e.g. http://127.0.0.1:8092); empty → env CIOS_CONTROL_URL; still empty → policy-only")
		controlToken = flag.String("control-token", "",
			"shared secret for gateway control API (M4 F1); empty → env CIOS_CONTROL_TOKEN / CIOS_GATEWAY_CONTROL_TOKEN")
		dataPlaneTLS = flag.String("data-plane-tls", "",
			"data-plane TLS: off|require (empty → env CIOS_DATA_PLANE_TLS, default off). P793 Phase 3")
		pgTLSCA = flag.String("pg-tls-ca", "",
			"Postgres server CA PEM (sslmode=verify-full); empty → env CIOS_PG_TLS_CA")
		pgTLSCert = flag.String("pg-tls-cert", "",
			"optional Postgres client cert PEM (or env CIOS_PG_TLS_CERT)")
		pgTLSKey = flag.String("pg-tls-key", "",
			"optional Postgres client key PEM (or env CIOS_PG_TLS_KEY)")
		natsTLSCA = flag.String("nats-tls-ca", "",
			"NATS server CA PEM (or env CIOS_NATS_TLS_CA)")
		natsTLSCert = flag.String("nats-tls-cert", "",
			"optional NATS client cert PEM (or env CIOS_NATS_TLS_CERT)")
		natsTLSKey = flag.String("nats-tls-key", "",
			"optional NATS client key PEM (or env CIOS_NATS_TLS_KEY)")
		vmTLSCA = flag.String("vm-tls-ca", "",
			"VictoriaMetrics / HTTPS upstream CA PEM (or env CIOS_VM_TLS_CA)")
		vmTLSCert = flag.String("vm-tls-cert", "",
			"optional VM client cert PEM (or env CIOS_VM_TLS_CERT)")
		vmTLSKey = flag.String("vm-tls-key", "",
			"optional VM client key PEM (or env CIOS_VM_TLS_KEY)")
		pprofAddr = flag.String("pprof-addr", "",
			"PRMT-211: optional pprof listen addr (empty=off; MUST be loopback, e.g. 127.0.0.1:6060)")
	)
	flag.Parse()
	if *protocolDir == "" {
		log.Fatalf("cios-core: -protocol is required")
	}
	if err := run(runArgs{
		listen:                 *listen,
		storePath:              *storePath,
		protocolDir:            *protocolDir,
		vmURL:                  *vmURL,
		seedAlarms:             *seedAlarms,
		pgDSN:                  *pgDSN,
		migrations:             *migrations,
		rbacPath:               *rbacPath,
		allowNoAuth:            *allowNoAuth,
		allowPublicBind:        *allowPublicBind,
		ticketWebhookURL:       *ticketWebhookURL,
		ticketWebhookURLs:      *ticketWebhookURLs,
		ticketSMTPHost:         *ticketSMTPHost,
		ticketSMTPPort:         *ticketSMTPPort,
		ticketSMTPFrom:         *ticketSMTPFrom,
		ticketSMTPTo:           *ticketSMTPTo,
		ticketSMTPUser:         *ticketSMTPUser,
		ticketSMTPPass:         *ticketSMTPPass,
		slaScanInterval:        *slaScanInterval,
		reportDir:              *reportDir,
		reportInterval:         *reportInterval,
		reportKeep:             *reportKeep,
		pmScanInterval:         *pmScanInterval,
		runbookDir:             *runbookDir,
		inspectionPhotoDir:     *inspectionPhotoDir,
		inspectionPhotoMax:     *inspectionPhotoMax,
		inspectionScanInterval: *inspectionScanInterval,
		spareScanInterval:      *spareScanInterval,
		usageScanInterval:      *usageScanInterval,
		natsURL:                *natsURL,
		reconcileScanInterval:  *reconcileScanInterval,
		reconcileWindow:        *reconcileWindow,
		mtlsMode:               *mtlsMode,
		tlsCert:                *tlsCert,
		tlsKey:                 *tlsKey,
		tlsClientCA:            *tlsClientCA,
		controlURL:             *controlURL,
		controlToken:           *controlToken,
		dataPlaneTLS:           *dataPlaneTLS,
		pgTLSCA:                *pgTLSCA,
		pgTLSCert:              *pgTLSCert,
		pgTLSKey:               *pgTLSKey,
		natsTLSCA:              *natsTLSCA,
		natsTLSCert:            *natsTLSCert,
		natsTLSKey:             *natsTLSKey,
		vmTLSCA:                *vmTLSCA,
		vmTLSCert:              *vmTLSCert,
		vmTLSKey:               *vmTLSKey,
		pprofAddr:              *pprofAddr,
	}); err != nil {
		log.Fatalf("cios-core: %v", err)
	}
}

// runArgs is the parsed-flag bag handed to run() so tests can
// exercise the boot path without touching flag.CommandLine.
type runArgs struct {
	listen            string
	storePath         string
	protocolDir       string
	vmURL             string
	seedAlarms        string
	pgDSN             string
	migrations        string
	rbacPath          string
	allowNoAuth       bool
	allowPublicBind   bool
	ticketWebhookURL  string
	ticketWebhookURLs string // comma-separated (PRMT-200)
	// P783 / L105 email channel (optional).
	ticketSMTPHost         string
	ticketSMTPPort         int
	ticketSMTPFrom         string
	ticketSMTPTo           string
	ticketSMTPUser         string
	ticketSMTPPass         string
	slaScanInterval        time.Duration
	reportDir              string
	reportInterval         time.Duration
	reportKeep             int
	pmScanInterval         time.Duration
	runbookDir             string
	inspectionPhotoDir     string
	inspectionPhotoMax     int64
	inspectionScanInterval time.Duration
	spareScanInterval      time.Duration
	usageScanInterval      time.Duration
	natsURL                string
	reconcileScanInterval  time.Duration
	reconcileWindow        string
	// P793 mTLS (default off).
	mtlsMode     string
	tlsCert      string
	tlsKey       string
	tlsClientCA  string
	controlURL   string
	controlToken string
	// P793 Phase 3 data-plane TLS (product-native).
	dataPlaneTLS string
	pgTLSCA      string
	pgTLSCert    string
	pgTLSKey     string
	natsTLSCA    string
	natsTLSCert  string
	natsTLSKey   string
	vmTLSCA      string
	vmTLSCert    string
	vmTLSKey     string
	// PRMT-211: optional loopback-only pprof (dev/lab profiling).
	pprofAddr string
}

// envLookup is overridable in tests; production reads os.Getenv.
var envLookup = os.Getenv

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func run(a runArgs) error {
	dict, err := cpath.LoadDict(a.protocolDir)
	if err != nil {
		return err
	}

	// Store selection: -pg-dsn > CIOS_PG_DSN > fileStore.
	// P793 Phase 3: optional PG TLS rewrite before connect.
	dsn := strings.TrimSpace(a.pgDSN)
	if dsn == "" {
		dsn = strings.TrimSpace(envLookup("CIOS_PG_DSN"))
	}
	dpMode, err := mtls.ParseMode(firstNonEmpty(a.dataPlaneTLS, envLookup("CIOS_DATA_PLANE_TLS")))
	if err != nil {
		return errors.New("data-plane tls: " + err.Error())
	}
	pgCA := firstNonEmpty(a.pgTLSCA, envLookup("CIOS_PG_TLS_CA"))
	pgCert := firstNonEmpty(a.pgTLSCert, envLookup("CIOS_PG_TLS_CERT"))
	pgKey := firstNonEmpty(a.pgTLSKey, envLookup("CIOS_PG_TLS_KEY"))
	if dsn != "" {
		var perr error
		dsn, perr = mtls.PGDSNApplyTLS(dsn, pgCA, pgCert, pgKey)
		if perr != nil {
			return errors.New("pg tls: " + perr.Error())
		}
		if dpMode == mtls.ModeRequire && !mtls.HasSecurePG(dsn) {
			return errors.New("data-plane tls require: postgres DSN needs sslmode=verify-full (set CIOS_PG_TLS_CA or sslmode in DSN)")
		}
		if pgCA != "" {
			log.Printf("cios-core: postgres TLS sslmode=verify-full ca=%s", pgCA)
		}
	}
	st, err := openStore(dsn, a.storePath, a.migrations)
	if err != nil {
		return err
	}

	if a.seedAlarms != "" {
		alarms, err := loadSeedAlarms(a.seedAlarms)
		if err != nil {
			return err
		}
		if err := st.SeedAlarms(context.Background(), alarms); err != nil {
			return err
		}
	}

	// RBAC is opt-in: empty -rbac → no auth. M1 hardening (PRMT-029
	// §2(B)): require an explicit -allow-no-auth so a missing
	// -rbac can't leave the API silently wide open.
	if a.rbacPath == "" {
		if !a.allowNoAuth {
			return errors.New("refusing to start without auth: pass -rbac <file> or -allow-no-auth")
		}
		log.Printf("WARN: running with NO auth (-allow-no-auth)")
	}
	// PRMT-217 (report S-1): a production build has no lab bypass, so
	// -allow-no-auth would leave admin surfaces unreachable rather than
	// open. Refuse outright instead of starting a half-working process.
	if a.allowNoAuth && !core.LabBypassAvailable() {
		return errors.New(
			"refusing to start: -allow-no-auth requires a lab build " +
				"(go build -tags lab); this binary has the auth bypass compiled out")
	}
	// PRMT-216 (report S-1/S-2): -allow-no-auth injects a synthetic
	// platform-admin principal (labNoAuthAdminPrincipal), so anything that
	// can reach this listener is a platform admin. Refuse to combine that
	// with a non-loopback bind unless the operator says so explicitly.
	if a.allowNoAuth {
		lo, err := isLoopbackHostPort(a.listen)
		if err != nil {
			return fmt.Errorf("invalid -listen %q: %w", a.listen, err)
		}
		if !lo && !a.allowPublicBind {
			return fmt.Errorf(
				"refusing to start: -allow-no-auth with non-loopback -listen %q "+
					"would expose a synthetic platform-admin to the network; "+
					"bind loopback, or pass -allow-public-bind if the exposure is "+
					"intentional (e.g. container with loopback-only port publishing)",
				a.listen)
		}
		if !lo && a.allowPublicBind {
			log.Printf("WARN: -allow-no-auth on non-loopback %s with -allow-public-bind: "+
				"every reachable client is a platform admin (dev only)", a.listen)
		}
	}
	var auth *core.AuthConfig
	if a.rbacPath != "" {
		auth, err = loadRBAC(a.rbacPath, st)
		if err != nil {
			return err
		}
	}

	srv := core.NewServerWithStore(st, dict, a.vmURL, auth)

	// P793 Phase 3: VictoriaMetrics HTTPS client (optional CA / mTLS).
	vmCA := firstNonEmpty(a.vmTLSCA, envLookup("CIOS_VM_TLS_CA"))
	vmCert := firstNonEmpty(a.vmTLSCert, envLookup("CIOS_VM_TLS_CERT"))
	vmKey := firstNonEmpty(a.vmTLSKey, envLookup("CIOS_VM_TLS_KEY"))
	if dpMode == mtls.ModeRequire {
		if err := mtls.RequireHTTPS(a.vmURL, "vm"); err != nil {
			return err
		}
	}
	if vmCA != "" {
		tlsCfg, terr := mtls.OutboundTLS(vmCA, vmCert, vmKey)
		if terr != nil {
			return errors.New("vm tls: " + terr.Error())
		}
		srv.SetVMHTTPClient(&http.Client{
			Timeout:   5 * time.Second,
			Transport: &http.Transport{TLSClientConfig: tlsCfg},
		})
		log.Printf("cios-core: victoria metrics TLS ca=%s", vmCA)
	}

	// Usage event sink (PRMT-198 / L102). Optional NATS core Publish
	// to cios.usage.upserted. Empty -nats-url and CIOS_NATS_URL →
	// leave sink nil (Noop at call sites).
	natsAddr := strings.TrimSpace(a.natsURL)
	if natsAddr == "" {
		natsAddr = strings.TrimSpace(envLookup("CIOS_NATS_URL"))
	}
	if natsAddr != "" {
		natsCA := firstNonEmpty(a.natsTLSCA, envLookup("CIOS_NATS_TLS_CA"))
		natsCert := firstNonEmpty(a.natsTLSCert, envLookup("CIOS_NATS_TLS_CERT"))
		natsKey := firstNonEmpty(a.natsTLSKey, envLookup("CIOS_NATS_TLS_KEY"))
		var nopts []nats.Option
		if natsCA != "" {
			tlsCfg, terr := mtls.OutboundTLS(natsCA, natsCert, natsKey)
			if terr != nil {
				return errors.New("nats tls: " + terr.Error())
			}
			nopts = append(nopts, nats.Secure(tlsCfg))
			log.Printf("cios-core: nats TLS ca=%s", natsCA)
		} else if dpMode == mtls.ModeRequire {
			return errors.New("data-plane tls require: NATS configured but CIOS_NATS_TLS_CA empty")
		}
		nc, nerr := nats.Connect(natsAddr, nopts...)
		if nerr != nil {
			return errors.New("nats connect (" + natsAddr + "): " + nerr.Error())
		}
		// Conn implements Publish(subject, data []byte) error.
		srv.SetUsageEventSink(core.NATSUsageEventSink{Pub: nc})
		log.Printf("cios-core: usage event sink → nats %s subject=cios.usage.upserted", natsAddr)
		// Close on process exit via shutdown path: defer after signal
		// ctx is created below would miss early returns; close on
		// ListenAndServe exit is enough for demo. Hold ref via defer
		// once we have the run lifetime — attach after ctx below.
		defer nc.Close()
	}

	// P722: optional southbound Set → gateway control API.
	ctrl := strings.TrimSpace(a.controlURL)
	if ctrl == "" {
		ctrl = strings.TrimSpace(envLookup("CIOS_CONTROL_URL"))
	}
	if ctrl != "" {
		if dpMode == mtls.ModeRequire {
			if err := mtls.RequireHTTPS(ctrl, "control-url"); err != nil {
				return err
			}
		}
		tok := strings.TrimSpace(a.controlToken)
		if tok == "" {
			tok = strings.TrimSpace(envLookup("CIOS_CONTROL_TOKEN"))
		}
		if tok == "" {
			tok = strings.TrimSpace(envLookup("CIOS_GATEWAY_CONTROL_TOKEN"))
		}
		if tok == "" {
			return errors.New("control-url set but control token empty (pass -control-token or CIOS_CONTROL_TOKEN)")
		}
		sink := core.HTTPControlSink{BaseURL: ctrl, Token: tok}
		if vmCA != "" {
			// Reuse VM TLS CA for control HTTPS when same site PKI; optional.
			if tlsCfg, terr := mtls.OutboundTLS(vmCA, vmCert, vmKey); terr == nil {
				sink.HTTPClient = &http.Client{
					Timeout:   10 * time.Second,
					Transport: &http.Transport{TLSClientConfig: tlsCfg},
				}
			}
		}
		srv.SetControlSink(sink)
		log.Printf("cios-core: control sink → %s/v1/control/set (bearer)", strings.TrimRight(ctrl, "/"))
	}

	// Ticket lifecycle webhook fan-out (PRMT-035 + PRMT-200 / P644 v0).
	// Empty → no-op.
	hooks := parseWebhookURLList(a.ticketWebhookURL, a.ticketWebhookURLs, envLookup("CIOS_TICKET_WEBHOOK_URLS"))
	if len(hooks) == 1 {
		srv.SetTicketWebhookURL(hooks[0], nil)
	} else if len(hooks) > 1 {
		srv.SetTicketWebhookURLs(hooks, nil)
	}
	if len(hooks) > 0 {
		log.Printf("cios-core: ticket webhook channels=%d", len(hooks))
	}
	// P783 / L105: optional SMTP email channel (second notify path).
	if smtpCfg := buildTicketSMTPConfig(a); smtpCfg != nil {
		srv.SetTicketSMTP(smtpCfg)
		log.Printf("cios-core: ticket email channel enabled host=%s port=%d to=%d",
			smtpCfg.Host, smtpCfg.Port, len(smtpCfg.To))
	}
	srv.SetRunbookDir(a.runbookDir)
	if a.runbookDir != "" {
		log.Printf("cios-core: runbook dir = %s", a.runbookDir)
	}
	// PRMT-063: photo upload dir + per-file cap. Empty dir
	// disables the endpoint (returns 503). The max is applied
	// even when the dir is set so an operator can keep the
	// default 8 MiB or tighten it via -inspection-photo-max.
	srv.SetInspectionPhotoDir(a.inspectionPhotoDir, a.inspectionPhotoMax)
	if a.inspectionPhotoDir != "" {
		log.Printf("cios-core: inspection photo dir = %s max = %d bytes",
			a.inspectionPhotoDir, a.inspectionPhotoMax)
	}

	// Log what we actually enabled — without leaking the DSN password
	// or any token material. DSN is reduced to scheme+host; the file
	// store path is logged verbatim because it's not a secret.
	backend := "file:" + a.storePath
	if dsn != "" {
		backend = "pg:" + redactDSN(dsn)
	}
	authMode := "off"
	if auth != nil {
		authMode = "on"
	}
	log.Printf("cios-core: boot backend=%s auth=%s", backend, authMode)

	// HTTP server timeouts (PRMT-075, CODE-EVALUATION-2026-06-21 H3).
	// ReadHeaderTimeout (5s) caps header read; ReadTimeout (15s) caps
	// the entire request body read; WriteTimeout (30s) caps the
	// response write (30s, not 15s, because /v1/reports/ops and
	// /v1/inspections/form can spend longer computing/rendering);
	// IdleTimeout (60s) caps keep-alive between requests. Without
	// ReadTimeout/WriteTimeout/IdleTimeout, a slow or stalled client
	// can hold a connection indefinitely and exhaust file descriptors.
	h := &http.Server{
		Addr:              a.listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	// P793 component mTLS (H2/H3). Default off preserves lab loopback.
	modeStr := strings.TrimSpace(a.mtlsMode)
	if modeStr == "" {
		modeStr = envLookup("CIOS_MTLS_MODE")
	}
	mode, err := mtls.ParseMode(modeStr)
	if err != nil {
		return errors.New("mtls mode: " + err.Error())
	}
	certFile := firstNonEmpty(a.tlsCert, envLookup("CIOS_CORE_TLS_CERT"))
	keyFile := firstNonEmpty(a.tlsKey, envLookup("CIOS_CORE_TLS_KEY"))
	caFile := firstNonEmpty(a.tlsClientCA, envLookup("CIOS_CORE_TLS_CLIENT_CA"))
	if mode == mtls.ModeRequire {
		tlsCfg, err := mtls.ServerTLS(mtls.Material{
			CertFile: certFile, KeyFile: keyFile, CAFile: caFile,
		})
		if err != nil {
			return errors.New("mtls require: " + err.Error())
		}
		h.TLSConfig = tlsCfg
		core.SetTenantHeaderRequiresMTLSPeer(true)
		log.Printf("cios-core: mTLS MODE=require (client cert required; X-CIOS-Tenant peer-gated)")
	} else {
		core.SetTenantHeaderRequiresMTLSPeer(false)
	}

	// Graceful shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// PRMT-211: optional pprof on a separate mux/server (never on business mux).
	// Default off. Non-loopback bind fails boot (memory/stack leak surface).
	var pprofSrv *http.Server
	if addr := strings.TrimSpace(a.pprofAddr); addr != "" {
		if err := validatePprofLoopback(addr); err != nil {
			return err
		}
		pmux := http.NewServeMux()
		pmux.HandleFunc("/debug/pprof/", pprof.Index)
		pmux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		pmux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		pmux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		pmux.HandleFunc("/debug/pprof/trace", pprof.Trace)
		pmux.Handle("/debug/pprof/heap", pprof.Handler("heap"))
		pmux.Handle("/debug/pprof/goroutine", pprof.Handler("goroutine"))
		pmux.Handle("/debug/pprof/allocs", pprof.Handler("allocs"))
		pmux.Handle("/debug/pprof/mutex", pprof.Handler("mutex"))
		pmux.Handle("/debug/pprof/block", pprof.Handler("block"))
		pprofSrv = &http.Server{
			Addr:              addr,
			Handler:           pmux,
			ReadHeaderTimeout: 5 * time.Second,
		}
		go func() {
			log.Printf("WARN: cios-core pprof listening on %s (dev only; PRMT-211)", addr)
			if err := pprofSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Printf("cios-core: pprof server: %v", err)
			}
		}()
	}

	// SLA scanner goroutine (PRMT-036, spec-008 §3 + §5). Shares
	// the shutdown ctx with the HTTP server, so SIGINT cleanly
	// stops both. The scanner is fail-soft (logs and continues)
	// so it never crashes the process.
	go srv.RunSLAScanner(ctx, a.slaScanInterval)
	log.Printf("cios-core: sla scanner interval=%s", a.slaScanInterval)
	// Report scheduler (PRMT-042 / §M2-4). Empty -report-dir
	// disables the goroutine — RunReportScheduler returns
	// immediately in that case.
	go srv.RunReportScheduler(ctx, a.reportInterval, a.reportDir, a.reportKeep)
	if a.reportDir != "" {
		log.Printf("cios-core: report scheduler dir=%s interval=%s keep=%d", a.reportDir, a.reportInterval, a.reportKeep)
	}
	// PM scanner (PRMT-043 / E2.4 P531). Fail-soft by contract;
	// never crashes the process.
	go srv.RunPMScanner(ctx, a.pmScanInterval)
	log.Printf("cios-core: pm scanner interval=%s", a.pmScanInterval)
	// Inspection scanner (PRMT-049 / E2.7 P561). Mirrors PM:
	// fail-soft, share the same shutdown ctx, never crashes the
	// process.
	go srv.RunInspectionScanner(ctx, a.inspectionScanInterval)
	log.Printf("cios-core: inspection scanner interval=%s", a.inspectionScanInterval)
	// Spare low-stock scanner (PRMT-054 / E2.5 P541 闭环). Mirrors
	// PM: fail-soft, share the same shutdown ctx, never crashes
	// the process. Single-instance assumption (leader election
	// = T43); two cios-core instances ticking in parallel would
	// race on the alarm_id dedup check.
	go srv.RunSpareStockScanner(ctx, a.spareScanInterval)
	log.Printf("cios-core: spare stock scanner interval=%s", a.spareScanInterval)
	// Usage daily recompute scanner (PRMT-197 / L102). Default-off
	// (interval=0); opt-in via -usage-scan-interval. Same pattern as
	// reconcile: short-circuits inside RunUsageScanner when <=0.
	if a.usageScanInterval > 0 {
		go srv.RunUsageScanner(ctx, a.usageScanInterval)
		log.Printf("cios-core: usage scanner interval=%s", a.usageScanInterval)
	} else {
		log.Printf("cios-core: usage scanner disabled (interval<=0)")
	}
	// CMDB/telemetry drift scanner (PRMT-057 / E2.6 闭环). Default-
	// off (interval=0): the scanner short-circuits inside
	// RunReconcileScanner, so no goroutine body ever runs in the
	// default deployment. Opt-in via -reconcile-scan-interval.
	if a.reconcileScanInterval > 0 {
		go srv.RunReconcileScanner(ctx, a.reconcileScanInterval, a.reconcileWindow)
		log.Printf("cios-core: reconcile scanner interval=%s window=%s",
			a.reconcileScanInterval, a.reconcileWindow)
	} else {
		log.Printf("cios-core: reconcile scanner disabled (interval<=0)")
	}
	errCh := make(chan error, 1)
	go func() {
		log.Printf("cios-core: listening on %s (vm=%s) mtls=%s", a.listen, a.vmURL, mode)
		var err error
		if mode == mtls.ModeRequire {
			err = h.ListenAndServeTLS(certFile, keyFile)
		} else {
			err = h.ListenAndServe()
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()
	select {
	case <-ctx.Done():
		log.Printf("cios-core: shutdown signal received")
	case err := <-errCh:
		if err != nil {
			return err
		}
	}
	shCtx, shCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shCancel()
	if pprofSrv != nil {
		_ = pprofSrv.Shutdown(shCtx)
	}
	return h.Shutdown(shCtx)
}

// isLoopbackHostPort reports whether addr binds only the loopback
// interface. An empty host (":8080") means all interfaces and is NOT
// loopback. Shared by the -pprof-addr check and the no-auth public
// bind guard (PRMT-216).
func isLoopbackHostPort(addr string) (bool, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false, err
	}
	h := strings.TrimSpace(host)
	if h == "" || h == "0.0.0.0" || h == "::" || h == "[::]" {
		return false, nil
	}
	if h == "127.0.0.1" || h == "::1" || h == "localhost" || h == "[::1]" {
		return true, nil
	}
	return false, nil
}

// validatePprofLoopback enforces PRMT-211: pprof may only bind loopback.
// Error messages are unchanged from PRMT-211 §4.6 (callers may match text).
func validatePprofLoopback(addr string) error {
	lo, err := isLoopbackHostPort(addr)
	if err != nil {
		// Allow bare ":6060" → empty host means all interfaces → reject.
		// SplitHostPort succeeds for ":6060"; this branch covers malformed
		// forms that still begin with ":" (historical PRMT-211 wording).
		if strings.HasPrefix(addr, ":") {
			return errors.New("pprof-addr must bind loopback")
		}
		return errors.New("pprof-addr: " + err.Error())
	}
	if !lo {
		return errors.New("pprof-addr must bind loopback")
	}
	return nil
}

// parseWebhookURLList merges single + comma-list + env strings into
// a de-duplicated ordered URL slice (PRMT-200).
func parseWebhookURLList(parts ...string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, p := range parts {
		for _, u := range strings.Split(p, ",") {
			u = strings.TrimSpace(u)
			if u == "" {
				continue
			}
			if _, ok := seen[u]; ok {
				continue
			}
			seen[u] = struct{}{}
			out = append(out, u)
		}
	}
	return out
}

// buildTicketSMTPConfig merges flags + env for P783 email channel.
// Returns nil when Host/From/To are incomplete (email disabled).
func buildTicketSMTPConfig(a runArgs) *core.TicketSMTPConfig {
	host := firstNonEmpty(a.ticketSMTPHost, envLookup("CIOS_TICKET_SMTP_HOST"))
	from := firstNonEmpty(a.ticketSMTPFrom, envLookup("CIOS_TICKET_SMTP_FROM"))
	toRaw := firstNonEmpty(a.ticketSMTPTo, envLookup("CIOS_TICKET_SMTP_TO"))
	if host == "" || from == "" || toRaw == "" {
		return nil
	}
	port := a.ticketSMTPPort
	if port <= 0 {
		if p := strings.TrimSpace(envLookup("CIOS_TICKET_SMTP_PORT")); p != "" {
			if n, err := strconv.Atoi(p); err == nil {
				port = n
			}
		}
	}
	if port <= 0 {
		port = 587
	}
	to := parseWebhookURLList(toRaw) // same comma-split + dedupe
	if len(to) == 0 {
		return nil
	}
	return &core.TicketSMTPConfig{
		Host: host,
		Port: port,
		From: from,
		To:   to,
		User: firstNonEmpty(a.ticketSMTPUser, envLookup("CIOS_TICKET_SMTP_USER")),
		Pass: firstNonEmpty(a.ticketSMTPPass, envLookup("CIOS_TICKET_SMTP_PASS")),
	}
}

// openStore is the pure dispatch function behind run()'s store
// selection. It exists so the test can assert the dsn>env>file
// decision without driving the full boot path (which would also
// run migrations, which a unit test should not do).
func openStore(dsn, storePath, migrations string) (core.Store, error) {
	if dsn != "" {
		return core.NewPGStore(context.Background(), dsn, migrations)
	}
	return core.NewFileStore(storePath)
}

// loadSeedAlarms parses a YAML []core.Alarm from path. We use the
// same file path resolution as the rest of the project (relative
// to cwd).
func loadSeedAlarms(path string) ([]core.Alarm, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(abs)
	if err != nil {
		return nil, err
	}
	var xs []core.Alarm
	if err := yaml.Unmarshal(raw, &xs); err != nil {
		return nil, err
	}
	return xs, nil
}

// rbacFile mirrors config/rbac.example.yaml. We only read the
// fields we need; the example file's role-list is the source of
// truth for the schema on disk.
type rbacFile struct {
	Tokens []rbacToken `yaml:"tokens"`
}

type rbacToken struct {
	SHA256  string   `yaml:"sha256"`
	Subject string   `yaml:"subject"`
	Role    string   `yaml:"role"`
	Scopes  []string `yaml:"scopes"`
}

// loadRBAC reads path, builds the sha256hex → Principal map, and
// constructs an AuthConfig via core.NewStaticTokenVerifier (which
// validates every scope glob at load time). Returns an error on:
// unreadable / non-YAML file, empty sha256, role not in
// {viewer, operator, admin}, or any bad scope (surfaced by
// NewStaticTokenVerifier). The plaintext token never appears in
// logs — the file stores only digests, and we only log the count.
//
// PRMT-M1-Checkpoint-Fix-R1 §4.3.
// PRMT-190-bis §4.3 wiring: between building the seed map (authn
// seed = static token config) and calling NewStaticTokenVerifier,
// LoadRoleBindingsInto extends each Principal.Scopes with the
// subject's persisted role_bindings rows. The authn seed
// (token → subject/role) is UNCHANGED; only the Scopes grow. Row
// scopes then compile through the same NewStaticTokenVerifier path
// and gain their origin tag from the row's origin column.
func loadRBAC(path string, st core.Store) (*core.AuthConfig, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(abs)
	if err != nil {
		return nil, err
	}
	var rf rbacFile
	if err := yaml.Unmarshal(raw, &rf); err != nil {
		return nil, err
	}
	if len(rf.Tokens) == 0 {
		return nil, errors.New("rbac: no tokens in file")
	}
	m := make(map[string]core.Principal, len(rf.Tokens))
	for i, t := range rf.Tokens {
		if t.SHA256 == "" {
			return nil, errors.New("rbac: token missing sha256")
		}
		// NewStaticTokenVerifier does the heavy lifting on scope
		// validation; the role check is on us (it's not a glob).
		role := core.Role(t.Role)
		if !isKnownRole(role) {
			return nil, errors.New("rbac: token has unknown role: " + t.Role)
		}
		m[t.SHA256] = core.Principal{
			Subject: t.Subject,
			Role:    role,
			Scopes:  append([]string(nil), t.Scopes...),
		}
		_ = i // index reserved for future per-line errors with line numbers
	}
	// PRMT-190-bis §4.3 loader: augment the seed map with persisted
	// role_bindings rows. The authn seed (token → subject/role)
	// does NOT change; only Scopes grows. Row scopes then flow through
	// NewStaticTokenVerifier → compilePrincipalScopes unchanged,
	// gaining their origin tag from the row's origin column.
	if st != nil {
		m, err = core.LoadRoleBindingsInto(context.Background(), st, m)
		if err != nil {
			return nil, err
		}
	}
	v, err := core.NewStaticTokenVerifier(m)
	if err != nil {
		return nil, err
	}
	return &core.AuthConfig{Verifier: v}, nil
}

// isKnownRole gates the YAML loader to the role set PRMT-019
// defines; M3 tenant is intentionally rejected (fail-noisy) until
// its prompt lands and edits core/rbac.go (M1-CHECKPOINT §3.8).
func isKnownRole(r core.Role) bool {
	switch r {
	case core.RoleViewer, core.RoleOperator, core.RoleAdmin:
		return true
	default:
		return false
	}
}

// redactDSN strips the password component from a postgres URL so
// the boot log can show which server we connected to without
// leaking the credential. Non-parseable DSNs are passed through a
// coarse strip — anything matching the "://user:secret@host"
// pattern loses the "secret" segment.
func redactDSN(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil || u.User == nil {
		return "***"
	}
	if _, hasPwd := u.User.Password(); hasPwd {
		u.User = url.UserPassword(u.User.Username(), "***")
	}
	return u.Redacted()
}
