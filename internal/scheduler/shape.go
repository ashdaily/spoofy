// Package scheduler decides how much traffic to send and when.
//
// Load-testing tools answer "can it handle N requests per second", so a
// constant rate or a linear ramp covers them. Spoofy answers "does this
// environment look alive", and a flat line is not what alive looks like. Alert
// thresholds tuned against one fire as soon as real traffic arrives.
//
// Shapes are pure functions of time, so a 24-hour cycle is testable in
// microseconds instead of 24 hours.
package scheduler

import (
	"fmt"
	"math"
	"time"

	"github.com/ashdaily/spoofy/internal/settings"
)

// Shape maps a moment in time to a target request rate.
type Shape interface {
	// RateAt returns requests per second at now, for a run that began at start.
	RateAt(now, start time.Time) float64
	// Describe is a one-line summary shown at startup, so an operator can check
	// the config means what they thought.
	Describe() string
}

// Constant holds a steady rate. The default, and the right choice for a
// predictable baseline.
type Constant struct{ Rate float64 }

func (c Constant) RateAt(_, _ time.Time) float64 { return c.Rate }
func (c Constant) Describe() string {
	return fmt.Sprintf("constant %s", settings.Rate(c.Rate))
}

// Diurnal is a sine wave over a day: busy afternoons, quiet nights.
//
// Two properties matter. The mean over a full cycle is exactly Average, since
// sine integrates to zero, so moving from constant to diurnal redistributes
// traffic without changing the total.
//
// It is also aligned to wall-clock time of day rather than process start.
// Starting at 3pm lands at afternoon levels immediately; otherwise every
// restart puts an artefact in the graph that reads as an incident.
type Diurnal struct {
	Average   float64
	Amplitude float64       // 0.6 => peak 1.6x average, trough 0.4x
	Period    time.Duration // usually 24h
	PeakHour  float64       // hour of day the peak lands on, default 15:00
}

func (d Diurnal) RateAt(now, _ time.Time) float64 {
	period := d.Period
	if period <= 0 {
		period = 24 * time.Hour
	}

	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	sinceMidnight := now.Sub(midnight)

	peakOffset := time.Duration(d.PeakHour * float64(time.Hour))
	phase := 2 * math.Pi * float64(sinceMidnight-peakOffset) / float64(period)

	return clampRate(d.Average * (1 + d.Amplitude*math.Cos(phase)))
}

func (d Diurnal) Describe() string {
	return fmt.Sprintf("diurnal, averaging %s (peak %s around %02d:00, trough %s)",
		settings.Rate(d.Average),
		settings.Rate(d.Average*(1+d.Amplitude)),
		int(d.PeakHour),
		settings.Rate(d.Average*(1-d.Amplitude)))
}

// Ramp climbs from one rate to another over a period, then holds. Useful for
// watching an autoscaler react, or finding where latency turns.
type Ramp struct {
	From float64
	To   float64
	Over time.Duration
}

func (r Ramp) RateAt(now, start time.Time) float64 {
	if r.Over <= 0 {
		return clampRate(r.To)
	}
	elapsed := now.Sub(start)
	if elapsed <= 0 {
		return clampRate(r.From)
	}
	if elapsed >= r.Over {
		return clampRate(r.To)
	}
	progress := float64(elapsed) / float64(r.Over)
	return clampRate(r.From + (r.To-r.From)*progress)
}

func (r Ramp) Describe() string {
	return fmt.Sprintf("ramp from %s to %s over %s, then hold",
		settings.Rate(r.From), settings.Rate(r.To), r.Over)
}

// Spike holds a baseline and bursts periodically, which is what makes alert
// rules testable: set the burst above the threshold and see whether the alert
// fires.
type Spike struct {
	Base  float64
	Peak  float64
	Every time.Duration
	For   time.Duration
}

func (s Spike) RateAt(now, start time.Time) float64 {
	if s.Every <= 0 || s.For <= 0 {
		return clampRate(s.Base)
	}
	elapsed := now.Sub(start)
	if elapsed < 0 {
		elapsed = 0
	}
	if elapsed%s.Every < s.For {
		return clampRate(s.Peak)
	}
	return clampRate(s.Base)
}

func (s Spike) Describe() string {
	return fmt.Sprintf("%s baseline, spiking to %s for %s every %s",
		settings.Rate(s.Base), settings.Rate(s.Peak), s.For, s.Every)
}

// minRate keeps a shape off exactly zero. A rate limiter set to zero blocks
// forever, so a diurnal trough at amplitude 1.0 would wedge every worker until
// dawn, which looks identical to a crash.
const minRate = 0.001

func clampRate(v float64) float64 {
	if math.IsNaN(v) || v < minRate {
		return minRate
	}
	return v
}

// FromConfig builds the Shape described by a validated config.
//
// It assumes Validate has already run. A missing parameter here is a validation
// gap rather than user error, so it falls back to something safe instead of
// failing at runtime.
func FromConfig(cfg *settings.Config) Shape {
	t := cfg.Traffic
	avg := t.Rate.PerSecond()

	switch t.Shape {
	case settings.ShapeDiurnal:
		amplitude := t.Amplitude
		if amplitude <= 0 {
			amplitude = 0.6
		}
		period := t.Period.D()
		if period <= 0 {
			period = 24 * time.Hour
		}
		return Diurnal{Average: avg, Amplitude: amplitude, Period: period, PeakHour: 15}

	case settings.ShapeRamp:
		from := t.From.PerSecond()
		if from <= 0 {
			from = avg
		}
		return Ramp{From: from, To: t.To.PerSecond(), Over: t.Over.D()}

	case settings.ShapeSpike:
		return Spike{
			Base:  avg,
			Peak:  t.SpikeRate.PerSecond(),
			Every: t.SpikeEvery.D(),
			For:   t.SpikeFor.D(),
		}

	default:
		return Constant{Rate: avg}
	}
}
