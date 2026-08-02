//go:build !windows

package main

// End-to-end tests: the real campaign binary, exec'd against a temp BENCH_ROOT
// with a fake toolchain on PATH and a stub standing in for stellar-rpc. Nothing
// else in this repo proves the pieces compose — config, plan, executor, bundle
// writers, resume, and the epilogue — and scenario ConverterOverBundle proves
// the bundle they produce is one converter/convert.py can convert, which is the
// cross-repo contract these benchmarks exist to keep.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// stubCommit is what the fake git resolves every ref to and what the stub
// binary reports as its build identity; the two must agree or the converter
// warns about a commit mismatch. stubSha8 is the short sha every derived path
// carries.
const (
	stubCommit = "0123456789abcdef0123456789abcdef01234567"
	stubSha8   = "01234567"
)

// waitTimeout caps every poll in this file: a regression that stops a campaign
// from reaching a leg should fail the test in seconds, not hang the suite.
// runTimeout does the same for a whole campaign invocation, and killGrace is
// how long a killed group has to release the output pipe before Wait gives up
// on it.
const (
	waitTimeout = 30 * time.Second
	runTimeout  = 60 * time.Second
	killGrace   = 5 * time.Second
)

// legNames are the eight timed legs the e2e config produces, in plan order.
var legNames = []string{
	"ingest-cold-fix-c1-run1", "ingest-cold-fix-c1-run2",
	"ingest-hot-fix-c1-run1", "ingest-hot-fix-c1-run2",
	"query-cold-fix-c1-run1", "query-cold-fix-c1-run2",
	"query-hot-fix-c1-run1", "query-hot-fix-c1-run2",
}

var reBundleName = regexp.MustCompile(`^e2e-` + stubSha8 + `-[0-9]{8}T[0-9]{6}Z$`)

func TestE2E(t *testing.T) {
	work := t.TempDir()
	bin := buildCampaign(t, work)
	cfgPath := absPath(t, filepath.Join("testdata", "e2e-campaign.toml"))

	// ConverterOverBundle converts the bundle HappyPath produced, so the two
	// share it through the parent test rather than each running a campaign.
	var happyBundle string

	t.Run("HappyPath", func(t *testing.T) {
		sc := newScenario(t, bin, filepath.Join(work, "happy"), cfgPath)
		code, out := sc.run(t, "run", sc.cfg, "--no-preflight")
		if code != 0 {
			t.Fatalf("exit code = %d, want 0; output:\n%s", code, out)
		}
		bundle := sc.bundle(t)
		happyBundle = bundle

		if name := filepath.Base(bundle); !reBundleName.MatchString(name) {
			t.Errorf("bundle name = %q, want e2e-<sha8>-<stamp>", name)
		}
		// The untimed prep dir the golden freeze wrote, and the eight timed legs.
		mustExist(t, filepath.Join(bundle, "golden-fix-c1"))
		for _, leg := range legNames {
			dir := filepath.Join(bundle, leg)
			for _, f := range []string{"driver.csv", "invocation.json", "leg.json"} {
				mustExist(t, filepath.Join(dir, f))
			}
			if got := readLeg(t, dir).ExitCode; got != 0 {
				t.Errorf("%s: leg.json exit_code = %d, want 0", leg, got)
			}
		}
		for _, f := range []string{"plan.json", "binary.txt", "machine-metadata.txt", "campaign.log"} {
			mustExist(t, filepath.Join(bundle, f))
		}

		meta := readMetadata(t, bundle)
		if meta.FinishedAt == "" {
			t.Error("metadata.json has no finished_at")
		}
		if meta.Status != "finished" {
			t.Errorf("metadata.json status = %q, want finished", meta.Status)
		}
		// Absent, not false: the manifest omits resumed entirely on a campaign
		// that ran in one session.
		if raw := readFile(t, filepath.Join(bundle, "metadata.json")); strings.Contains(raw, `"resumed"`) {
			t.Errorf("metadata.json records resumed on a fresh campaign:\n%s", raw)
		}

		// The cold scratch store is post-cleaned the moment its rep is done; the
		// hot DB is deliberately kept, because query-hot reads what it holds.
		mustNotExist(t, filepath.Join(sc.root, "scratch", "fix", "1"))
		mustExist(t, filepath.Join(sc.root, "hot", "fix", "1"))
	})

	t.Run("KillMidLegResume", func(t *testing.T) {
		sc := newScenario(t, bin, filepath.Join(work, "resume"), cfgPath)
		sc.setControl("hang ingest-hot-fix-c1-run2")

		proc := sc.start(t, "run", sc.cfg, "--no-preflight")
		bundle := waitForBundle(t, sc.root)
		killed := filepath.Join(bundle, "ingest-hot-fix-c1-run2")
		waitFor(t, "the hung leg's out dir", func() bool { return exists(killed) })
		// The whole process group: the stub's sleep must die with the runner.
		proc.terminate()
		if s := proc.out.String(); !strings.Contains(s, "ingest-hot-fix-c1-run2") {
			t.Fatalf("killed before the campaign reached the hung leg; output:\n%s", s)
		}

		sc.setControl("")
		code, resumeOut := sc.run(t, "run", sc.cfg, "--no-preflight", "--resume", bundle)
		if code != 0 {
			t.Fatalf("resume exit code = %d, want 0; output:\n%s", code, resumeOut)
		}

		log := readFile(t, filepath.Join(bundle, "campaign.log"))
		for _, want := range []string{
			"session start",
			"session resume",
			"resume: ingest-cold-fix-c1-run1 already complete — skipping",
			"resume: ingest-hot-fix-c1-run2 is a partial leg — wiping and re-running",
		} {
			if !strings.Contains(log, want) {
				t.Errorf("campaign.log missing %q", want)
			}
		}
		if got := readLeg(t, killed).ExitCode; got != 0 {
			t.Errorf("re-run leg exit_code = %d, want 0", got)
		}
		meta := readMetadata(t, bundle)
		if !meta.Campaign.Resumed {
			t.Error("metadata.json does not record resumed")
		}
		if meta.Status != "finished" {
			t.Errorf("metadata.json status = %q, want finished", meta.Status)
		}
	})

	t.Run("FailedLegKeepGoing", func(t *testing.T) {
		sc := newScenario(t, bin, filepath.Join(work, "failed"), cfgPath)
		// The last hot rep: query-hot depends on it, so its failure is what
		// takes the hot query suite down with it.
		sc.setControl("fail ingest-hot-fix-c1-run2")

		code, out := sc.run(t, "run", sc.cfg, "--no-preflight")
		if code == 0 {
			t.Fatalf("exit code = 0, want nonzero; output:\n%s", out)
		}
		bundle := sc.bundle(t)
		log := readFile(t, filepath.Join(bundle, "campaign.log"))
		for _, want := range []string{
			"skipping query-hot-fix-c1-run1: needs ingest-hot-fix-c1-run2",
			"skipping query-hot-fix-c1-run2: needs ingest-hot-fix-c1-run2",
			"== campaign summary:",
		} {
			if !strings.Contains(log, want) {
				t.Errorf("campaign.log missing %q", want)
			}
		}
		// A hot failure must not take the cold query suite with it: it needs
		// nothing the failed leg produced.
		for _, leg := range []string{"query-cold-fix-c1-run1", "query-cold-fix-c1-run2"} {
			if got := readLeg(t, filepath.Join(bundle, leg)).ExitCode; got != 0 {
				t.Errorf("%s: leg.json exit_code = %d, want 0", leg, got)
			}
		}
		mustNotExist(t, filepath.Join(bundle, "query-hot-fix-c1-run1"))

		failed := filepath.Join(bundle, "ingest-hot-fix-c1-run2")
		leg := readLeg(t, failed)
		if leg.ExitCode != 1 || leg.Error == "" {
			t.Errorf("failed leg.json = exit_code %d, error %q; want exit_code 1 with an error",
				leg.ExitCode, leg.Error)
		}
		if inv := readFile(t, filepath.Join(failed, "invocation.json")); !strings.Contains(inv, `"error": "induced failure"`) {
			t.Errorf("failed leg's invocation.json carries no error:\n%s", inv)
		}

		meta := readMetadata(t, bundle)
		if meta.Status != "failed" {
			t.Errorf("metadata.json status = %q, want failed", meta.Status)
		}
		if meta.FinishedAt == "" {
			t.Error("a failed campaign's metadata.json has no finished_at")
		}
	})

	t.Run("ResumeRefusesEditedConfig", func(t *testing.T) {
		// The resume runs from a copy: editing the file the operator passed must
		// leave the bundle's stored copy — what the guard compares against —
		// untouched.
		original := readFile(t, cfgPath)
		working := filepath.Join(work, "edited-cfg")
		mkdirAll(t, working)
		copyPath := filepath.Join(working, "e2e-campaign.toml")
		writeFile(t, copyPath, original)

		sc := newScenario(t, bin, filepath.Join(work, "edited"), copyPath)
		if code, out := sc.run(t, "run", sc.cfg, "--no-preflight"); code != 0 {
			t.Fatalf("exit code = %d, want 0; output:\n%s", code, out)
		}
		bundle := sc.bundle(t)

		writeFile(t, copyPath, original+"\n# an operator edit between sessions\n")
		code, out := sc.run(t, "run", sc.cfg, "--no-preflight", "--resume", bundle)
		if code != 1 {
			t.Fatalf("resume exit code = %d, want 1; output:\n%s", code, out)
		}
		if !strings.Contains(out, "--resume: the config differs") {
			t.Errorf("output does not refuse the edited config:\n%s", out)
		}
		if stored := readFile(t, filepath.Join(bundle, "e2e-campaign.toml")); stored != original {
			t.Errorf("the bundle's stored config changed:\n%s", stored)
		}
	})

	t.Run("ConverterOverBundle", func(t *testing.T) {
		if happyBundle == "" {
			t.Skip("HappyPath produced no bundle to convert")
		}
		python, err := exec.LookPath("python3")
		if err != nil {
			t.Skip("python3 not available")
		}
		convert := absPath(t, filepath.Join("..", "..", "..", "converter", "convert.py"))
		outDir := t.TempDir()
		ctx, cancel := context.WithTimeout(context.Background(), runTimeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, python, convert, happyBundle,
			"--run-id", "e2e-test", "--run-name", "E2E", "--run-date", "2026-08-01",
			"--dataset-kind", "synthetic", "--out-dir", outDir)
		// Warnings are expected: the golden-* prep dirs are dataset preparation,
		// and the converter says so every time it skips them.
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("convert.py: %v\n%s", err, out)
		}
		mustExist(t, filepath.Join(outDir, "e2e-test.json"))
	})

	t.Run("TarFailureGatesPublish", func(t *testing.T) {
		// A publish-configured campaign whose tar fails: the tarball is the
		// artifact publish uploads, so publish must not be reached.
		cfgBody := strings.Replace(readFile(t, cfgPath), "[[dataset]]",
			"publish_uri = \"gs://e2e-bucket/results\"\n\n[[dataset]]", 1)
		cfgCopy := filepath.Join(work, "publish-cfg", "e2e-campaign.toml")
		mkdirAll(t, filepath.Dir(cfgCopy))
		writeFile(t, cfgCopy, cfgBody)

		sc := newScenario(t, bin, filepath.Join(work, "tarfail"), cfgCopy)
		sc.breakTar()

		code, out := sc.run(t, "run", sc.cfg, "--no-preflight")
		if code != 1 {
			t.Fatalf("exit code = %d, want 1; output:\n%s", code, out)
		}
		bundle := sc.bundle(t)
		log := readFile(t, filepath.Join(bundle, "campaign.log"))
		for _, want := range []string{"warning: tar failed", "skipping publish: tar failed"} {
			if !strings.Contains(log, want) {
				t.Errorf("campaign.log missing %q", want)
			}
		}
		if strings.Contains(log, "published:") {
			t.Errorf("campaign.log reports a publish after a failed tar:\n%s", log)
		}
		// tar is the epilogue's archiver, not a leg: the legs all passed, and
		// the status is theirs.
		if meta := readMetadata(t, bundle); meta.Status != "finished" {
			t.Errorf("metadata.json status = %q, want finished", meta.Status)
		}
	})
}

// ------------------------------------------------------------------ scenario

// scenario is one campaign's world: its own BENCH_ROOT, its own control file,
// and a fake toolchain on PATH ahead of the real one, so git, make, and (when a
// test asks for it) tar answer the way that scenario needs while the real sh,
// mv, and python3 stay reachable.
type scenario struct {
	bin     string   // the campaign binary under test
	cfg     string   // the config path passed on the CLI
	root    string   // BENCH_ROOT
	control string   // STUB_RPC_CONTROL
	path    []string // dirs prepended to PATH, in order
}

func newScenario(t *testing.T, bin, root, cfg string) *scenario {
	t.Helper()
	sc := &scenario{bin: bin, cfg: cfg, root: root, control: filepath.Join(root, "control")}
	// The build clone is a black box to the runner: a .git is all EnsureSrc and
	// ResolveRef look for before handing the work to git, which is fake here.
	src := filepath.Join(root, "src")
	mkdirAll(t, filepath.Join(src, ".git"))
	writeFile(t, sc.control, "")

	stubs := filepath.Join(root, "toolchain")
	mkdirAll(t, stubs)
	writeExec(t, filepath.Join(stubs, "git"), fmt.Sprintf(`#!/bin/sh
# Fake git: every rev-parse resolves to one fixed commit, and the clone,
# fetch, reset, and checkout the runner drives are no-ops.
for arg in "$@"; do
	if [ "$arg" = rev-parse ]; then
		echo %s
		exit 0
	fi
done
exit 0
`, stubCommit))
	writeExec(t, filepath.Join(stubs, "make"), fmt.Sprintf(`#!/bin/sh
# Fake make: build-rpc-v2 installs the stub binary where the real target leaves
# it, for the runner's own mv to move into the versioned path. Every other
# target succeeds without doing anything.
src=
prev=
target=
for arg in "$@"; do
	if [ "$prev" = -C ]; then src=$arg; fi
	if [ "$arg" = build-rpc-v2 ]; then target=$arg; fi
	prev=$arg
done
[ -n "$target" ] || exit 0
cp %s "$src/stellar-rpc-v2"
chmod +x "$src/stellar-rpc-v2"
`, absPath(t, filepath.Join("testdata", "stub-rpc.sh"))))
	sc.path = []string{stubs}

	// Every campaign tars its bundle into /tmp, a path the plan owns; take the
	// tarball with the scenario rather than leaving it behind.
	t.Cleanup(func() {
		bundles, _ := filepath.Glob(filepath.Join(root, "results", "*"))
		for _, b := range bundles {
			os.Remove("/tmp/bench-results-" + filepath.Base(b) + ".tgz")
		}
	})
	return sc
}

// setControl rewrites the stub's control file. An empty body clears it.
func (sc *scenario) setControl(body string) {
	if body != "" {
		body += "\n"
	}
	if err := os.WriteFile(sc.control, []byte(body), 0o644); err != nil {
		panic(err)
	}
}

// breakTar puts a failing tar ahead of everything else on PATH, for the one
// scenario that needs the archiver to fail.
func (sc *scenario) breakTar() {
	dir := filepath.Join(sc.root, "broken-tar")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		panic(err)
	}
	body := "#!/bin/sh\necho 'tar: stub failure' >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "tar"), []byte(body), 0o755); err != nil {
		panic(err)
	}
	sc.path = append([]string{dir}, sc.path...)
}

func (sc *scenario) env() []string {
	var env []string
	for _, kv := range os.Environ() {
		switch {
		case strings.HasPrefix(kv, "PATH="),
			strings.HasPrefix(kv, "BENCH_ROOT="),
			// PUBLISH_URI would give `campaign publish` a destination the
			// scenario never asked for.
			strings.HasPrefix(kv, "PUBLISH_URI="),
			strings.HasPrefix(kv, "STUB_RPC_"):
		default:
			env = append(env, kv)
		}
	}
	path := append(append([]string{}, sc.path...), os.Getenv("PATH"))
	return append(env,
		"PATH="+strings.Join(path, string(os.PathListSeparator)),
		"BENCH_ROOT="+sc.root,
		"STUB_RPC_CONTROL="+sc.control,
		"STUB_RPC_COMMIT="+stubCommit,
	)
}

// run executes the campaign binary to completion, returning its exit code and
// its combined output. The campaign gets its own process group and a deadline:
// a runner that wedges fails this test rather than the suite.
func (sc *scenario) run(t *testing.T, args ...string) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), runTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, sc.bin, args...)
	cmd.Env = sc.env()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// The group, not the leader: the stub's children hold the output pipe, and
	// killing only the campaign would leave them writing into it.
	cmd.Cancel = func() error { killGroup(cmd); return nil }
	cmd.WaitDelay = killGrace
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("campaign did not finish within %s; output:\n%s", runTimeout, out)
	}
	code := 0
	if err != nil {
		code = cmd.ProcessState.ExitCode()
	}
	return code, string(out)
}

// started is one running campaign: the output it is still writing, and the one
// call that ends it.
type started struct {
	out *syncBuffer
	// terminate SIGKILLs the whole process group and reaps it, at most once.
	// Signalling a group twice is not safe: after the first call reaps the
	// leader the kernel may hand that PGID to an unrelated process, and the
	// second signal would land on it.
	terminate func()
}

// start launches the campaign in its own process group, so a test can kill the
// runner and everything it spawned the way an operator's ^C would not.
func (sc *scenario) start(t *testing.T, args ...string) *started {
	t.Helper()
	cmd := exec.Command(sc.bin, args...)
	cmd.Env = sc.env()
	buf := &syncBuffer{}
	cmd.Stdout, cmd.Stderr = buf, buf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = killGrace
	if err := cmd.Start(); err != nil {
		t.Fatalf("start campaign: %v", err)
	}
	var once sync.Once
	proc := &started{out: buf, terminate: func() {
		once.Do(func() {
			killGroup(cmd)
			_ = cmd.Wait()
		})
	}}
	// Whatever ends this test — a passing kill, or a poll deadline that fails
	// it before the kill — the campaign and its sleeping stub die with it.
	t.Cleanup(proc.terminate)
	return proc
}

// killGroup SIGKILLs a started command's whole process group.
func killGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}

// bundle is the single results directory this scenario produced.
func (sc *scenario) bundle(t *testing.T) string {
	t.Helper()
	hits, err := filepath.Glob(filepath.Join(sc.root, "results", "e2e-*"))
	if err != nil || len(hits) != 1 {
		t.Fatalf("results dirs = %v (err %v), want exactly one", hits, err)
	}
	return hits[0]
}

// waitForBundle polls for the results directory of a campaign that is still
// running.
func waitForBundle(t *testing.T, root string) string {
	t.Helper()
	var found string
	waitFor(t, "the results directory", func() bool {
		hits, _ := filepath.Glob(filepath.Join(root, "results", "e2e-*"))
		if len(hits) == 1 {
			found = hits[0]
			return true
		}
		return false
	})
	return found
}

// waitFor polls cond until it holds or waitTimeout expires.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", waitTimeout, what)
}

// syncBuffer is a bytes.Buffer a test may read while the child process is still
// writing into it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// ------------------------------------------------------------------- helpers

func buildCampaign(t *testing.T, dir string) string {
	t.Helper()
	bin := filepath.Join(dir, "campaign")
	out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput()
	if err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

// legJSON is the runner's completion sentinel, read back the way an operator
// diagnosing a bundle would.
type legJSON struct {
	ExitCode int    `json:"exit_code"`
	Error    string `json:"error"`
}

func readLeg(t *testing.T, dir string) legJSON {
	t.Helper()
	var leg legJSON
	readJSON(t, filepath.Join(dir, "leg.json"), &leg)
	return leg
}

// metadataJSON is the sliver of the bundle manifest these tests assert on.
type metadataJSON struct {
	Campaign struct {
		Resumed bool `json:"resumed"`
	} `json:"campaign"`
	FinishedAt string `json:"finished_at"`
	Status     string `json:"status"`
}

func readMetadata(t *testing.T, bundle string) metadataJSON {
	t.Helper()
	var meta metadataJSON
	readJSON(t, filepath.Join(bundle, "metadata.json"), &meta)
	return meta
}

func readJSON(t *testing.T, path string, into any) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if err := json.Unmarshal(b, into); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func writeExec(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func mkdirAll(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir -p %s: %v", dir, err)
	}
}

func absPath(t *testing.T, path string) string {
	t.Helper()
	abs, err := filepath.Abs(path)
	if err != nil {
		t.Fatalf("abs %s: %v", path, err)
	}
	return abs
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func mustExist(t *testing.T, path string) {
	t.Helper()
	if !exists(path) {
		t.Errorf("missing: %s", path)
	}
}

func mustNotExist(t *testing.T, path string) {
	t.Helper()
	if exists(path) {
		t.Errorf("present, want absent: %s", path)
	}
}
