// Package resilmetrics is a stdlib-only Prometheus text exposition helper
// for edge telemetry daemons (DATA-RESILIENCE G5 / R-c).
//
// No client_golang dependency: counters/gauges are atomic and rendered
// as Prometheus text on GET /metrics.
package resilmetrics

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Counter is a monotonically increasing value.
type Counter struct{ v atomic.Uint64 }

func (c *Counter) Inc()         { c.v.Add(1) }
func (c *Counter) Add(n uint64) { c.v.Add(n) }
func (c *Counter) Get() uint64  { return c.v.Load() }

// Gauge is a point-in-time value (e.g. WAL bytes).
type Gauge struct{ v atomic.Int64 }

func (g *Gauge) Set(n int64) { g.v.Store(n) }
func (g *Gauge) Get() int64  { return g.v.Load() }

// LabeledCounter is a map of counters keyed by a single label value.
type LabeledCounter struct {
	mu   sync.Mutex
	by   map[string]*Counter
	keys []string
}

func (l *LabeledCounter) With(label string) *Counter {
	if label == "" {
		label = "unknown"
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.by == nil {
		l.by = make(map[string]*Counter)
	}
	if c, ok := l.by[label]; ok {
		return c
	}
	c := &Counter{}
	l.by[label] = c
	l.keys = append(l.keys, label)
	sort.Strings(l.keys)
	return c
}

func (l *LabeledCounter) forEach(fn func(label string, v uint64)) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, k := range l.keys {
		fn(k, l.by[k].Get())
	}
}

// WriteCounter emits one unlabeled counter series.
func WriteCounter(w io.Writer, name, help string, v uint64) {
	if help != "" {
		fmt.Fprintf(w, "# HELP %s %s\n", name, help)
	}
	fmt.Fprintf(w, "# TYPE %s counter\n", name)
	fmt.Fprintf(w, "%s %d\n", name, v)
}

// WriteLabeledCounter emits counter series with one label.
func WriteLabeledCounter(w io.Writer, name, help, labelKey string, lc *LabeledCounter) {
	if help != "" {
		fmt.Fprintf(w, "# HELP %s %s\n", name, help)
	}
	fmt.Fprintf(w, "# TYPE %s counter\n", name)
	lc.forEach(func(label string, v uint64) {
		fmt.Fprintf(w, "%s{%s=%q} %d\n", name, labelKey, label, v)
	})
}

// WriteGauge emits one gauge series.
func WriteGauge(w io.Writer, name, help string, v int64) {
	if help != "" {
		fmt.Fprintf(w, "# HELP %s %s\n", name, help)
	}
	fmt.Fprintf(w, "# TYPE %s gauge\n", name)
	fmt.Fprintf(w, "%s %d\n", name, v)
}

// Handler serves GET /metrics by calling write.
func Handler(write func(io.Writer)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		write(w)
	})
}

// Listen starts a background HTTP server on addr serving /metrics and /healthz.
// Empty addr is a no-op. Prefer loopback (e.g. 127.0.0.1:9102).
func Listen(addr string, write func(io.Writer)) (shutdown func(), err error) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return func() {}, nil
	}
	mux := http.NewServeMux()
	mux.Handle("/metrics", Handler(write))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("resilmetrics: listen %s: %w", addr, err)
	}
	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		log.Printf("resilmetrics: /metrics on %s", ln.Addr().String())
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("resilmetrics: serve: %v", err)
		}
	}()
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}, nil
}
