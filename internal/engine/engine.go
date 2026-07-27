// Package engine wires the pieces together and runs the traffic loop.
//
// Everything else in Spoofy is a component with one job. This is where they
// meet: the spec supplies operations, the scheduler decides when, the
// generator builds a request, the client sends it, and metrics and stats record
// what happened.
package engine

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"time"

	"github.com/ashdaily/spoofy/internal/client"
	"github.com/ashdaily/spoofy/internal/generator"
	"github.com/ashdaily/spoofy/internal/metrics"
	"github.com/ashdaily/spoofy/internal/openapi"
	"github.com/ashdaily/spoofy/internal/randomizer"
	"github.com/ashdaily/spoofy/internal/scheduler"
	"github.com/ashdaily/spoofy/internal/settings"
)

// Engine runs a traffic session.
type Engine struct {
	cfg   *settings.Config
	ops   []*openapi.Operation
	sched *scheduler.Scheduler
	http  *client.Client
	mx    *metrics.Metrics
	stats *Stats

	// workers is a pool of per-goroutine state. The randomizer and generator
	// are not safe for concurrent use — matching math/rand — so rather than
	// putting a mutex on the hot path, each concurrent worker borrows its own.
	workers chan *worker

	// cumulativeWeights supports weighted operation selection without
	// re-summing the weights on every request.
	cumulativeWeights []float64
	totalWeight       float64
}

type worker struct {
	gen *generator.Generator
	rng *rand.Rand
}

// New builds an Engine from a validated config and a selected operation set.
func New(cfg *settings.Config, ops []*openapi.Operation, mx *metrics.Metrics, now time.Time) (*Engine, error) {
	if len(ops) == 0 {
		return nil, fmt.Errorf("no operations left to exercise after filtering; " +
			"check your endpoints rules and whether writes are allowed")
	}

	seed := cfg.Seed
	if seed == 0 {
		seed = now.UnixNano()
	}

	e := &Engine{
		cfg:     cfg,
		ops:     ops,
		sched:   scheduler.New(scheduler.FromConfig(cfg), cfg.Traffic.Concurrency),
		http:    client.New(cfg.Traffic.Timeout.D(), cfg.Traffic.Concurrency),
		mx:      mx,
		stats:   NewStats(now),
		workers: make(chan *worker, cfg.Traffic.Concurrency),
	}

	for i := 0; i < cfg.Traffic.Concurrency; i++ {
		// Each worker's seed is derived from the base, so a run is reproducible
		// as a whole even though the workers are independent.
		workerSeed := seed + int64(i)*7919
		gen, err := generator.New(cfg.Target, randomizer.New(workerSeed), cfg.Auth)
		if err != nil {
			return nil, err
		}
		e.workers <- &worker{gen: gen, rng: rand.New(rand.NewSource(workerSeed))}
	}

	e.buildWeights()
	return e, nil
}

func (e *Engine) buildWeights() {
	e.cumulativeWeights = make([]float64, len(e.ops))
	var running float64
	for i, op := range e.ops {
		running += e.cfg.WeightFor(op.Path)
		e.cumulativeWeights[i] = running
	}
	e.totalWeight = running
}

// Stats exposes the live counters for rendering.
func (e *Engine) Stats() *Stats { return e.stats }

// Scheduler exposes the scheduler, for the live view's rate readout.
func (e *Engine) Scheduler() *scheduler.Scheduler { return e.sched }

// Client exposes the HTTP client, for target-health readouts.
func (e *Engine) Client() *client.Client { return e.http }

// Operations returns the operations being exercised.
func (e *Engine) Operations() []*openapi.Operation { return e.ops }

// Run generates traffic until ctx is cancelled, then drains.
func (e *Engine) Run(ctx context.Context) {
	// Keep target_up fresh even while idle, so a scrape during a quiet diurnal
	// trough still reports health rather than a stale value.
	go e.publishHealth(ctx)

	e.sched.Run(ctx, e.once)
}

// once performs a single request: borrow a worker, pick an operation, build,
// send, record.
func (e *Engine) once(ctx context.Context) {
	var w *worker
	select {
	case w = <-e.workers:
	case <-ctx.Done():
		return
	}
	defer func() { e.workers <- w }()

	op := e.pick(w.rng)

	req, err := w.gen.Build(ctx, op)
	if err != nil {
		// A spec that cannot produce a request is a persistent condition, not a
		// transient one. Counting it separately keeps it out of the HTTP error
		// rate, where it would look like the target misbehaving.
		e.mx.RecordBuildError()
		return
	}

	e.mx.IncInFlight()
	result := e.http.Do(ctx, req, client.OperationRef{
		ID:     op.ID,
		Method: op.Method,
		Path:   op.Path,
	})
	e.mx.DecInFlight()

	// A request cut short by shutdown is not a data point about the target.
	if ctx.Err() != nil && result.Err != nil {
		return
	}

	e.mx.Record(result)
	e.stats.Record(result, time.Now())
}

// pick chooses an operation, honouring configured weights.
func (e *Engine) pick(rng *rand.Rand) *openapi.Operation {
	if e.totalWeight <= 0 || len(e.ops) == 1 {
		return e.ops[rng.Intn(len(e.ops))]
	}

	target := rng.Float64() * e.totalWeight
	// Linear scan. Operation counts are in the tens or low hundreds, where this
	// beats a binary search on cache behaviour and is far easier to read.
	for i, cumulative := range e.cumulativeWeights {
		if target < cumulative {
			return e.ops[i]
		}
	}
	return e.ops[len(e.ops)-1]
}

// publishHealth mirrors client and scheduler state into Prometheus gauges.
func (e *Engine) publishHealth(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			e.mx.SetTargetUp(e.http.Up())
			e.mx.SetTargetRate(e.sched.CurrentRate())
		}
	}
}

// DryRun builds one request per operation and returns them rendered as curl
// commands, without sending anything.
//
// This is the answer to "what will this actually do to my environment", and it
// has to be answerable before the first request rather than after.
func (e *Engine) DryRun(ctx context.Context) ([]string, error) {
	w := <-e.workers
	defer func() { e.workers <- w }()

	out := make([]string, 0, len(e.ops))
	for _, op := range e.ops {
		req, err := w.gen.Build(ctx, op)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", op, err)
		}

		// GetBody is set by http.NewRequest for in-memory bodies, and rereading
		// through it leaves req.Body intact.
		var body []byte
		if req.GetBody != nil {
			if rc, err := req.GetBody(); err == nil {
				body, _ = io.ReadAll(rc)
				rc.Close()
			}
		}
		out = append(out, generator.Curl(req, body))
	}
	return out, nil
}
