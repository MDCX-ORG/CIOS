// Package gateway — control.go: southbound Set registry + local HTTP API (P722 / M4 F1).
//
// Core (after L108 policy) POSTs to POST /v1/control/set with a full cpath.
// The registry maps full path → (driver, relative binding point) and calls
// driver.Write. ControlListen empty → feature disabled (zero regression).
//
// Security (M4 F1):
//   - bind MUST be loopback (127.0.0.1 / ::1 / localhost) — refuse 0.0.0.0 / bare :port
//   - shared bearer token required (ControlToken / -control-token / env)
//   - healthz stays unauthenticated for local probes only on the same loopback listener
package gateway

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/yurimeng/cios/pkg/driver"
)

// controlRegistry maps full point path → write target.
type controlRegistry struct {
	mu     sync.RWMutex
	byPath map[string]controlTarget
}

type controlTarget struct {
	drv      driver.Driver
	relPoint string // binding key used by the driver
	asset    string
}

func newControlRegistry() *controlRegistry {
	return &controlRegistry{byPath: make(map[string]controlTarget)}
}

func (r *controlRegistry) register(asset, relPoint string, d driver.Driver) {
	if asset == "" || relPoint == "" || d == nil {
		return
	}
	full := asset + "." + relPoint
	r.mu.Lock()
	r.byPath[full] = controlTarget{drv: d, relPoint: relPoint, asset: asset}
	r.mu.Unlock()
}

func (r *controlRegistry) write(ctx context.Context, fullPath string, value float64, requestID string, ttl time.Duration) (driver.ControlResult, error) {
	r.mu.RLock()
	t, ok := r.byPath[fullPath]
	r.mu.RUnlock()
	if !ok {
		return driver.ControlResult{}, fmt.Errorf("gateway: control: unknown point %q", fullPath)
	}
	return t.drv.Write(ctx, driver.ControlCommand{
		Point:     t.relPoint,
		Value:     value,
		RequestID: requestID,
		TTL:       ttl,
	})
}

// controlSetRequest is the body of POST /v1/control/set.
type controlSetRequest struct {
	Path       string  `json:"path"`
	Value      float64 `json:"value"`
	RequestID  string  `json:"request_id"`
	TTLSeconds int     `json:"ttl_seconds"`
}

type controlSetResponse struct {
	Accepted bool    `json:"accepted"`
	Readback float64 `json:"readback,omitempty"`
	Detail   string  `json:"detail,omitempty"`
	Path     string  `json:"path"`
	RelPoint string  `json:"rel_point,omitempty"`
}

// controlListenLoopback reports whether host is loopback-only.
// Empty host (":port" / "0.0.0.0") is rejected — that binds all interfaces.
func controlListenLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("gateway: control listen: bad address %q: %w", addr, err)
	}
	h := strings.TrimSpace(host)
	switch strings.ToLower(h) {
	case "127.0.0.1", "::1", "localhost":
		return nil
	case "", "0.0.0.0", "::", "[::]":
		return fmt.Errorf("gateway: control listen must be loopback (got host %q); use 127.0.0.1:PORT", h)
	}
	// Resolve other hostnames; every IP must be loopback.
	ips, err := net.LookupIP(h)
	if err != nil || len(ips) == 0 {
		return fmt.Errorf("gateway: control listen host %q is not loopback (resolve failed)", h)
	}
	for _, ip := range ips {
		if !ip.IsLoopback() {
			return fmt.Errorf("gateway: control listen host %q resolves to non-loopback %s", h, ip)
		}
	}
	return nil
}

func controlTokenFromRequest(r *http.Request) string {
	if h := strings.TrimSpace(r.Header.Get("X-CIOS-Control-Token")); h != "" {
		return h
	}
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(auth) >= 7 && strings.EqualFold(auth[:7], "bearer ") {
		return strings.TrimSpace(auth[7:])
	}
	return ""
}

func controlTokenOK(got, want string) bool {
	got = strings.TrimSpace(got)
	want = strings.TrimSpace(want)
	if want == "" || got == "" || len(got) != len(want) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// startControlServer listens on addr (must be loopback) and serves
// POST /v1/control/set until ctx is done. Token is required (M4 F1).
// Returns immediately after Listen.
func startControlServer(ctx context.Context, addr, token string, reg *controlRegistry) error {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return nil
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return fmt.Errorf("gateway: control listen set but control token empty (pass -control-token or CIOS_GATEWAY_CONTROL_TOKEN)")
	}
	if err := controlListenLoopback(addr); err != nil {
		return err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/control/set", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		// Accept Authorization: Bearer or X-CIOS-Control-Token (same secret).
		if !controlTokenOK(controlTokenFromRequest(r), token) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var req controlSetRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
			http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
			return
		}
		if req.Path == "" {
			http.Error(w, "path required", http.StatusBadRequest)
			return
		}
		ttl := time.Duration(req.TTLSeconds) * time.Second
		if ttl <= 0 {
			ttl = 30 * time.Second
		}
		if req.RequestID == "" {
			req.RequestID = fmt.Sprintf("gw-%d", time.Now().UnixNano())
		}
		res, err := reg.write(r.Context(), req.Path, req.Value, req.RequestID, ttl)
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(controlSetResponse{
				Accepted: false,
				Path:     req.Path,
				Detail:   err.Error(),
			})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		status := http.StatusOK
		if !res.Accepted {
			status = http.StatusConflict
		}
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(controlSetResponse{
			Accepted: res.Accepted,
			Readback: res.Readback,
			Path:     req.Path,
		})
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("gateway: control listen %s: %w", addr, err)
	}
	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shCtx)
	}()
	go func() {
		log.Printf("gateway: control API listening on %s (loopback + bearer required)", addr)
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("gateway: control server: %v", err)
		}
	}()
	return nil
}
