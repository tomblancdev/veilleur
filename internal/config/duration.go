package config

import (
	"encoding/json"
	"fmt"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration is a time.Duration that reads as "20m" in YAML and writes as
// "20m0s" in JSON. The graph is meant to be edited by a person and read by
// a person; nanosecond integers are neither.
type Duration time.Duration

// D is the underlying duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

// UnmarshalYAML accepts "90s", "3m", "2h" — and a bare number as seconds.
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err == nil {
		p, err := time.ParseDuration(s)
		if err != nil {
			return fmt.Errorf("duration %q: %w", s, err)
		}
		*d = Duration(p)
		return nil
	}
	var n int64
	if err := value.Decode(&n); err == nil {
		*d = Duration(time.Duration(n) * time.Second)
		return nil
	}
	return fmt.Errorf("duration: cannot read %q", value.Value)
}

// MarshalYAML writes it back the way a person typed it.
func (d Duration) MarshalYAML() (any, error) { return time.Duration(d).String(), nil }

// MarshalJSON keeps the API readable.
func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(time.Duration(d).String()) }

// UnmarshalJSON accepts the same string form.
func (d *Duration) UnmarshalJSON(raw []byte) error {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return err
	}
	p, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(p)
	return nil
}
