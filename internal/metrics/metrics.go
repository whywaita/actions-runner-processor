// Package metrics provides a Prometheus metrics exporter for the
// runner-listener.
package metrics

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Registry holds all scaler metrics across multiple installations.
type Registry struct {
	mu       sync.RWMutex
	scalers  map[string]ScalerSnapshot // scope → snapshot
}

// ScalerSnapshot provides read-only metrics state from a scaler.
type ScalerSnapshot interface {
	ActiveRunners() int
	MaxRunners() int
}

// NewRegistry creates a new metrics registry.
func NewRegistry() *Registry {
	return &Registry{
		scalers: make(map[string]ScalerSnapshot),
	}
}

// Register adds a scaler to the registry under the given scope name.
func (r *Registry) Register(scope string, s ScalerSnapshot) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.scalers[scope] = s
}

var (
	activeRunners = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "runner_listener_active_runners",
			Help: "Number of active runners per scale set.",
		},
		[]string{"scope"},
	)
	maxRunners = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "runner_listener_max_runners",
			Help: "Maximum runners per scale set.",
		},
		[]string{"scope"},
	)
)

func init() {
	prometheus.MustRegister(activeRunners, maxRunners)
}

// collect updates Prometheus gauges from the registry state.
func (r *Registry) collect() {
	r.mu.RLock()
	defer r.mu.RUnlock()

	for scope, s := range r.scalers {
		activeRunners.WithLabelValues(scope).Set(float64(s.ActiveRunners()))
		maxRunners.WithLabelValues(scope).Set(float64(s.MaxRunners()))
	}
}

// Serve starts the Prometheus metrics HTTP server.
func Serve(ctx context.Context, addr string, registry *Registry) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		registry.collect()
		promhttp.Handler().ServeHTTP(w, r)
	})

	srv := &http.Server{Addr: addr, Handler: mux}

	go func() {
		<-ctx.Done()
		_ = srv.Shutdown(context.Background())
	}()

	log.Printf("metrics server listening on %s", addr)
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		return fmt.Errorf("metrics serve: %w", err)
	}
	return nil
}
