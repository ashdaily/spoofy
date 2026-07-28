package scheduler

import (
	"context"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/time/rate"
)

// DefaultAdjustInterval is how often pacing is re-derived from the shape. Finer
// than any shape needs, while still reacting promptly at a spike boundary.
const DefaultAdjustInterval = time.Second

// Scheduler paces work according to a Shape.
//
// It owns how fast (the shape, re-evaluated on a ticker) and how many at once
// (a fixed worker pool). They stay separate because concurrency describes the
// target's tolerance while rate describes the traffic you want to see.
type Scheduler struct {
	shape       Shape
	concurrency int

	// now and adjustInterval are injectable so tests can drive a 24-hour cycle
	// without waiting 24 hours.
	now            func() time.Time
	adjustInterval time.Duration

	// currentRate is published for the live view, stored as bits because
	// sync/atomic has no float64.
	currentRate atomic.Uint64
	completed   atomic.Int64
}

// New returns a Scheduler driving concurrency workers at the shape's rate.
func New(shape Shape, concurrency int) *Scheduler {
	if concurrency < 1 {
		concurrency = 1
	}
	return &Scheduler{
		shape:          shape,
		concurrency:    concurrency,
		now:            time.Now,
		adjustInterval: DefaultAdjustInterval,
	}
}

// WithClock replaces the time source. Tests only.
func (s *Scheduler) WithClock(now func() time.Time, adjustInterval time.Duration) *Scheduler {
	s.now = now
	if adjustInterval > 0 {
		s.adjustInterval = adjustInterval
	}
	return s
}

// CurrentRate reports the rate currently being targeted, in requests/sec.
func (s *Scheduler) CurrentRate() float64 {
	return math.Float64frombits(s.currentRate.Load())
}

// Completed reports how many units of work have finished.
func (s *Scheduler) Completed() int64 { return s.completed.Load() }

// Shape returns the configured shape, for startup output.
func (s *Scheduler) Shape() Shape { return s.shape }

// Run drives work at the shaped rate until ctx is cancelled.
//
// It returns only once every in-flight worker has finished, so cancelling on
// SIGTERM drains cleanly instead of severing connections.
func (s *Scheduler) Run(ctx context.Context, work func(context.Context)) {
	start := s.now()

	initial := s.shape.RateAt(start, start)
	s.setRate(initial)

	// Burst 1 gives strict pacing. With several workers waiting, the limiter
	// hands out permits one at a time instead of releasing a clump at each
	// interval boundary, which would show up in Grafana as a sawtooth the
	// target never experienced.
	limiter := rate.NewLimiter(rate.Limit(initial), 1)

	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		s.adjust(ctx, limiter, start)
	}()

	for i := 0; i < s.concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				if err := limiter.Wait(ctx); err != nil {
					return // context cancelled
				}
				// Wait can return successfully at the same moment cancellation
				// arrives, and one extra request during shutdown makes a drain
				// look unclean.
				if ctx.Err() != nil {
					return
				}
				work(ctx)
				s.completed.Add(1)
			}
		}()
	}

	wg.Wait()
}

// adjust re-derives the target rate from the shape on a ticker.
func (s *Scheduler) adjust(ctx context.Context, limiter *rate.Limiter, start time.Time) {
	ticker := time.NewTicker(s.adjustInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r := s.shape.RateAt(s.now(), start)
			limiter.SetLimit(rate.Limit(r))
			s.setRate(r)
		}
	}
}

func (s *Scheduler) setRate(r float64) {
	s.currentRate.Store(math.Float64bits(r))
}
