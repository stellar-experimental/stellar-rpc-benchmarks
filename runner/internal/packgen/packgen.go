// Package packgen is the registry of dataset pack generations: which packs-vN
// tree a stellar-rpc commit reads, told apart by source markers in the commit.
// The registry is generations.json; a new generation is an entry there.
package packgen

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

//go:embed generations.json
var registryJSON []byte

// Marker is a source probe: an extended regexp (git grep -E) over the
// non-test files of a commit under Paths.
type Marker struct {
	Pattern string   `json:"pattern"`
	Paths   []string `json:"paths"`
}

// String is the marker as plan.json and the notes name it.
func (m Marker) String() string {
	return fmt.Sprintf("/%s/ in %s", m.Pattern, strings.Join(m.Paths, ","))
}

// Generation is one registry entry. A commit reads it when ReaderMarker
// matches and ReaderAbsent, if set, does not.
type Generation struct {
	Name         string  `json:"name"`
	ReaderMarker Marker  `json:"reader_marker"`
	ReaderAbsent *Marker `json:"reader_absent,omitempty"`
	WriterCommit string  `json:"writer_commit"`
	FormatChange string  `json:"format_change"`
}

// Describe names the markers that select g.
func (g Generation) Describe() string {
	s := g.ReaderMarker.String()
	if g.ReaderAbsent != nil {
		s += " and not " + g.ReaderAbsent.String()
	}
	return s
}

// Registry is the list of generations, newest first.
type Registry []Generation

var (
	nameRe   = regexp.MustCompile(`^packs-v([0-9]+)$`)
	commitRe = regexp.MustCompile(`^[0-9a-f]{7,40}$`)
)

// Parse decodes and validates a registry: at least one entry, names packs-vN
// in strictly decreasing N, a compilable non-empty marker with paths, and a
// hex writer commit.
func Parse(b []byte) (Registry, error) {
	var doc struct {
		Generations Registry `json:"generations"`
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("pack generation registry: %w", err)
	}
	if len(doc.Generations) == 0 {
		return nil, errors.New("pack generation registry: no generations")
	}
	prev := -1
	for i, g := range doc.Generations {
		m := nameRe.FindStringSubmatch(g.Name)
		if m == nil {
			return nil, fmt.Errorf("pack generation registry: entry %d: name %q is not packs-vN", i, g.Name)
		}
		n, _ := strconv.Atoi(m[1])
		if prev >= 0 && n >= prev {
			return nil, fmt.Errorf("pack generation registry: %s follows packs-v%d; list the newest first", g.Name, prev)
		}
		prev = n
		if err := g.ReaderMarker.validate(); err != nil {
			return nil, fmt.Errorf("pack generation registry: %s reader_marker: %w", g.Name, err)
		}
		if g.ReaderAbsent != nil {
			if err := g.ReaderAbsent.validate(); err != nil {
				return nil, fmt.Errorf("pack generation registry: %s reader_absent: %w", g.Name, err)
			}
		}
		if !commitRe.MatchString(g.WriterCommit) {
			return nil, fmt.Errorf("pack generation registry: %s writer_commit %q is not a hex commit", g.Name, g.WriterCommit)
		}
	}
	return doc.Generations, nil
}

func (m Marker) validate() error {
	if m.Pattern == "" {
		return errors.New("empty pattern")
	}
	if _, err := regexp.Compile(m.Pattern); err != nil {
		return err
	}
	if len(m.Paths) == 0 {
		return errors.New("no paths")
	}
	for _, p := range m.Paths {
		if p == "" {
			return errors.New("empty path")
		}
	}
	return nil
}

// Lookup returns the generation called name.
func (r Registry) Lookup(name string) (Generation, bool) {
	for _, g := range r {
		if g.Name == name {
			return g, true
		}
	}
	return Generation{}, false
}

var loadDefault = sync.OnceValues(func() (Registry, error) { return Parse(registryJSON) })

// Default is the embedded generations.json. A registry that fails Parse
// panics: the tests parse it, so a bad edit never reaches a campaign.
func Default() Registry {
	r, err := loadDefault()
	if err != nil {
		panic(err)
	}
	return r
}
