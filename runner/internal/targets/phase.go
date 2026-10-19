package targets

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
)

// PhaseLabel names one entry of the phases table: "1", "2", "3", "3b". It is a
// label, never a number to compute with; the table's order is what ties a phase
// to its floors. Phases 1-3 are JSON and TOML numbers, later ones strings, and
// a label is read from and written back to either form, so manifests written
// before 3b keep their shape.
type PhaseLabel string

var rePhaseLabel = regexp.MustCompile(`^[1-9][0-9]*[a-z]*$`)

// ParsePhaseLabel validates s as a label. "" and "0" mean unset.
func ParsePhaseLabel(s string) (PhaseLabel, error) {
	if s == "" || s == "0" {
		return "", nil
	}
	if !rePhaseLabel.MatchString(s) {
		return "", fmt.Errorf("phase label %q: want a phase number with an optional letter suffix, such as 3 or 3b", s)
	}
	return PhaseLabel(s), nil
}

// UnmarshalJSON reads a number or a string.
func (l *PhaseLabel) UnmarshalJSON(b []byte) error {
	var s string
	if bytes.HasPrefix(b, []byte(`"`)) {
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
	} else {
		var n json.Number
		if err := json.Unmarshal(b, &n); err != nil {
			return fmt.Errorf("phase: %w", err)
		}
		s = n.String()
	}
	v, err := ParsePhaseLabel(s)
	*l = v
	return err
}

// MarshalJSON writes an all-digit label as a number and any other as a string.
func (l PhaseLabel) MarshalJSON() ([]byte, error) {
	if n, err := strconv.Atoi(string(l)); err == nil {
		return []byte(strconv.Itoa(n)), nil
	}
	return json.Marshal(string(l))
}

// UnmarshalTOML reads `phase = 3` or `phase = "3b"` (BurntSushi/toml).
func (l *PhaseLabel) UnmarshalTOML(v any) error {
	var s string
	switch x := v.(type) {
	case int64:
		s = strconv.FormatInt(x, 10)
	case string:
		s = x
	default:
		return fmt.Errorf("phase: want a number or a string, got %T", v)
	}
	p, err := ParsePhaseLabel(s)
	*l = p
	return err
}
