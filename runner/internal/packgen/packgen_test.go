package packgen

import (
	"strings"
	"testing"
)

// TestDefaultRegistry checks that the embedded generations.json parses and
// lists packs-v6, packs-v5 and packs-v4, newest first.
func TestDefaultRegistry(t *testing.T) {
	r, err := Parse(registryJSON)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, g := range r {
		names = append(names, g.Name)
	}
	if got := strings.Join(names, ","); got != "packs-v6,packs-v5,packs-v4" {
		t.Errorf("names = %s", got)
	}
	if _, ok := Default().Lookup("packs-v5"); !ok {
		t.Error("Lookup(packs-v5) found nothing")
	}
	if _, ok := Default().Lookup("packs-v3"); ok {
		t.Error("Lookup(packs-v3) found an entry")
	}
}

// TestParseRejects checks that Parse refuses each kind of bad registry.
func TestParseRejects(t *testing.T) {
	entry := func(name, pattern, paths, commit string) string {
		return `{"name":"` + name + `","reader_marker":{"pattern":"` + pattern + `","paths":` + paths +
			`},"writer_commit":"` + commit + `","format_change":"x"}`
	}
	ok := entry("packs-v2", "a", `["cmd"]`, "abcdef12")
	for name, body := range map[string]string{
		"not JSON":        `{`,
		"no generations":  `{"generations":[]}`,
		"unknown field":   `{"generations":[` + ok + `],"extra":1}`,
		"bad name":        `{"generations":[` + entry("v2", "a", `["cmd"]`, "abcdef12") + `]}`,
		"oldest first":    `{"generations":[` + ok + `,` + entry("packs-v3", "a", `["cmd"]`, "abcdef12") + `]}`,
		"duplicate name":  `{"generations":[` + ok + `,` + ok + `]}`,
		"empty pattern":   `{"generations":[` + entry("packs-v2", "", `["cmd"]`, "abcdef12") + `]}`,
		"bad pattern":     `{"generations":[` + entry("packs-v2", "(", `["cmd"]`, "abcdef12") + `]}`,
		"no paths":        `{"generations":[` + entry("packs-v2", "a", `[]`, "abcdef12") + `]}`,
		"bad commit":      `{"generations":[` + entry("packs-v2", "a", `["cmd"]`, "main") + `]}`,
		"bad absent mark": `{"generations":[{"name":"packs-v2","reader_marker":{"pattern":"a","paths":["cmd"]},"reader_absent":{"pattern":"b","paths":[]},"writer_commit":"abcdef12"}]}`,
	} {
		if _, err := Parse([]byte(body)); err == nil {
			t.Errorf("%s: Parse succeeded, want an error", name)
		}
	}
	if _, err := Parse([]byte(`{"generations":[` + ok + `]}`)); err != nil {
		t.Errorf("valid registry: %v", err)
	}
}
