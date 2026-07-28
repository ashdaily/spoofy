package settings

import (
	"fmt"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration wraps time.Duration so config can write "24h" or "90s" directly.
// yaml.v3 has no native support, and an integer field named timeout_seconds
// pushes unit bookkeeping onto every reader of the file.
type Duration time.Duration

// D returns the underlying time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

func (d Duration) String() string { return time.Duration(d).String() }

// UnmarshalYAML accepts "30s", "24h", "1h30m", or a bare number of seconds.
//
// The branch comes from the YAML node tag rather than a failed decode, since
// yaml.v3 decodes the scalar 45 into a Go string quite happily.
func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	if node.Tag == "!!int" || node.Tag == "!!float" {
		var secs float64
		if err := node.Decode(&secs); err != nil {
			return fmt.Errorf("duration must be a string like \"30s\" or a number of seconds")
		}
		*d = Duration(time.Duration(secs * float64(time.Second)))
		return nil
	}

	var s string
	if err := node.Decode(&s); err != nil {
		return fmt.Errorf("duration must be a string like \"30s\" or a number of seconds")
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("duration %q is not valid (try \"30s\", \"5m\", \"24h\")", s)
	}
	*d = Duration(parsed)
	return nil
}

// MarshalYAML writes the human-readable form back out.
func (d Duration) MarshalYAML() (any, error) { return d.String(), nil }
