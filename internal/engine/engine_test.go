package engine

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ashdaily/spoofy/internal/metrics"
	"github.com/ashdaily/spoofy/internal/openapi"
	"github.com/ashdaily/spoofy/internal/settings"
)

const specPath = "../openapi/testdata/petstore.yaml"

// petstore stands up a server implementing every route in the test spec and
// records what it was asked for.
type petstore struct {
	*httptest.Server
	mu   sync.Mutex
	hits map[string]int
}

func newPetstore(t *testing.T, status int) *petstore {
	t.Helper()

	p := &petstore{hits: map[string]int{}}
	mux := http.NewServeMux()

	for _, pattern := range []string{
		"GET /pets", "POST /pets",
		"GET /pets/{petId}", "DELETE /pets/{petId}", "PATCH /pets/{petId}",
		"GET /pets/{petId}/photos", "GET /admin/users", "GET /health",
	} {
		pattern := pattern
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			p.mu.Lock()
			p.hits[pattern]++
			p.mu.Unlock()
			w.WriteHeader(status)
			w.Write([]byte(`{"ok":true}`))
		})
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		p.hits["UNROUTED"]++
		p.mu.Unlock()
		http.Error(w, "no route", http.StatusNotFound)
	})

	p.Server = httptest.NewServer(mux)
	t.Cleanup(p.Close)
	return p
}

func (p *petstore) count(pattern string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.hits[pattern]
}

func (p *petstore) total() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	var n int
	for _, v := range p.hits {
		n += v
	}
	return n
}

func baseConfig(target string) *settings.Config {
	cfg := settings.Default()
	cfg.Target = target
	cfg.Spec = specPath
	cfg.Traffic.Rate = 200
	cfg.Traffic.Concurrency = 4
	cfg.Seed = 7
	return &cfg
}

func buildEngine(t *testing.T, cfg *settings.Config) (*Engine, *metrics.Metrics) {
	t.Helper()

	spec, err := openapi.Load(context.Background(), specPath)
	if err != nil {
		t.Fatalf("loading spec: %v", err)
	}
	selected, _, _ := spec.Select(cfg)

	mx := metrics.New()
	eng, err := New(cfg, selected, mx, time.Now())
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	return eng, mx
}

// The whole pipeline in one test: spec to scheduler to generator to client to
// a server that only answers real routes. Any 404 means some layer produced a
// request the API does not serve.
func TestEngineGeneratesRoutableTraffic(t *testing.T) {
	store := newPetstore(t, http.StatusOK)
	cfg := baseConfig(store.URL)

	eng, _ := buildEngine(t, cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()
	eng.Run(ctx)

	if store.count("UNROUTED") > 0 {
		t.Errorf("%d requests hit no route", store.count("UNROUTED"))
	}
	if store.total() == 0 {
		t.Fatal("no traffic was generated at all")
	}

	snap := eng.Stats().Snapshot(time.Now())
	if snap.Total == 0 {
		t.Fatal("stats recorded nothing")
	}
	if snap.SuccessRate() < 0.99 {
		t.Errorf("success rate %.2f against an all-200 server", snap.SuccessRate())
	}
}

// Read-only is the default and it must actually hold: a daemon quietly issuing
// DELETEs against staging for a week is a data-loss incident.
func TestReadOnlyByDefault(t *testing.T) {
	store := newPetstore(t, http.StatusOK)
	cfg := baseConfig(store.URL)

	eng, _ := buildEngine(t, cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	eng.Run(ctx)

	for _, mutating := range []string{
		"POST /pets", "DELETE /pets/{petId}", "PATCH /pets/{petId}",
	} {
		if got := store.count(mutating); got != 0 {
			t.Errorf("%s was called %d times without allow_writes", mutating, got)
		}
	}
	if store.count("GET /pets") == 0 {
		t.Error("no reads happened either; the test proves nothing")
	}
}

func TestAllowWritesEnablesMutatingMethods(t *testing.T) {
	store := newPetstore(t, http.StatusOK)
	cfg := baseConfig(store.URL)
	cfg.Safety.AllowWrites = true

	eng, _ := buildEngine(t, cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 900*time.Millisecond)
	defer cancel()
	eng.Run(ctx)

	var mutations int
	for _, pattern := range []string{"POST /pets", "DELETE /pets/{petId}", "PATCH /pets/{petId}"} {
		mutations += store.count(pattern)
	}
	if mutations == 0 {
		t.Error("allow_writes was set but no mutating request was sent")
	}
	if store.count("UNROUTED") > 0 {
		t.Errorf("%d write requests hit no route", store.count("UNROUTED"))
	}
}

func TestEndpointRulesAreHonoured(t *testing.T) {
	store := newPetstore(t, http.StatusOK)
	cfg := baseConfig(store.URL)
	cfg.Endpoints = []settings.EndpointRule{
		{Match: "/admin/*", Skip: true},
		{Match: "/health", Skip: true},
	}

	eng, _ := buildEngine(t, cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()
	eng.Run(ctx)

	if got := store.count("GET /admin/users"); got != 0 {
		t.Errorf("/admin/users was skipped in config but called %d times", got)
	}
	if got := store.count("GET /health"); got != 0 {
		t.Errorf("/health was skipped in config but called %d times", got)
	}
	if store.count("GET /pets") == 0 {
		t.Error("unfiltered endpoints saw no traffic")
	}
}

// Weighting is what lets a config approximate a real traffic mix rather than
// hitting every endpoint uniformly.
func TestWeightsBiasSelection(t *testing.T) {
	store := newPetstore(t, http.StatusOK)
	cfg := baseConfig(store.URL)
	cfg.Traffic.Rate = 400
	cfg.Endpoints = []settings.EndpointRule{{Match: "/pets", Weight: 20}}

	eng, _ := buildEngine(t, cfg)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	eng.Run(ctx)

	heavy := store.count("GET /pets")
	light := store.count("GET /health")

	if heavy == 0 || light == 0 {
		t.Fatalf("need traffic on both endpoints to compare: heavy=%d light=%d", heavy, light)
	}
	// Weight 20 against 1; anything under 3x means weighting is not applied.
	if float64(heavy) < float64(light)*3 {
		t.Errorf("weighted endpoint got %d hits vs %d unweighted; expected a clear bias", heavy, light)
	}
}

// gaugeValue reads a gauge straight out of the registry, which both answers the
// question and confirms the metric is actually exported.
func gaugeValue(t *testing.T, mx *metrics.Metrics, name string) float64 {
	t.Helper()

	families, err := mx.Registry().Gather()
	if err != nil {
		t.Fatalf("gathering metrics: %v", err)
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			return m.GetGauge().GetValue()
		}
	}
	t.Fatalf("metric %q was never exported", name)
	return 0
}

// Run must not return while requests are still outstanding. A caller cancelling
// on SIGTERM depends on this for a clean drain instead of severed connections
// mid-flight.
//
// The assertion is on Spoofy's own in-flight gauge, not on a server-side
// counter: an HTTP handler keeps running after its client disconnects, so
// server-side state would report work in flight that Spoofy has already
// abandoned.
func TestRunDrainsOnCancel(t *testing.T) {
	var served atomic.Int64

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served.Add(1)
		time.Sleep(20 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := baseConfig(srv.URL)
	eng, mx := buildEngine(t, cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	eng.Run(ctx)

	if got := gaugeValue(t, mx, "spoofy_requests_in_flight"); got != 0 {
		t.Errorf("Run returned with %v requests still in flight", got)
	}
	if served.Load() == 0 {
		t.Error("no requests were ever sent")
	}
}

// Concurrency is a ceiling, not a target. Exceeding it means the worker pool
// leaked, which against a real staging environment is how a traffic generator
// becomes an outage.
func TestConcurrencyIsBounded(t *testing.T) {
	var inFlight, maxSeen atomic.Int64

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := inFlight.Add(1)
		for {
			old := maxSeen.Load()
			if n <= old || maxSeen.CompareAndSwap(old, n) {
				break
			}
		}
		time.Sleep(15 * time.Millisecond)
		inFlight.Add(-1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := baseConfig(srv.URL)
	cfg.Traffic.Concurrency = 3
	cfg.Traffic.Rate = 500 // far more than the workers can deliver

	eng, _ := buildEngine(t, cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	eng.Run(ctx)

	if got := maxSeen.Load(); got > 3 {
		t.Errorf("saw %d concurrent requests, configured ceiling is 3", got)
	}
}

func TestDryRunSendsNothing(t *testing.T) {
	store := newPetstore(t, http.StatusOK)
	cfg := baseConfig(store.URL)

	eng, _ := buildEngine(t, cfg)

	commands, err := eng.DryRun(context.Background())
	if err != nil {
		t.Fatalf("DryRun: %v", err)
	}

	if len(commands) != len(eng.Operations()) {
		t.Errorf("got %d commands for %d operations", len(commands), len(eng.Operations()))
	}
	if store.total() != 0 {
		t.Errorf("dry run sent %d real requests", store.total())
	}
	for _, c := range commands {
		if !strings.HasPrefix(c, "curl ") {
			t.Errorf("command is not a curl invocation: %q", c)
		}
		if strings.ContainsAny(c, "{}") && strings.Contains(c, "{petId}") {
			t.Errorf("dry run shows an unsubstituted path: %q", c)
		}
	}
}

func TestNewRejectsAnEmptyOperationSet(t *testing.T) {
	cfg := baseConfig("http://localhost:1")
	_, err := New(cfg, nil, metrics.New(), time.Now())
	if err == nil {
		t.Fatal("expected an error when every operation was filtered out")
	}
	if !strings.Contains(err.Error(), "filtering") {
		t.Errorf("error should explain why nothing is left, got: %v", err)
	}
}

// A target that is down must not stop the engine. It should back off and keep
// trying, since the target coming back is the normal end of a deploy.
func TestSurvivesAnUnreachableTarget(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close() // nothing listening

	cfg := baseConfig(url)
	cfg.Traffic.Timeout = settings.Duration(200 * time.Millisecond)

	eng, _ := buildEngine(t, cfg)

	done := make(chan struct{})
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()

	go func() {
		eng.Run(ctx)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("engine hung against an unreachable target")
	}

	if eng.Client().Up() {
		t.Error("target should be reported down")
	}
}
