package engine

import (
	"testing"
	"time"

	"github.com/ashdaily/spoofy/internal/client"
)

func result(status int, d time.Duration) client.Result {
	return client.Result{Method: "GET", Path: "/pets", StatusCode: status, Duration: d}
}

func TestSnapshotCountsByClass(t *testing.T) {
	now := time.Now()
	s := NewStats(now)

	for i := 0; i < 10; i++ {
		s.Record(result(200, 10*time.Millisecond), now)
	}
	for i := 0; i < 3; i++ {
		s.Record(result(404, 5*time.Millisecond), now)
	}
	s.Record(result(500, 50*time.Millisecond), now)
	s.Record(client.Result{Method: "GET", Path: "/pets", Err: context_DeadlineExceeded()}, now)

	snap := s.Snapshot(now)

	if snap.Total != 15 {
		t.Errorf("Total = %d, want 15", snap.Total)
	}
	if snap.ByClass["2xx"] != 10 {
		t.Errorf("2xx = %d, want 10", snap.ByClass["2xx"])
	}
	if snap.ByClass["4xx"] != 3 {
		t.Errorf("4xx = %d, want 3", snap.ByClass["4xx"])
	}
	if snap.ByClass["5xx"] != 1 {
		t.Errorf("5xx = %d, want 1", snap.ByClass["5xx"])
	}
	if snap.Errors != 1 {
		t.Errorf("Errors = %d, want 1", snap.Errors)
	}

	// 10 of 15 were 2xx.
	if got := snap.SuccessRate(); got < 0.66 || got > 0.67 {
		t.Errorf("SuccessRate = %v, want ~0.667", got)
	}
}

func TestSuccessRateOnAnEmptySnapshot(t *testing.T) {
	s := NewStats(time.Now())
	if got := s.Snapshot(time.Now()).SuccessRate(); got != 0 {
		t.Errorf("SuccessRate with no data = %v, want 0", got)
	}
}

func TestPercentiles(t *testing.T) {
	now := time.Now()
	s := NewStats(now)

	// 1ms..100ms, uniform.
	for i := 1; i <= 100; i++ {
		s.Record(result(200, time.Duration(i)*time.Millisecond), now)
	}

	snap := s.Snapshot(now)
	if snap.P50 < 45*time.Millisecond || snap.P50 > 55*time.Millisecond {
		t.Errorf("P50 = %v, want about 50ms", snap.P50)
	}
	if snap.P95 < 90*time.Millisecond || snap.P95 > 100*time.Millisecond {
		t.Errorf("P95 = %v, want about 95ms", snap.P95)
	}
	if snap.P95 <= snap.P50 {
		t.Errorf("P95 (%v) should exceed P50 (%v)", snap.P95, snap.P50)
	}
}

// Failed requests have no meaningful latency; folding a near-zero connection
// refusal into the histogram would drag the percentiles down and make an
// outage look fast.
func TestErrorsAreExcludedFromLatency(t *testing.T) {
	now := time.Now()
	s := NewStats(now)

	for i := 0; i < 50; i++ {
		s.Record(result(200, 100*time.Millisecond), now)
	}
	for i := 0; i < 50; i++ {
		s.Record(client.Result{Method: "GET", Path: "/pets", Err: context_DeadlineExceeded(), Duration: 0}, now)
	}

	snap := s.Snapshot(now)
	if snap.P50 < 95*time.Millisecond {
		t.Errorf("P50 = %v; failed requests appear to be dragging latency down", snap.P50)
	}
}

// Memory has to stay flat over weeks. An unbounded latency slice is the most
// obvious way to leak slowly enough that nobody notices until the pod is
// OOM-killed.
func TestLatencyMemoryIsBounded(t *testing.T) {
	now := time.Now()
	s := NewStats(now)

	for i := 0; i < latencySamples*10; i++ {
		s.Record(result(200, time.Duration(i%200)*time.Millisecond), now)
	}

	if s.latCount > latencySamples {
		t.Errorf("kept %d latency samples, cap is %d", s.latCount, latencySamples)
	}
	// Totals must still be exact even though samples are windowed.
	if got := s.Snapshot(now).Total; got != int64(latencySamples*10) {
		t.Errorf("Total = %d, want %d", got, latencySamples*10)
	}
}

// The achieved rate should reflect recent traffic, not the lifetime average —
// otherwise a shape change takes hours to become visible.
func TestAchievedRateUsesARecentWindow(t *testing.T) {
	start := time.Date(2024, 6, 12, 10, 0, 0, 0, time.UTC)
	s := NewStats(start)

	// Ten seconds of heavy traffic, then five of light.
	for sec := 0; sec < 10; sec++ {
		at := start.Add(time.Duration(sec) * time.Second)
		for i := 0; i < 100; i++ {
			s.Record(result(200, time.Millisecond), at)
		}
	}
	for sec := 10; sec < 15; sec++ {
		at := start.Add(time.Duration(sec) * time.Second)
		for i := 0; i < 10; i++ {
			s.Record(result(200, time.Millisecond), at)
		}
	}

	// Observing at t=15s, the window covers seconds 10..14 — the light period.
	snap := s.Snapshot(start.Add(15 * time.Second))
	if snap.AchievedRPS < 8 || snap.AchievedRPS > 12 {
		t.Errorf("AchievedRPS = %v, want about 10 (the recent window, not the lifetime average)",
			snap.AchievedRPS)
	}
}

func TestSnapshotIsACopy(t *testing.T) {
	now := time.Now()
	s := NewStats(now)
	s.Record(result(200, time.Millisecond), now)

	snap := s.Snapshot(now)
	snap.ByClass["2xx"] = 9999

	if again := s.Snapshot(now); again.ByClass["2xx"] != 1 {
		t.Error("mutating a snapshot changed the live stats")
	}
}

func TestElapsedTracksTheAnchor(t *testing.T) {
	start := time.Now()
	s := NewStats(start)

	snap := s.Snapshot(start.Add(90 * time.Second))
	if snap.Elapsed != 90*time.Second {
		t.Errorf("Elapsed = %v, want 90s", snap.Elapsed)
	}
}

func context_DeadlineExceeded() error { return errDeadline }

var errDeadline = deadlineError{}

type deadlineError struct{}

func (deadlineError) Error() string { return "context deadline exceeded" }
