// cios-apigw: the M3 experience-layer API Gateway (PRMT-101,
// spec-009 §7.1). Boots a Server with the supplied Config and an
// Upstream bound to core /v1, then ListenAndServe until SIGINT/
// SIGTERM, at which point it shuts down gracefully.
//
// PRMT-111 layers on top without changing the public surface
// here: the assembly (auth handler, tuned upstream client, OPA
// PDP, mTLS) lives in startup.go. main() is a fixed wiring
// order per PRMT-111 §4:
//
//	LoadConfig
//	→ loadStartupConfig   (fail-fast on AUTH_MODE=on misconfig;
//	                       H1 refuse no-auth without ALLOW_NO_AUTH/DEV_NO_AUTH)
//	→ NewUpstream         (uses sc.UpstreamHC: mTLS + timeout)
//	→ NewServer
//	→ SetPDP              (only when CIOS_OPA_URL set)
//	→ SetOmniverseHTTPClient
//	→ SetAuthHandler      (only when sc.AuthMode && len(sc.Verifiers) > 0)
//	→ http.Server
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/yurimeng/cios/pkg/apigw"
	"github.com/yurimeng/cios/pkg/authn"
)

func main() {
	cfg, err := apigw.LoadConfig()
	if err != nil {
		// PRMT-101 §5: config errors must exit non-zero with a
		// stderr message. We use log.Fatalf so the exit code is
		// non-zero (it calls os.Exit(1)) and the message lands
		// on stderr.
		log.Fatalf("cios-apigw: %v", err)
	}
	// PRMT-216: after LoadConfig (STS/OPA may demote DevNoAuth).
	if err := validateDevNoAuthListen(cfg); err != nil {
		log.Fatalf("cios-apigw: %v", err)
	}

	// PRMT-111: load env-driven assembly (auth handler, mTLS
	// client, OPA URL). Any required env missing under
	// AUTH_MODE=on yields a fail-fast exit with a stderr message
	// that names the missing variable. AUTH_MODE unset / off
	// requires explicit ALLOW_NO_AUTH or DEV_NO_AUTH (H1 / L104);
	// silent no-auth boot is refused.
	sc, err := loadStartupConfig()
	if err != nil {
		log.Fatalf("cios-apigw: %v", err)
	}

	// PRMT-173: LoadConfig() MUST run before NewServer() so the
	// sanity check (STS/OPA vs DevNoAuth) lands first;
	// AuthMiddleware reads the snapshot below at request time.
	apigw.SnapshotDevNoAuth(cfg.DevNoAuth)
	if cfg.DevNoAuth {
		log.Printf("WARN: CIOS_APIGW_DEV_NO_AUTH=1 active: anonymous dev claims will be injected on pass-through requests; this is a dev-only path")
	}

	// Upstream uses the tuned client (timeout, optional mTLS)
	// rather than http.DefaultClient (PRMT-111 §2-bis; previously
	// F-S2 from PRMT-102/103/105/108). NewUpstream returns nil
	// on an empty base URL; LoadConfig already rejects that, but
	// we re-check defensively so the process never serves with
	// a half-configured upstream.
	up := apigw.NewUpstream(cfg.UpstreamURL, sc.UpstreamHC)
	if up == nil {
		log.Fatalf("cios-apigw: upstream URL is empty")
	}

	srv := apigw.NewServer(cfg, up)

	// Optional OPA PDP (PRMT-104 / PRMT-111 §4): only inject
	// when CIOS_OPA_URL is set. NewServer's loadPDP is a no-op
	// on empty URL, but we keep the explicit injection step
	// here so the tuned http client (timeout + mTLS) reaches
	// the PDP rather than http.DefaultClient.
	if pdp := buildOPAPDP(sc.OPAURL, sc.UpstreamHC); pdp != nil {
		srv.SetPDP(pdp)
	}

	// Omniverse outbound client gets the same tuned http client
	// so mTLS + timeout apply to upstream /v1 reverse-calls
	// (spec-006 §5).
	srv.SetOmniverseHTTPClient(sc.UpstreamHC)

	// Auth handler is wired only when AUTH_MODE=on produced at
	// least one verifier. buildAuthHandler returns (nil, nil)
	// in off mode so SetAuthHandler is skipped — preserving
	// PRMT-101's "no auth required" boot path.
	h, err := buildAuthHandler(sc, []byte(os.Getenv(envSessionKey)))
	if err != nil {
		log.Fatalf("cios-apigw: auth handler: %v", err)
	}
	if h != nil {
		srv.SetAuthHandler(h.Routes())
	}

	// PRMT-117 R2: inject a SessionDecoder that adapts
	// authn.DecodeSession into the apigw-local SessionInfo
	// projection. Wired unconditionally so a misconfiguration
	// (forget-to-wire) surfaces as a 500 at request time
	// rather than silently mishandling /auth/{realm}/token.
	// Production pkg/apigw has no import of pkg/authn (spec-006
	// §5 — apigw ← authn); this composition root is the single
	// place that bridges the seam.
	srv.SetSessionDecoder(func(key []byte, cookie string) (apigw.SessionInfo, error) {
		sess, err := authn.DecodeSession(key, cookie)
		if err != nil {
			return apigw.SessionInfo{}, err
		}
		return apigw.SessionInfo{
			Subject: sess.Subject(),
			Realm:   sess.Realm().String(),
			Claims:  sess.Claims(),
		}, nil
	})

	// HTTP server with the same timeouts the core process uses
	// (PRMT-075 baseline). ReadHeaderTimeout caps header read;
	// ReadTimeout caps body read; WriteTimeout caps response
	// write; IdleTimeout caps keep-alive.
	httpSrv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	// Graceful shutdown on SIGINT/SIGTERM — mirrors cmd/cios-core
	// so operators don't have to learn two shutdown models.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		log.Printf("cios-apigw: listening on %s upstream=%s", cfg.ListenAddr, cfg.UpstreamURL)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()
	select {
	case <-ctx.Done():
		log.Printf("cios-apigw: shutdown signal received")
	case err := <-errCh:
		if err != nil {
			log.Fatalf("cios-apigw: %v", err)
		}
	}
	shCtx, shCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shCancel()
	if err := httpSrv.Shutdown(shCtx); err != nil {
		log.Fatalf("cios-apigw: shutdown: %v", err)
	}
}
