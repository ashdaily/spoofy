package settings

import (
	"fmt"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Rate is a request rate, stored internally as requests per second.
//
// It exists so config can say what an engineer would say out loud — "20/s",
// "1200/m", "5/min" — instead of forcing everyone to convert to a bare float.
// The unit is the part people get wrong; making it explicit in the config text
// means a misread is visible in review rather than discovered in Grafana.
type Rate float64

// unitDivisors maps a written unit to the number of seconds it spans.
var unitDivisors = map[string]float64{
	"s": 1, "sec": 1, "secs": 1, "second": 1, "seconds": 1,
	"m": 60, "min": 60, "mins": 60, "minute": 60, "minutes": 60,
	"h": 3600, "hr": 3600, "hrs": 3600, "hour": 3600, "hours": 3600,
}

// ParseRate accepts "20/s", "1200/m", "72000/h", or a bare "20" (per second).
func ParseRate(s string) (Rate, error) {
	raw := strings.TrimSpace(s)
	if raw == "" {
		return 0, fmt.Errorf("empty rate")
	}

	numPart, unitPart, hasUnit := strings.Cut(raw, "/")
	numPart = strings.TrimSpace(numPart)

	n, err := strconv.ParseFloat(numPart, 64)
	if err != nil {
		return 0, fmt.Errorf("rate %q: %q is not a number (try \"20/s\")", raw, numPart)
	}
	if n < 0 {
		return 0, fmt.Errorf("rate %q: must not be negative", raw)
	}

	if !hasUnit {
		return Rate(n), nil
	}

	unit := strings.ToLower(strings.TrimSpace(unitPart))
	divisor, ok := unitDivisors[unit]
	if !ok {
		return 0, fmt.Errorf("rate %q: unknown unit %q (use s, m, or h)", raw, unitPart)
	}
	return Rate(n / divisor), nil
}

// PerSecond returns the rate in requests per second.
func (r Rate) PerSecond() float64 { return float64(r) }

// String renders the rate in whichever unit reads most naturally, so that a
// rate parsed from "30/m" does not echo back as "0.5/s" in logs and errors.
func (r Rate) String() string {
	switch v := float64(r); {
	case v == 0:
		return "0/s"
	case v >= 1:
		return trimFloat(v) + "/s"
	case v*60 >= 1:
		return trimFloat(v*60) + "/m"
	default:
		return trimFloat(v*3600) + "/h"
	}
}

func trimFloat(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// UnmarshalYAML accepts both `rate: 20/s` and `rate: 20`, so neither a quoted
// string nor a bare number is a mistake.
//
// The branch is chosen from the YAML node tag, not from a failed decode: a bare
// 20 decodes cleanly into a Go string, which would make a decode-failure
// fallback unreachable.
func (r *Rate) UnmarshalYAML(node *yaml.Node) error {
	if node.Tag == "!!int" || node.Tag == "!!float" {
		var f float64
		if err := node.Decode(&f); err != nil {
			return fmt.Errorf("rate must be a string like \"20/s\" or a number")
		}
		if f < 0 {
			return fmt.Errorf("rate must not be negative")
		}
		*r = Rate(f)
		return nil
	}

	var s string
	if err := node.Decode(&s); err != nil {
		return fmt.Errorf("rate must be a string like \"20/s\" or a number")
	}
	parsed, err := ParseRate(s)
	if err != nil {
		return err
	}
	*r = parsed
	return nil
}

// MarshalYAML writes the human-readable form back out.
func (r Rate) MarshalYAML() (any, error) { return r.String(), nil }
