// Package metrics publishes what Spoofy did, in Prometheus form.
//
// This is not incidental instrumentation — it is half the product. Spoofy
// exists to put signal into a monitoring stack, and its own /metrics endpoint
// is what lets you compare "traffic Spoofy sent" against "traffic the app
// reports receiving". A gap between those two numbers is a real finding:
// dropped requests, broken instrumentation, a misconfigured scrape.
package metrics

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/ashdaily/spoofy/internal/client"
)

// Namespace prefixes every metric.
const Namespace = "spoofy"

// Metrics owns the collectors and the registry they live in.
//
// A dedicated registry rather than the global default: the global one carries
// whatever any imported library decided to register, and a tool whose job is
// producing clean signal should not export surprises.
type Metrics struct {
	registry *prometheus.Registry

	requests *prometheus.CounterVec
	duration *prometheus.HistogramVec
	errors   *prometheus.CounterVec
	bytes    *prometheus.CounterVec

	targetUp    prometheus.Gauge
	targetRate  prometheus.Gauge
	inFlight    prometheus.Gauge
	buildErrors prometheus.Counter
}

// New builds the collectors and registers them.
func New() *Metrics {
	m := &Metrics{registry: prometheus.NewRegistry()}

	// Labels are deliberately few and bounded. `path` is always the templated
	// form ("/pets/{petId}"); labelling with concrete URLs would add a new time
	// series per pet id and take Prometheus down within a day.
	m.requests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: Namespace,
		Name:      "requests_total",
		Help:      "Requests sent, by method, templated path, and status class.",
	}, []string{"method", "path", "status", "class"})

	m.duration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: Namespace,
		Name:      "request_duration_seconds",
		Help:      "Request latency as observed by Spoofy.",
		// Buckets span 1ms to ~16s: local calls at the bottom, timeouts at the
		// top. The default buckets bunch around 100ms and lose both ends.
		Buckets: []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 16},
	}, []string{"method", "path"})

	m.errors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: Namespace,
		Name:      "errors_total",
		Help:      "Transport-level failures, by kind. Not HTTP error responses.",
	}, []string{"kind"})

	m.bytes = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: Namespace,
		Name:      "response_bytes_total",
		Help:      "Response bytes read, by templated path.",
	}, []string{"path"})

	m.targetUp = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: Namespace,
		Name:      "target_up",
		Help:      "1 when the target is answering, 0 when it is not.",
	})

	m.targetRate = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: Namespace,
		Name:      "target_rate",
		Help:      "Requests per second the traffic shape is currently asking for.",
	})

	m.inFlight = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: Namespace,
		Name:      "requests_in_flight",
		Help:      "Requests currently awaiting a response.",
	})

	m.buildErrors = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: Namespace,
		Name:      "request_build_errors_total",
		Help:      "Requests that could not be constructed from the spec.",
	})

	m.registry.MustRegister(
		m.requests, m.duration, m.errors, m.bytes,
		m.targetUp, m.targetRate, m.inFlight, m.buildErrors,
		// Go runtime and process collectors: the soak test needs to see whether
		// RSS is flat over a week, and that answer belongs on the same endpoint
		// as everything else.
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	m.targetUp.Set(1)
	return m
}

// Registry exposes the registry, for tests and for embedding.
func (m *Metrics) Registry() *prometheus.Registry { return m.registry }

// Record observes one completed request.
func (m *Metrics) Record(r client.Result) {
	if r.Err != nil {
		m.errors.WithLabelValues(errorKind(r.Err)).Inc()
		m.requests.WithLabelValues(r.Method, r.Path, "error", "error").Inc()
		return
	}

	m.requests.WithLabelValues(r.Method, r.Path, statusText(r.StatusCode), r.StatusClass()).Inc()
	m.duration.WithLabelValues(r.Method, r.Path).Observe(r.Duration.Seconds())
	if r.BytesRead > 0 {
		m.bytes.WithLabelValues(r.Path).Add(float64(r.BytesRead))
	}
}

// RecordBuildError counts a request that could not be constructed at all.
func (m *Metrics) RecordBuildError() { m.buildErrors.Inc() }

// SetTargetUp publishes target reachability.
func (m *Metrics) SetTargetUp(up bool) {
	if up {
		m.targetUp.Set(1)
		return
	}
	m.targetUp.Set(0)
}

// SetTargetRate publishes the rate the shape is currently asking for.
func (m *Metrics) SetTargetRate(r float64) { m.targetRate.Set(r) }

// IncInFlight and DecInFlight bracket an in-flight request.
func (m *Metrics) IncInFlight() { m.inFlight.Inc() }
func (m *Metrics) DecInFlight() { m.inFlight.Dec() }

// Handler returns the /metrics HTTP handler.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

// Serve runs the metrics endpoint until ctx is cancelled.
//
// It also serves /healthz, because anything deployed to Kubernetes needs a
// probe target and making operators invent one is a papercut.
func (m *Metrics) Serve(ctx context.Context, addr string) error {
	mux := http.NewServeMux()
	mux.Handle("/metrics", m.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/metrics", http.StatusFound)
	})

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// errorKind buckets transport failures into a small, bounded label set.
// Using err.Error() directly would put a unique label on every failure — a
// cardinality bomb on a flaky network.
func errorKind(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	}

	msg := err.Error()
	switch {
	case contains(msg, "connection refused"):
		return "connection_refused"
	case contains(msg, "no such host"), contains(msg, "dns"):
		return "dns"
	case contains(msg, "timeout"), contains(msg, "deadline exceeded"):
		return "timeout"
	case contains(msg, "connection reset"):
		return "connection_reset"
	case contains(msg, "EOF"):
		return "eof"
	case contains(msg, "tls"), contains(msg, "certificate"):
		return "tls"
	default:
		return "other"
	}
}

func contains(haystack, needle string) bool {
	if len(needle) > len(haystack) {
		return false
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if equalFold(haystack[i:i+len(needle)], needle) {
			return true
		}
	}
	return false
}

func equalFold(a, b string) bool {
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}

// statusText renders a status code as a bounded label.
func statusText(code int) string {
	if code < 100 || code > 599 {
		return "unknown"
	}
	return itoa(code)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [8]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
