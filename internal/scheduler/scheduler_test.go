package scheduler

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestRunStopsPromptlyOnCancel(t *testing.T) {
	s := New(Constant{Rate: 500}, 4)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		s.Run(ctx, func(context.Context) {})
		close(done)
	}()

	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return within 3s of cancellation")
	}

	if s.Completed() == 0 {
		t.Error("no work completed before cancellation")
	}
}

// Run must not return until every worker has finished. A caller cancelling on
// SIGTERM relies on this for a clean drain rather than severed connections.
func TestRunWaitsForInFlightWork(t *testing.T) {
	var (
		started  atomic.Int64
		finished atomic.Int64
	)

	s := New(Constant{Rate: 200}, 4)
	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()

	s.Run(ctx, func(context.Context) {
		started.Add(1)
		time.Sleep(20 * time.Millisecond)
		finished.Add(1)
	})

	// Run has returned, so every started unit must also have finished.
	if got, want := finished.Load(), started.Load(); got != want {
		t.Errorf("Run returned with work in flight: %d started, %d finished", want, got)
	}
	if started.Load() == 0 {
		t.Error("no work ran at all")
	}
}

// Pacing is checked with a wide band. The precise number depends on scheduler
// timing, but an order-of-magnitude error — no limiting at all, or a limiter
// that never releases — must fail.
func TestRunPacesRoughlyToTheTargetRate(t *testing.T) {
	if testing.Short() {
		t.Skip("timing-sensitive")
	}

	const (
		targetRate = 200.0
		window     = 500 * time.Millisecond
	)
	expected := targetRate * window.Seconds() // 100

	s := New(Constant{Rate: targetRate}, 8)
	ctx, cancel := context.WithTimeout(context.Background(), window)
	defer cancel()

	s.Run(ctx, func(context.Context) {})

	got := float64(s.Completed())
	if got < expected*0.4 || got > expected*1.8 {
		t.Errorf("completed %v units in %s at %v/s, want roughly %v",
			got, window, targetRate, expected)
	}
}

// The rate must track the shape as time passes. Driven by an injected clock so
// a 24-hour cycle is exercised in milliseconds.
func TestRunFollowsTheShapeOverTime(t *testing.T) {
	start := at(0, 0)
	var offset atomic.Int64 // nanoseconds since start

	clock := func() time.Time {
		return start.Add(time.Duration(offset.Load()))
	}

	shape := Diurnal{Average: 100, Amplitude: 0.5, Period: 24 * time.Hour, PeakHour: 15}
	s := New(shape, 2).WithClock(clock, 5*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.Run(ctx, func(context.Context) {})
		close(done)
	}()

	// Advance the fake clock to 03:00, the trough.
	offset.Store(int64(3 * time.Hour))
	troughRate := waitForRateChange(t, s, 0)

	// Now to 15:00, the peak.
	offset.Store(int64(15 * time.Hour))
	peakRate := waitForRateChange(t, s, troughRate)

	cancel()
	<-done

	if peakRate <= troughRate {
		t.Errorf("peak rate %v is not above trough rate %v", peakRate, troughRate)
	}
	if want := 150.0; peakRate < want*0.9 || peakRate > want*1.1 {
		t.Errorf("peak rate = %v, want about %v", peakRate, want)
	}
	if want := 50.0; troughRate < want*0.9 || troughRate > want*1.1 {
		t.Errorf("trough rate = %v, want about %v", troughRate, want)
	}
}

// waitForRateChange polls until CurrentRate differs from previous, so the test
// synchronises on the adjust loop rather than on a sleep.
func waitForRateChange(t *testing.T, s *Scheduler, previous float64) float64 {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatalf("rate never moved away from %v", previous)
		default:
		}
		if got := s.CurrentRate(); got != previous {
			return got
		}
		time.Sleep(time.Millisecond)
	}
}

func TestNewClampsConcurrency(t *testing.T) {
	for _, in := range []int{0, -5} {
		if got := New(Constant{Rate: 1}, in).concurrency; got != 1 {
			t.Errorf("New(_, %d) concurrency = %d, want 1", in, got)
		}
	}
}

func TestCurrentRateIsPublishedBeforeAnyTick(t *testing.T) {
	s := New(Constant{Rate: 42}, 1)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.Run(ctx, func(context.Context) {})
		close(done)
	}()

	deadline := time.After(time.Second)
	for s.CurrentRate() == 0 {
		select {
		case <-deadline:
			t.Fatal("CurrentRate stayed at 0; the live view would show nothing at startup")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	if got := s.CurrentRate(); got != 42 {
		t.Errorf("CurrentRate = %v, want 42", got)
	}

	cancel()
	<-done
}
