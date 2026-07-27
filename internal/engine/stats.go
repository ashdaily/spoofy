package engine

import (
	"sort"
	"sync"
	"time"

	"github.com/ashdaily/spoofy/internal/client"
)

// latencySamples is the size of the ring buffer backing percentile estimates.
// Fixed, not growing: this daemon runs for weeks, and an unbounded slice of
// every latency ever observed is the most obvious way to leak memory slowly
// enough that nobody notices until the pod is OOM-killed.
const latencySamples = 4096

// rpsWindow is how many one-second buckets are kept for the achieved-rate
// readout.
const rpsWindow = 60

// Stats accumulates what the live view shows.
//
// Prometheus is the durable record; this exists so the terminal can show
// something useful without scraping itself. It is deliberately bounded in
// memory regardless of how long the process runs.
type Stats struct {
	mu sync.Mutex

	started time.Time
	total   int64
	byClass map[string]int64
	errors  int64

	// latencies is a ring of recent samples, in seconds.
	latencies [latencySamples]float64
	latCount  int
	latNext   int

	// perSecond is a ring of counts indexed by unix second, so the achieved
	// rate reflects recent traffic rather than the lifetime average.
	perSecond [rpsWindow]int64
	perSecSec [rpsWindow]int64
}

// NewStats returns Stats anchored at now.
func NewStats(now time.Time) *Stats {
	return &Stats{
		started: now,
		byClass: make(map[string]int64, 5),
	}
}

// Record folds one result into the running totals.
func (s *Stats) Record(r client.Result, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.total++
	s.byClass[r.StatusClass()]++
	if r.Err != nil {
		s.errors++
	}

	if r.Err == nil {
		s.latencies[s.latNext] = r.Duration.Seconds()
		s.latNext = (s.latNext + 1) % latencySamples
		if s.latCount < latencySamples {
			s.latCount++
		}
	}

	sec := now.Unix()
	slot := int(sec % rpsWindow)
	if s.perSecSec[slot] != sec {
		s.perSecSec[slot] = sec
		s.perSecond[slot] = 0
	}
	s.perSecond[slot]++
}

// Snapshot is an immutable view of the stats at one moment.
type Snapshot struct {
	Elapsed     time.Duration
	Total       int64
	Errors      int64
	ByClass     map[string]int64
	AchievedRPS float64
	P50, P95    time.Duration
}

// SuccessRate is the fraction of requests that were 2xx or 3xx.
func (s Snapshot) SuccessRate() float64 {
	if s.Total == 0 {
		return 0
	}
	ok := s.ByClass["2xx"] + s.ByClass["3xx"]
	return float64(ok) / float64(s.Total)
}

// Snapshot copies the current state.
func (s *Stats) Snapshot(now time.Time) Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := Snapshot{
		Elapsed: now.Sub(s.started),
		Total:   s.total,
		Errors:  s.errors,
		ByClass: make(map[string]int64, len(s.byClass)),
	}
	for k, v := range s.byClass {
		out.ByClass[k] = v
	}

	// Achieved rate over the last 5 whole seconds, excluding the current
	// partial one — including it makes the number sag at every refresh.
	const window = 5
	var sum int64
	current := now.Unix()
	for i := 1; i <= window; i++ {
		sec := current - int64(i)
		slot := int(((sec % rpsWindow) + rpsWindow) % rpsWindow)
		if s.perSecSec[slot] == sec {
			sum += s.perSecond[slot]
		}
	}
	out.AchievedRPS = float64(sum) / window

	if s.latCount > 0 {
		sorted := make([]float64, s.latCount)
		copy(sorted, s.latencies[:s.latCount])
		sort.Float64s(sorted)
		out.P50 = percentile(sorted, 0.50)
		out.P95 = percentile(sorted, 0.95)
	}

	return out
}

func percentile(sorted []float64, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(p * float64(len(sorted)))
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return time.Duration(sorted[idx] * float64(time.Second))
}
