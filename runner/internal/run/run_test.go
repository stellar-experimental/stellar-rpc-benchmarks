package run

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/stellar/stellar-rpc-benchmarks/runner/internal/plan"
)

// --- helpers --------------------------------------------------------------

func mustMkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	mustMkdir(t, filepath.Dir(path))
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// shLeg is a timed leg whose measurement is a shell script. The script's
// positional parameters carry the paths it needs — passing them as arguments
// rather than environment keeps the leg's Env free for the env-propagation
// test.
func shLeg(id, outDir, script string, args ...string) plan.Step {
	argv := append([]string{"/bin/sh", "-c", script, "sh", outDir}, args...)
	return plan.Step{ID: id, Kind: plan.KindLeg, Timed: true, OutDir: outDir, Argv: [][]string{argv}}
}

// outcome is everything one Execute call produced.
type outcome struct {
	results []StepResult
	err     error
	log     string
}

func walk(t *testing.T, p *plan.Plan, opts Options) outcome {
	t.Helper()
	var buf bytes.Buffer
	opts.Output = &buf
	results, err := Execute(p, opts)
	return outcome{results: results, err: err, log: buf.String()}
}

func (o outcome) statuses() []Status {
	got := make([]Status, len(o.results))
	for i, r := range o.results {
		got[i] = r.Status
	}
	return got
}

func (o outcome) assertStatuses(t *testing.T, want ...Status) {
	t.Helper()
	if got := o.statuses(); !slices.Equal(got, want) {
		t.Errorf("statuses = %v, want %v\nlog:\n%s", got, want, o.log)
	}
}

func (o outcome) assertLogHas(t *testing.T, want string) {
	t.Helper()
	if !strings.Contains(o.log, want) {
		t.Errorf("log does not contain %q\nlog:\n%s", want, o.log)
	}
}

func (o outcome) assertLogLacks(t *testing.T, unwanted string) {
	t.Helper()
	if strings.Contains(o.log, unwanted) {
		t.Errorf("log contains %q, want it not to\nlog:\n%s", unwanted, o.log)
	}
}

func readSentinel(t *testing.T, outDir string) legSentinel {
	t.Helper()
	path := filepath.Join(outDir, legSentinelName)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var s legSentinel
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatalf("unmarshal %s: %v", path, err)
	}
	return s
}

func assertGone(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("%s still exists (stat err = %v)", path, err)
	}
}

func assertExists(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Errorf("%s missing: %v", path, err)
	}
}

// --- resume decision table -------------------------------------------------

func TestClassifyLegDir(t *testing.T) {
	// The leg being classified: its plan id is its --out directory's basename,
	// and the sentinel has to name it to be believed.
	const legID = "ingest-cold-ds-c0-run1"
	cases := []struct {
		name       string
		setup      func(t *testing.T, dir string)
		want       legStateKind
		wantReason string
	}{
		{
			name:  "no directory at all",
			setup: func(t *testing.T, dir string) { os.RemoveAll(dir) },
			want:  legAbsent,
		},
		{
			name:  "empty directory",
			setup: func(t *testing.T, dir string) {},
			want:  legPartial,
		},
		{
			name: "dangling symlink where the out-dir should be",
			setup: func(t *testing.T, dir string) {
				if err := os.RemoveAll(dir); err != nil {
					t.Fatalf("RemoveAll: %v", err)
				}
				if err := os.Symlink(dir+"-gone", dir); err != nil {
					t.Fatalf("Symlink: %v", err)
				}
			},
			want: legPartial,
		},
		{
			name: "sentinel says success",
			setup: func(t *testing.T, dir string) {
				mustWrite(t, filepath.Join(dir, legSentinelName), `{"schema_version":1,"id":"`+legID+`","exit_code":0}`)
			},
			want: legComplete,
		},
		{
			name: "sentinel says failure",
			setup: func(t *testing.T, dir string) {
				mustWrite(t, filepath.Join(dir, legSentinelName),
					`{"schema_version":1,"id":"`+legID+`","exit_code":1,"error":"exit status 1"}`)
			},
			want:       legFailedEarlier,
			wantReason: "exit status 1",
		},
		{
			name: "sentinel with a nonzero exit and no error field",
			setup: func(t *testing.T, dir string) {
				mustWrite(t, filepath.Join(dir, legSentinelName), `{"schema_version":1,"id":"`+legID+`","exit_code":2}`)
			},
			want:       legFailedEarlier,
			wantReason: "exit status 2",
		},
		{
			// The degenerate sentinel: valid JSON, zero exit code by omission,
			// and no claim to be anything. Believing it would skip a leg that
			// never ran.
			name: "empty JSON object as a sentinel",
			setup: func(t *testing.T, dir string) {
				mustWrite(t, filepath.Join(dir, legSentinelName), `{}`)
			},
			want:       legPartial,
			wantReason: "sentinel does not match this leg",
		},
		{
			// The right leg, the right schema, and no record of how it ended:
			// an absent exit_code must not decode into the 0 that means success.
			name: "sentinel without an exit_code",
			setup: func(t *testing.T, dir string) {
				mustWrite(t, filepath.Join(dir, legSentinelName), `{"schema_version":1,"id":"`+legID+`"}`)
			},
			want:       legPartial,
			wantReason: "sentinel records no exit",
		},
		{
			name: "sentinel records a different leg",
			setup: func(t *testing.T, dir string) {
				mustWrite(t, filepath.Join(dir, legSentinelName),
					`{"schema_version":1,"id":"ingest-cold-ds-c0-run2","exit_code":0}`)
			},
			want:       legPartial,
			wantReason: "sentinel does not match this leg",
		},
		{
			name: "sentinel from an unknown schema version",
			setup: func(t *testing.T, dir string) {
				mustWrite(t, filepath.Join(dir, legSentinelName), `{"schema_version":0,"id":"`+legID+`","exit_code":0}`)
			},
			want:       legPartial,
			wantReason: "sentinel does not match this leg",
		},
		{
			name: "corrupt sentinel",
			setup: func(t *testing.T, dir string) {
				mustWrite(t, filepath.Join(dir, legSentinelName), `{"schema_version":1,`)
			},
			want: legPartial,
		},
		{
			name: "bash-era bundle: invocation.json and driver.csv, no error",
			setup: func(t *testing.T, dir string) {
				mustWrite(t, filepath.Join(dir, "invocation.json"),
					`{"schemaVersion":1,"command":"bench-ingest cold","startedAt":"2026-07-01T00:00:00Z","finishedAt":"2026-07-01T00:10:00Z"}`)
				mustWrite(t, filepath.Join(dir, "driver.csv"), "stage,wall\n")
			},
			want: legComplete,
		},
		{
			name: "bash-era bundle: invocation.json records an error",
			setup: func(t *testing.T, dir string) {
				mustWrite(t, filepath.Join(dir, "invocation.json"), `{"schemaVersion":1,"error":"datastore unreachable"}`)
				mustWrite(t, filepath.Join(dir, "driver.csv"), "stage,wall\n")
			},
			want:       legFailedEarlier,
			wantReason: "datastore unreachable",
		},
		{
			name: "invocation.json without driver.csv",
			setup: func(t *testing.T, dir string) {
				mustWrite(t, filepath.Join(dir, "invocation.json"), `{"schemaVersion":1}`)
			},
			want: legPartial,
		},
		{
			name: "driver.csv without invocation.json",
			setup: func(t *testing.T, dir string) {
				mustWrite(t, filepath.Join(dir, "driver.csv"), "stage,wall\n")
			},
			want: legPartial,
		},
		{
			name: "unreadable invocation.json",
			setup: func(t *testing.T, dir string) {
				mustWrite(t, filepath.Join(dir, "invocation.json"), `{"schemaVersion":`)
				mustWrite(t, filepath.Join(dir, "driver.csv"), "stage,wall\n")
			},
			want: legPartial,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), legID)
			mustMkdir(t, dir)
			tc.setup(t, dir)
			got := classifyLegDir(dir, legID)
			if got.kind != tc.want {
				t.Errorf("kind = %v, want %v", got.kind, tc.want)
			}
			if got.reason != tc.wantReason {
				t.Errorf("reason = %q, want %q", got.reason, tc.wantReason)
			}
		})
	}
}

// --- executor mechanics -----------------------------------------------------

func TestExecuteHappyPath(t *testing.T) {
	tmp := t.TempDir()
	scratch := filepath.Join(tmp, "scratch")
	post := filepath.Join(tmp, "post")
	mustWrite(t, filepath.Join(scratch, "stale"), "left over from the previous rep")
	mustWrite(t, filepath.Join(post, "hot.db"), "x")
	outA := filepath.Join(tmp, "res", "ingest-cold-ds-c0-run1")
	outB := filepath.Join(tmp, "res", "ingest-cold-ds-c0-run2")

	a := shLeg("a", outA, `test ! -e "$2/stale" && : > "$1/driver.csv"`, scratch)
	a.PreClean = []string{scratch}
	a.PostClean = []string{post}
	b := shLeg("b", outB, `: > "$1/driver.csv"`)
	b.Needs = []string{"a"}
	p := &plan.Plan{Steps: []plan.Step{a, b}}

	got := walk(t, p, Options{})
	if got.err != nil {
		t.Fatalf("Execute: %v\nlog:\n%s", got.err, got.log)
	}
	got.assertStatuses(t, StatusOK, StatusOK)

	for _, out := range []string{outA, outB} {
		assertExists(t, filepath.Join(out, "driver.csv"))
		s := readSentinel(t, out)
		if s.SchemaVersion != LegSchemaVersion || s.ExitCode != 0 || s.Error != "" {
			t.Errorf("%s sentinel = %+v, want schema %d, exit 0, no error", out, s, LegSchemaVersion)
		}
		if s.DurationNS <= 0 {
			t.Errorf("%s duration_ns = %d, want > 0", out, s.DurationNS)
		}
		if s.StartedAt == "" || s.FinishedAt == "" {
			t.Errorf("%s sentinel is missing timestamps: %+v", out, s)
		}
	}
	if s := readSentinel(t, outA); !slices.Equal(s.Argv, p.Steps[0].Argv[0]) {
		t.Errorf("sentinel argv = %v, want %v", s.Argv, p.Steps[0].Argv[0])
	}
	// PostClean runs only on success, and it ran: the hot DB is gone.
	assertGone(t, post)
	got.assertLogHas(t, "  $ rm -rf "+scratch)
}

func TestExecuteKeepGoing(t *testing.T) {
	tmp := t.TempDir()
	out := func(id string) string { return filepath.Join(tmp, "res", id) }

	fail := shLeg("fail", out("fail"), `exit 1`)
	dep := shLeg("dep", out("dep"), `: > "$1/driver.csv"`)
	dep.Needs = []string{"fail"}
	dep2 := shLeg("dep2", out("dep2"), `: > "$1/driver.csv"`)
	dep2.Needs = []string{"dep"}
	indep := shLeg("indep", out("indep"), `: > "$1/driver.csv"`)
	p := &plan.Plan{Steps: []plan.Step{fail, dep, dep2, indep}}

	got := walk(t, p, Options{})
	if got.err == nil {
		t.Fatalf("Execute returned nil error after a failed leg\nlog:\n%s", got.log)
	}
	if want := "1 step(s) failed"; got.err.Error() != want {
		t.Errorf("Execute error = %q, want %q", got.err, want)
	}
	got.assertStatuses(t, StatusFailed, StatusSkipped, StatusSkipped, StatusOK)

	got.assertLogHas(t, "== campaign summary: 1 failed, 2 skipped")
	got.assertLogHas(t, "==   failed:  fail (exit status 1)")
	got.assertLogHas(t, "==   skipped: dep (needs fail)")
	// dep2 needs dep, which was skipped rather than failed: the propagation is
	// transitive without the executor walking the graph.
	got.assertLogHas(t, "==   skipped: dep2 (needs dep)")

	s := readSentinel(t, out("fail"))
	if s.ExitCode != 1 || s.Error != "exit status 1" {
		t.Errorf("failed leg sentinel = %+v, want exit_code 1 and error \"exit status 1\"", s)
	}
	assertExists(t, filepath.Join(out("indep"), legSentinelName))
	assertGone(t, out("dep"))
}

func TestExecuteFailFast(t *testing.T) {
	tmp := t.TempDir()
	out := func(id string) string { return filepath.Join(tmp, "res", id) }
	fail := shLeg("fail", out("fail"), `exit 3`)
	indep := shLeg("indep", out("indep"), `: > "$1/driver.csv"`)
	p := &plan.Plan{Steps: []plan.Step{fail, indep}}

	got := walk(t, p, Options{FailFast: true})
	if got.err == nil {
		t.Fatalf("Execute returned nil error under fail-fast\nlog:\n%s", got.log)
	}
	got.assertStatuses(t, StatusFailed)
	// The walk stopped: the later step has no result and never ran.
	assertGone(t, out("indep"))
	if s := readSentinel(t, out("fail")); s.ExitCode != 3 {
		t.Errorf("sentinel exit_code = %d, want 3", s.ExitCode)
	}
}

func TestExecuteResume(t *testing.T) {
	tmp := t.TempDir()
	ran := filepath.Join(tmp, "ran.txt")
	outDone := filepath.Join(tmp, "res", "ingest-cold-ds-c0-run1")
	outPartial := filepath.Join(tmp, "res", "ingest-cold-ds-c0-run2")

	// An earlier session finished run1 and was killed during run2, which left a
	// half-written directory with no manifests in it.
	mustWrite(t, filepath.Join(outDone, legSentinelName), `{"schema_version":1,"id":"done","exit_code":0}`)
	mustWrite(t, filepath.Join(outDone, "driver.csv"), "stage,wall\n")
	mustWrite(t, filepath.Join(outPartial, "driver.csv.tmp"), "half a row")

	script := `echo "$2" >> "$3"; : > "$1/driver.csv"`
	p := &plan.Plan{Steps: []plan.Step{
		shLeg("done", outDone, script, "done", ran),
		shLeg("partial", outPartial, script, "partial", ran),
	}}

	got := walk(t, p, Options{Resume: true})
	if got.err != nil {
		t.Fatalf("Execute: %v\nlog:\n%s", got.err, got.log)
	}
	got.assertStatuses(t, StatusResumed, StatusOK)
	got.assertLogHas(t, "resume: ingest-cold-ds-c0-run1 already complete — skipping")
	got.assertLogHas(t, "resume: ingest-cold-ds-c0-run2 is a partial leg — wiping and re-running")

	b, err := os.ReadFile(ran)
	if err != nil {
		t.Fatalf("read %s: %v", ran, err)
	}
	if got := strings.Fields(string(b)); !slices.Equal(got, []string{"partial"}) {
		t.Errorf("legs that ran = %v, want only [partial]", got)
	}
	// The partial directory was wiped, not merged into.
	assertGone(t, filepath.Join(outPartial, "driver.csv.tmp"))
	assertExists(t, filepath.Join(outPartial, legSentinelName))
}

func TestExecuteResumeAfterRecordedFailure(t *testing.T) {
	tmp := t.TempDir()
	out := filepath.Join(tmp, "res", "query-cold-ds-c0-run1")
	mustWrite(t, filepath.Join(out, legSentinelName), `{"schema_version":1,"id":"leg","exit_code":1,"error":"exit status 1"}`)

	p := &plan.Plan{Steps: []plan.Step{shLeg("leg", out, `: > "$1/driver.csv"`)}}
	got := walk(t, p, Options{Resume: true})
	if got.err != nil {
		t.Fatalf("Execute: %v\nlog:\n%s", got.err, got.log)
	}
	got.assertStatuses(t, StatusOK)
	got.assertLogHas(t, "resume: query-cold-ds-c0-run1 failed in an earlier session (exit status 1) — wiping and re-running")
	if s := readSentinel(t, out); s.ExitCode != 0 || s.Error != "" {
		t.Errorf("sentinel after re-run = %+v, want a clean success", s)
	}
}

// A sentinel that does not name this leg proves nothing about the directory it
// sits in — a copied or hand-made leg.json must not skip a leg that never ran.
func TestExecuteResumeRejectsForeignSentinel(t *testing.T) {
	tmp := t.TempDir()
	out := filepath.Join(tmp, "res", "ingest-cold-ds-c0-run1")
	mustWrite(t, filepath.Join(out, legSentinelName), `{"schema_version":1,"id":"ingest-cold-ds-c0-run2","exit_code":0}`)
	ran := filepath.Join(tmp, "ran.txt")

	p := &plan.Plan{Steps: []plan.Step{shLeg("ingest-cold-ds-c0-run1", out, `echo ran >> "$2"; : > "$1/driver.csv"`, ran)}}
	got := walk(t, p, Options{Resume: true})
	if got.err != nil {
		t.Fatalf("Execute: %v\nlog:\n%s", got.err, got.log)
	}
	got.assertStatuses(t, StatusOK)
	got.assertLogHas(t, "resume: ingest-cold-ds-c0-run1 is a partial leg (sentinel does not match this leg) — wiping and re-running")
	assertExists(t, ran)
	if s := readSentinel(t, out); s.ID != "ingest-cold-ds-c0-run1" {
		t.Errorf("sentinel after re-run = %+v, want this leg's id", s)
	}
}

func TestExecuteWithoutResumeIgnoresExistingOutput(t *testing.T) {
	tmp := t.TempDir()
	out := filepath.Join(tmp, "res", "ingest-cold-ds-c0-run1")
	mustWrite(t, filepath.Join(out, legSentinelName), `{"schema_version":1,"exit_code":0}`)
	ran := filepath.Join(tmp, "ran.txt")

	p := &plan.Plan{Steps: []plan.Step{shLeg("leg", out, `echo ran >> "$2"`, ran)}}
	got := walk(t, p, Options{})
	got.assertStatuses(t, StatusOK)
	assertExists(t, ran)
	if strings.Contains(got.log, "resume:") {
		t.Errorf("a non-resume walk inspected existing output\nlog:\n%s", got.log)
	}
}

func TestExecuteLegEnv(t *testing.T) {
	tmp := t.TempDir()
	out := filepath.Join(tmp, "res", "golden-ds-c0")
	step := shLeg("env", out, `test "$FOO" = bar`)
	step.Env = map[string]string{"FOO": "bar"}

	got := walk(t, &plan.Plan{Steps: []plan.Step{step}}, Options{})
	if got.err != nil {
		t.Fatalf("Execute: %v\nlog:\n%s", got.err, got.log)
	}
	got.assertStatuses(t, StatusOK)
	got.assertLogHas(t, "  $ env FOO=bar /bin/sh -c")
}

func TestExecuteBuild(t *testing.T) {
	t.Run("existing binary skips the build", func(t *testing.T) {
		tmp := t.TempDir()
		bin := filepath.Join(tmp, "bin", "stellar-rpc-deadbeef")
		mustWrite(t, bin, "#!/bin/sh\n")
		if err := os.Chmod(bin, 0o755); err != nil {
			t.Fatalf("chmod: %v", err)
		}
		p := &plan.Plan{Bin: bin, Steps: []plan.Step{{
			ID: "build", Kind: plan.KindBuild, Argv: [][]string{{"/bin/sh", "-c", "exit 1"}},
		}}}
		got := walk(t, p, Options{})
		got.assertStatuses(t, StatusOK)
		got.assertLogHas(t, "binary "+bin+" already built — skipping build")
		if strings.Contains(got.log, "  $ ") {
			t.Errorf("build ran a command despite the binary being there\nlog:\n%s", got.log)
		}
	})

	t.Run("missing binary runs every command in order", func(t *testing.T) {
		tmp := t.TempDir()
		order := filepath.Join(tmp, "order.txt")
		p := &plan.Plan{Bin: filepath.Join(tmp, "bin", "stellar-rpc-deadbeef"), Steps: []plan.Step{{
			ID: "build", Kind: plan.KindBuild, Argv: [][]string{
				{"/bin/sh", "-c", `echo checkout >> "$1"`, "sh", order},
				{"/bin/sh", "-c", `echo make >> "$1"`, "sh", order},
			},
		}}}
		got := walk(t, p, Options{})
		got.assertStatuses(t, StatusOK)
		b, err := os.ReadFile(order)
		if err != nil {
			t.Fatalf("read %s: %v", order, err)
		}
		if want := []string{"checkout", "make"}; !slices.Equal(strings.Fields(string(b)), want) {
			t.Errorf("commands ran as %q, want %v", b, want)
		}
	})

	t.Run("a failed command stops the rest of the step", func(t *testing.T) {
		tmp := t.TempDir()
		marker := filepath.Join(tmp, "second.txt")
		p := &plan.Plan{Bin: filepath.Join(tmp, "bin", "stellar-rpc-deadbeef"), Steps: []plan.Step{{
			ID: "build", Kind: plan.KindBuild, Argv: [][]string{
				{"/bin/sh", "-c", "exit 1"},
				{"/bin/sh", "-c", `: > "$1"`, "sh", marker},
			},
		}}}
		got := walk(t, p, Options{})
		got.assertStatuses(t, StatusFailed)
		assertGone(t, marker)
	})
}

// TestExecuteSkipsTheEpilogue pins the division of labour: the tarball and the
// publish are in the plan, but Execute leaves them to the run wiring, which
// makes the tarball only after the final provenance writes.
func TestExecuteSkipsTheEpilogue(t *testing.T) {
	tmp := t.TempDir()
	marker := filepath.Join(tmp, "tarball.txt")
	tarball := plan.Step{
		ID: "tarball", Kind: plan.KindTarball,
		Argv: [][]string{{"/bin/sh", "-c", `: > "$1"`, "sh", marker}},
	}
	p := &plan.Plan{Steps: []plan.Step{
		shLeg("leg", filepath.Join(tmp, "res", "leg"), `: > "$1/driver.csv"`),
		tarball,
		{ID: "publish", Kind: plan.KindPublish, Argv: [][]string{}, PublishURI: "gs://bucket/runs", Needs: []string{"tarball"}},
	}}

	got := walk(t, p, Options{})
	if got.err != nil {
		t.Fatalf("Execute: %v\nlog:\n%s", got.err, got.log)
	}
	// Only the leg has a result, and no tar ran.
	got.assertStatuses(t, StatusOK)
	assertGone(t, marker)

	// The wiring runs the same step itself, out of the same plan.
	if err := RunStep(tarball, io.Discard); err != nil {
		t.Fatalf("RunStep(tarball): %v", err)
	}
	assertExists(t, marker)
}

func TestOnStepDoneFiresForExecutedSteps(t *testing.T) {
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "bin", "stellar-rpc-deadbeef")
	p := &plan.Plan{Bin: bin, Steps: []plan.Step{
		{ID: "build", Kind: plan.KindBuild, Argv: [][]string{{"/bin/sh", "-c", `mkdir -p "$(dirname "$1")" && : > "$1"`, "sh", bin}}},
		shLeg("fail", filepath.Join(tmp, "res", "fail"), `exit 1`),
		shLeg("dep", filepath.Join(tmp, "res", "dep"), `: > "$1/driver.csv"`),
	}}
	p.Steps[2].Needs = []string{"fail"}

	var seen []string
	var buf bytes.Buffer
	if _, err := Execute(p, Options{
		Output: &buf,
		OnStepDone: func(s plan.Step, r StepResult) {
			seen = append(seen, s.ID+"="+string(r.Status))
		},
	}); err == nil {
		t.Fatalf("Execute returned nil error after a failed leg\nlog:\n%s", buf.String())
	}
	// The skipped step is the one exception: it never ran, so nothing is done.
	if want := []string{"build=ok", "fail=failed"}; !slices.Equal(seen, want) {
		t.Errorf("OnStepDone saw %v, want %v", seen, want)
	}
}

func TestExecuteLegWithMissingBinaryIsFailedWithASentinel(t *testing.T) {
	tmp := t.TempDir()
	out := filepath.Join(tmp, "res", "ingest-cold-ds-c0-run1")
	p := &plan.Plan{Steps: []plan.Step{{
		ID: "leg", Kind: plan.KindLeg, Timed: true, OutDir: out,
		Argv: [][]string{{filepath.Join(tmp, "bin", "stellar-rpc-deadbeef"), "bench-ingest", "cold"}},
	}}}
	got := walk(t, p, Options{})
	got.assertStatuses(t, StatusFailed)
	// The binary never started, so it wrote no invocation.json — the sentinel
	// is the only record that this leg was attempted, which is exactly why the
	// executor creates the out dir itself.
	s := readSentinel(t, out)
	if s.ExitCode != -1 || s.Error == "" {
		t.Errorf("sentinel = %+v, want exit_code -1 and an error", s)
	}
}

func TestExecuteAllOKPrintsNoSummary(t *testing.T) {
	tmp := t.TempDir()
	p := &plan.Plan{Steps: []plan.Step{shLeg("a", filepath.Join(tmp, "a"), `: > "$1/driver.csv"`)}}
	got := walk(t, p, Options{})
	if got.err != nil {
		t.Fatalf("Execute: %v", got.err)
	}
	if strings.Contains(got.log, "campaign summary") {
		t.Errorf("an all-ok campaign printed a summary\nlog:\n%s", got.log)
	}
}

// --- lock -------------------------------------------------------------------

func TestAcquireLock(t *testing.T) {
	benchRoot := filepath.Join(t.TempDir(), "bench")
	release, err := AcquireLock(benchRoot)
	if err != nil {
		t.Fatalf("first AcquireLock: %v", err)
	}
	if _, err := AcquireLock(benchRoot); err == nil {
		t.Fatal("second AcquireLock succeeded while the lock was held")
	} else if want := "another campaign is already running on this BENCH_ROOT"; !strings.Contains(err.Error(), want) {
		t.Errorf("error = %q, want it to contain %q", err, want)
	}

	release()
	release2, err := AcquireLock(benchRoot)
	if err != nil {
		t.Fatalf("AcquireLock after release: %v", err)
	}
	release2()
	// The lock file outlives the lock: deleting it would race the next campaign.
	assertExists(t, filepath.Join(benchRoot, lockName))
}

// --- logging ------------------------------------------------------------------

func TestNotef(t *testing.T) {
	var buf bytes.Buffer
	Notef(&buf, "build %s → %s", "abc1234", "/bench/bin/stellar-rpc-abc1234")
	want := regexp.MustCompile(`^== \[\d{2}:\d{2}:\d{2}\] build abc1234 → /bench/bin/stellar-rpc-abc1234\n$`)
	if !want.MatchString(buf.String()) {
		t.Errorf("Notef wrote %q, want it to match %s", buf.String(), want)
	}
}

func TestOpenCampaignLogAppends(t *testing.T) {
	dir := t.TempDir()
	for _, session := range []string{"first\n", "second\n"} {
		f, err := OpenCampaignLog(dir)
		if err != nil {
			t.Fatalf("OpenCampaignLog: %v", err)
		}
		if _, err := f.WriteString(session); err != nil {
			t.Fatalf("write: %v", err)
		}
		f.Close()
	}
	b, err := os.ReadFile(filepath.Join(dir, campaignLogName))
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if string(b) != "first\nsecond\n" {
		t.Errorf("campaign.log = %q, want both sessions in order", b)
	}
}

// TestLegSentinelJSONShape pins the wire format: the sentinel is read by resume
// and by anything inspecting a bundle, so its keys are a contract.
func TestLegSentinelJSONShape(t *testing.T) {
	tmp := t.TempDir()
	out := filepath.Join(tmp, "res", "ingest-cold-ds-c0-run1")
	p := &plan.Plan{Steps: []plan.Step{shLeg("ingest-cold-ds-c0-run1", out, `exit 0`)}}
	if got := walk(t, p, Options{}); got.err != nil {
		t.Fatalf("Execute: %v\nlog:\n%s", got.err, got.log)
	}
	b, err := os.ReadFile(filepath.Join(out, legSentinelName))
	if err != nil {
		t.Fatalf("read sentinel: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("unmarshal sentinel: %v", err)
	}
	want := []string{"argv", "duration_ns", "exit_code", "finished_at", "id", "schema_version", "started_at"}
	got := make([]string, 0, len(raw))
	for k := range raw {
		got = append(got, k)
	}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("sentinel keys = %v, want %v", got, want)
	}
	// error is omitted on success, and only then.
	if _, ok := raw["error"]; ok {
		t.Errorf("a successful leg recorded an error: %s", b)
	}
}
