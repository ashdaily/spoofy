// Command petstore is a throwaway API for demonstrating Spoofy.
//
// It exists so `docker compose up` needs nothing from the internet. It serves
// its own OpenAPI document at /openapi.yaml, implements every route in it, and
// behaves like a real service rather than a perfect one: latency varies, a
// small share of requests fail, and one endpoint is slow. A target that returns
// 200 in 1ms forever gives you a dashboard with nothing on it.
package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"
)

//go:embed openapi.yaml
var specFS embed.FS

func main() {
	addr := flag.String("addr", envOr("PETSTORE_ADDR", ":8080"), "listen address")
	errorRate := flag.Float64("error-rate", envFloat("PETSTORE_ERROR_RATE", 0.02), "fraction of requests that fail")
	flag.Parse()

	srv := &http.Server{
		Addr:              *addr,
		Handler:           routes(*errorRate),
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.Printf("petstore listening on %s (spec at /openapi.yaml, error rate %.1f%%)",
		*addr, *errorRate*100)

	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
	log.Print("petstore stopped")
}

func routes(errorRate float64) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /openapi.yaml", func(w http.ResponseWriter, r *http.Request) {
		body, err := specFS.ReadFile("openapi.yaml")
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/yaml")
		w.Write(body)
	})

	// Latency differs per route, since a flat histogram teaches nobody anything
	// about their dashboards.
	handle := func(pattern string, status int, base, jitter time.Duration) {
		mux.Handle(pattern, endpoint(status, base, jitter, errorRate))
	}

	handle("GET /v1/pets", http.StatusOK, 12*time.Millisecond, 25*time.Millisecond)
	handle("POST /v1/pets", http.StatusCreated, 30*time.Millisecond, 40*time.Millisecond)
	handle("GET /v1/pets/{petId}", http.StatusOK, 8*time.Millisecond, 12*time.Millisecond)
	handle("DELETE /v1/pets/{petId}", http.StatusNoContent, 20*time.Millisecond, 20*time.Millisecond)
	handle("PATCH /v1/pets/{petId}", http.StatusOK, 25*time.Millisecond, 30*time.Millisecond)
	// The slow one. Every real API has one, and finding it on a latency
	// heatmap is worth being able to practise.
	handle("GET /v1/pets/{petId}/photos", http.StatusOK, 120*time.Millisecond, 180*time.Millisecond)
	handle("GET /v1/admin/users", http.StatusOK, 15*time.Millisecond, 20*time.Millisecond)
	// Health checks never fail and never lag.
	mux.Handle("GET /v1/health", endpoint(http.StatusOK, time.Millisecond, 0, 0))

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"error": fmt.Sprintf("no route for %s %s", r.Method, r.URL.Path),
		})
	})

	return logging(mux)
}

func endpoint(status int, base, jitter time.Duration, errorRate float64) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		delay := base
		if jitter > 0 {
			// Squaring gives a long right tail, which is what real latency looks
			// like and what makes p95 differ from p50.
			f := rand.Float64()
			delay += time.Duration(f * f * float64(jitter))
		}
		time.Sleep(delay)

		if errorRate > 0 && rand.Float64() < errorRate {
			writeJSON(w, http.StatusInternalServerError, map[string]any{
				"error": "synthetic failure",
			})
			return
		}

		if status == http.StatusNoContent {
			w.WriteHeader(status)
			return
		}
		writeJSON(w, status, map[string]any{
			"ok":   true,
			"path": r.URL.Path,
		})
	})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// logging stays minimal. At a few hundred requests a second, a line per request
// is the loudest thing in the compose output. The counter is atomic because
// handlers run concurrently.
func logging(next http.Handler) http.Handler {
	var count atomic.Int64
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n := count.Add(1); n%500 == 0 {
			log.Printf("served %d requests", n)
		}
		next.ServeHTTP(w, r)
	})
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envFloat(key string, fallback float64) float64 {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	var out float64
	if _, err := fmt.Sscanf(v, "%f", &out); err != nil {
		return fallback
	}
	return out
}
