package targets

import (
	"encoding/json"
	"testing"

	"github.com/BurntSushi/toml"
)

// TestPhaseLabelForms checks that a label reads from a number or a string and
// writes phases 1-3 back as numbers, so manifests keep their pre-3b shape.
func TestPhaseLabelForms(t *testing.T) {
	for _, tc := range []struct {
		json, wantJSON string
		want           PhaseLabel
	}{
		{`3`, `3`, "3"},
		{`"3"`, `3`, "3"},
		{`"3b"`, `"3b"`, "3b"},
		{`0`, `""`, ""},
	} {
		var l PhaseLabel
		if err := json.Unmarshal([]byte(tc.json), &l); err != nil {
			t.Fatalf("unmarshal %s: %v", tc.json, err)
		}
		if l != tc.want {
			t.Errorf("unmarshal %s = %q, want %q", tc.json, l, tc.want)
		}
		if b, _ := json.Marshal(l); string(b) != tc.wantJSON {
			t.Errorf("marshal %q = %s, want %s", l, b, tc.wantJSON)
		}
	}
	for _, bad := range []string{`-1`, `"x"`, `"3B"`, `true`} {
		var l PhaseLabel
		if err := json.Unmarshal([]byte(bad), &l); err == nil {
			t.Errorf("unmarshal %s = %q, want an error", bad, l)
		}
	}

	var cfg struct {
		A PhaseLabel `toml:"a"`
		B PhaseLabel `toml:"b"`
	}
	if _, err := toml.Decode("a = 2\nb = \"3b\"\n", &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.A != "2" || cfg.B != "3b" {
		t.Errorf("toml = %q, %q; want 2, 3b", cfg.A, cfg.B)
	}
}
