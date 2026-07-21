// Package webui provides a simple embedded dashboard for actions-runner-processor.
package webui

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
)

//go:embed templates/*
var templates embed.FS

// ScalerSnapshot for web UI (same interface as metrics).
type ScalerSnapshot interface {
	ActiveRunners() int
	MaxRunners() int
}

// Registry holds scaler state for the dashboard.
type Registry struct {
	scalers map[string]ScalerSnapshot
}

// NewRegistry creates a new web UI registry.
func NewRegistry() *Registry {
	return &Registry{
		scalers: make(map[string]ScalerSnapshot),
	}
}

// Register adds a scaler.
func (r *Registry) Register(scope string, s ScalerSnapshot) {
	r.scalers[scope] = s
}

type statusResponse struct {
	Scope         string `json:"scope"`
	ActiveRunners int    `json:"active_runners"`
	MaxRunners    int    `json:"max_runners"`
}

// Serve starts the Web UI HTTP server.
func Serve(ctx context.Context, addr string, registry *Registry) error {
	mux := http.NewServeMux()

	mux.HandleFunc("/api/status", func(w http.ResponseWriter, _ *http.Request) {
		var items []statusResponse
		for scope, s := range registry.scalers {
			items = append(items, statusResponse{
				Scope:         scope,
				ActiveRunners: s.ActiveRunners(),
				MaxRunners:    s.MaxRunners(),
			})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(items)
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		data, _ := templates.ReadFile("templates/dashboard.html")
		_, _ = w.Write(data)
	})

	srv := &http.Server{Addr: addr, Handler: mux}

	go func() {
		<-ctx.Done()
		_ = srv.Shutdown(context.Background())
	}()

	log.Printf("web UI listening on %s", addr)
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		return fmt.Errorf("webui serve: %w", err)
	}
	return nil
}
