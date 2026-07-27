package scheduler

import (
	"math"
	"testing"
	"time"

	"github.com/ashdaily/spoofy/internal/settings"
)

func at(hour, minute int) time.Time {
	return time.Date(2024, 6, 12, hour, minute, 0, 0, time.UTC)
}

func TestConstantIsFlat(t *testing.T) {
	c := Constant{Rate: 20}
	start := at(0, 0)

	for h := 0; h < 24; h++ {
		if got := c.RateAt(at(h, 0), start); got != 20 {
			t.Errorf("at %02d:00 rate = %v, want 20", h, got)
		}
	}
}

// The defining property of the diurnal shape: its mean over a full cycle equals
// the configured average. Without this, switching shape would silently change
// total traffic volume, and nobody could try shapes without recalculating.
func TestDiurnalMeanEqualsConfiguredAverage(t *testing.T) {
	d := Diurnal{Average: 20, Amplitude: 0.6, Period: 24 * time.Hour, PeakHour: 15}
	start := at(0, 0)

	const samples = 10000
	var sum float64
	for i := 0; i < samples; i++ {
		offset := time.Duration(float64(i) / samples * float64(24*time.Hour))
		sum += d.RateAt(start.Add(offset), start)
	}
	mean := sum / samples

	if math.Abs(mean-20) > 0.05 {
		t.Errorf("mean rate over a full day = %v, want 20", mean)
	}
}

func TestDiurnalPeaksAndTroughs(t *testing.T) {
	d := Diurnal{Average: 10, Amplitude: 0.5, Period: 24 * time.Hour, PeakHour: 15}
	start := at(0, 0)

	peak := d.RateAt(at(15, 0), start)
	trough := d.RateAt(at(3, 0), start)

	if math.Abs(peak-15) > 0.01 {
		t.Errorf("15:00 rate = %v, want 15 (average 10 * 1.5)", peak)
	}
	if math.Abs(trough-5) > 0.01 {
		t.Errorf("03:00 rate = %v, want 5 (average 10 * 0.5)", trough)
	}
	if peak <= trough {
		t.Error("afternoon should be busier than the small hours")
	}
}

// Aligned to wall clock, not to process start: restarting the daemon at 3pm
// must resume at afternoon levels rather than beginning a fresh cycle, or every
// restart puts a cliff in the graph that looks like an incident.
func TestDiurnalIsWallClockAlignedNotStartAligned(t *testing.T) {
	d := Diurnal{Average: 10, Amplitude: 0.5, Period: 24 * time.Hour, PeakHour: 15}

	noon := at(12, 0)
	fromMidnightStart := d.RateAt(noon, at(0, 0))
	fromEveningStart := d.RateAt(noon, at(20, 0))

	if math.Abs(fromMidnightStart-fromEveningStart) > 1e-9 {
		t.Errorf("rate at noon depends on start time: %v vs %v",
			fromMidnightStart, fromEveningStart)
	}
}

// Amplitude 1.0 puts the mathematical trough at exactly zero. A rate limiter
// set to zero blocks forever, so every worker would wedge until dawn — a hang
// indistinguishable from a crash.
func TestDiurnalNeverReachesZero(t *testing.T) {
	d := Diurnal{Average: 10, Amplitude: 1.0, Period: 24 * time.Hour, PeakHour: 15}
	start := at(0, 0)

	for i := 0; i < 24*60; i++ {
		got := d.RateAt(start.Add(time.Duration(i)*time.Minute), start)
		if got <= 0 {
			t.Fatalf("rate hit %v at minute %d; a zero limit blocks forever", got, i)
		}
	}
}

func TestRamp(t *testing.T) {
	r := Ramp{From: 10, To: 50, Over: time.Hour}
	start := at(0, 0)

	tests := []struct {
		elapsed time.Duration
		want    float64
	}{
		{0, 10},
		{15 * time.Minute, 20},
		{30 * time.Minute, 30},
		{time.Hour, 50},
		{2 * time.Hour, 50}, // holds after the ramp completes
	}

	for _, tc := range tests {
		got := r.RateAt(start.Add(tc.elapsed), start)
		if math.Abs(got-tc.want) > 0.01 {
			t.Errorf("after %s rate = %v, want %v", tc.elapsed, got, tc.want)
		}
	}
}

func TestRampDownwards(t *testing.T) {
	r := Ramp{From: 100, To: 10, Over: time.Hour}
	start := at(0, 0)

	if got := r.RateAt(start.Add(30*time.Minute), start); math.Abs(got-55) > 0.01 {
		t.Errorf("halfway down = %v, want 55", got)
	}
}

func TestSpike(t *testing.T) {
	s := Spike{Base: 10, Peak: 100, Every: 30 * time.Minute, For: 2 * time.Minute}
	start := at(0, 0)

	tests := []struct {
		elapsed time.Duration
		want    float64
		why     string
	}{
		{0, 100, "spike starts immediately"},
		{time.Minute, 100, "still inside the first spike"},
		{2 * time.Minute, 10, "spike has ended"},
		{29 * time.Minute, 10, "baseline before the next spike"},
		{30 * time.Minute, 100, "second spike begins"},
		{31 * time.Minute, 100, "inside the second spike"},
		{32 * time.Minute, 10, "second spike ends"},
	}

	for _, tc := range tests {
		got := s.RateAt(start.Add(tc.elapsed), start)
		if got != tc.want {
			t.Errorf("after %s rate = %v, want %v (%s)", tc.elapsed, got, tc.want, tc.why)
		}
	}
}

// Every shape must describe itself in terms an operator can check against what
// they wrote in config.
func TestDescribeMentionsTheNumbers(t *testing.T) {
	tests := []struct {
		shape Shape
		want  []string
	}{
		{Constant{Rate: 20}, []string{"constant", "20/s"}},
		{Diurnal{Average: 20, Amplitude: 0.5, Period: 24 * time.Hour, PeakHour: 15},
			[]string{"diurnal", "20/s", "30/s", "10/s"}},
		{Ramp{From: 10, To: 50, Over: time.Hour}, []string{"ramp", "10/s", "50/s"}},
		{Spike{Base: 10, Peak: 100, Every: time.Hour, For: time.Minute},
			[]string{"10/s", "100/s"}},
	}

	for _, tc := range tests {
		got := tc.shape.Describe()
		for _, want := range tc.want {
			if !contains(got, want) {
				t.Errorf("Describe() = %q, missing %q", got, want)
			}
		}
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}

func TestFromConfig(t *testing.T) {
	base := func() *settings.Config {
		c := settings.Default()
		c.Target = "http://localhost:8080"
		c.Spec = "openapi.yaml"
		c.Traffic.Rate = 20
		return &c
	}

	t.Run("constant by default", func(t *testing.T) {
		shape := FromConfig(base())
		c, ok := shape.(Constant)
		if !ok {
			t.Fatalf("got %T, want Constant", shape)
		}
		if c.Rate != 20 {
			t.Errorf("Rate = %v", c.Rate)
		}
	})

	t.Run("diurnal with defaults filled in", func(t *testing.T) {
		cfg := base()
		cfg.Traffic.Shape = settings.ShapeDiurnal
		shape := FromConfig(cfg)

		d, ok := shape.(Diurnal)
		if !ok {
			t.Fatalf("got %T, want Diurnal", shape)
		}
		if d.Average != 20 {
			t.Errorf("Average = %v", d.Average)
		}
		if d.Amplitude != 0.6 {
			t.Errorf("Amplitude = %v, want the 0.6 default", d.Amplitude)
		}
		if d.Period != 24*time.Hour {
			t.Errorf("Period = %v, want the 24h default", d.Period)
		}
	})

	t.Run("ramp", func(t *testing.T) {
		cfg := base()
		cfg.Traffic.Shape = settings.ShapeRamp
		cfg.Traffic.From = 5
		cfg.Traffic.To = 50
		cfg.Traffic.Over = settings.Duration(30 * time.Minute)

		r, ok := FromConfig(cfg).(Ramp)
		if !ok {
			t.Fatal("want Ramp")
		}
		if r.From != 5 || r.To != 50 || r.Over != 30*time.Minute {
			t.Errorf("Ramp = %+v", r)
		}
	})

	t.Run("ramp without `from` starts at the configured rate", func(t *testing.T) {
		cfg := base()
		cfg.Traffic.Shape = settings.ShapeRamp
		cfg.Traffic.To = 50
		cfg.Traffic.Over = settings.Duration(time.Minute)

		r := FromConfig(cfg).(Ramp)
		if r.From != 20 {
			t.Errorf("From = %v, want the configured rate 20", r.From)
		}
	})

	t.Run("spike", func(t *testing.T) {
		cfg := base()
		cfg.Traffic.Shape = settings.ShapeSpike
		cfg.Traffic.SpikeRate = 100
		cfg.Traffic.SpikeEvery = settings.Duration(time.Hour)
		cfg.Traffic.SpikeFor = settings.Duration(2 * time.Minute)

		s, ok := FromConfig(cfg).(Spike)
		if !ok {
			t.Fatal("want Spike")
		}
		if s.Base != 20 || s.Peak != 100 || s.Every != time.Hour || s.For != 2*time.Minute {
			t.Errorf("Spike = %+v", s)
		}
	})
}
