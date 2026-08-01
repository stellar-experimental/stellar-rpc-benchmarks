package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestParseArgs(t *testing.T) {
	cases := []struct {
		name string
		sub  string
		args []string
		pos  []string
		set  map[string]string // flag name → value after parsing
	}{
		{
			name: "positional then flag",
			sub:  "run",
			args: []string{"cfg.toml", "--dry-run"},
			pos:  []string{"cfg.toml"},
			set:  map[string]string{"dry-run": "true"},
		},
		{
			name: "flag then positional",
			sub:  "run",
			args: []string{"--dry-run", "cfg.toml"},
			pos:  []string{"cfg.toml"},
			set:  map[string]string{"dry-run": "true"},
		},
		{
			name: "flag between two positionals",
			sub:  "publish",
			args: []string{"results", "--force", "dest"},
			pos:  []string{"results", "dest"},
			set:  map[string]string{"force": "true"},
		},
		{
			name: "no arguments at all",
			sub:  "run",
			args: nil,
			pos:  nil,
			set:  map[string]string{"dry-run": "false"},
		},
		{
			name: "terminator makes flag-shaped positionals verbatim",
			sub:  "publish",
			args: []string{"--", "-results", "-dest"},
			pos:  []string{"-results", "-dest"},
			set:  map[string]string{"force": "false"},
		},
		{
			name: "terminator after a peeled positional",
			sub:  "publish",
			args: []string{"results", "--", "--force"},
			pos:  []string{"results", "--force"},
			set:  map[string]string{"force": "false"},
		},
		{
			name: "flag before the terminator still parses",
			sub:  "publish",
			args: []string{"--force", "--", "x"},
			pos:  []string{"x"},
			set:  map[string]string{"force": "true"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := subFlags(tc.sub, io.Discard)
			pos, err := parseArgs(fs, tc.args)
			if err != nil {
				t.Fatalf("parseArgs(%q) = error %v, want nil", tc.args, err)
			}
			if !slices.Equal(pos, tc.pos) {
				t.Errorf("positionals = %q, want %q", pos, tc.pos)
			}
			for name, want := range tc.set {
				if got := fs.Lookup(name).Value.String(); got != want {
					t.Errorf("flag %s = %s, want %s", name, got, want)
				}
			}
		})
	}
}

func TestParseArgsRejectsUnknownFlagAfterPositional(t *testing.T) {
	fs := subFlags("run", io.Discard)
	if _, err := parseArgs(fs, []string{"cfg.toml", "--bogus"}); err == nil {
		t.Fatal("parseArgs = nil error, want an error for -bogus")
	}
}

const planConfig = `
name = "cli"
ingest = "cold"
query = false
runs = 1

[[dataset]]
name = "ds"
kind = "packs-local"
location = "/packs/ds"
chunks = [1]
`

func TestPlanCmd(t *testing.T) {
	t.Run("unreadable config exits 2 with the config error", func(t *testing.T) {
		t.Setenv("BENCH_ROOT", t.TempDir())
		var stdout, stderr bytes.Buffer
		if got := dispatch([]string{"plan", filepath.Join(t.TempDir(), "nope.toml")}, &stdout, &stderr); got != 2 {
			t.Errorf("exit code = %d, want 2", got)
		}
		if !strings.Contains(stderr.String(), "config:") {
			t.Errorf("stderr = %q, want the config error", stderr.String())
		}
	})

	t.Run("a valid config plans against a bench root with no clone", func(t *testing.T) {
		benchRoot := t.TempDir()
		t.Setenv("BENCH_ROOT", benchRoot)
		cfg := filepath.Join(t.TempDir(), "campaign.toml")
		if err := os.WriteFile(cfg, []byte(planConfig), 0o644); err != nil {
			t.Fatalf("write config: %v", err)
		}
		var stdout, stderr bytes.Buffer
		if got := dispatch([]string{"plan", cfg}, &stdout, &stderr); got != 0 {
			t.Errorf("exit code = %d, want 0 (stderr: %s)", got, stderr.String())
		}
		for _, want := range []string{
			"using placeholder sha 'deadbeef' in paths",
			"== build\n",
			filepath.Join(benchRoot, "bin", "stellar-rpc-deadbeef"),
			"== ingest-cold-ds-c1-run1\n",
		} {
			if !strings.Contains(stdout.String(), want) {
				t.Errorf("stdout missing %q, got:\n%s", want, stdout.String())
			}
		}
		if stderr.Len() != 0 {
			t.Errorf("stderr = %q, want empty", stderr.String())
		}
	})
}

// stubPATH points PATH at a directory of no-op executables, so preflight's
// tool checks see exactly the named tools and nothing else.
func stubPATH(t *testing.T, tools ...string) {
	t.Helper()
	dir := t.TempDir()
	for _, tool := range tools {
		if err := os.WriteFile(filepath.Join(dir, tool), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatalf("write %s: %v", tool, err)
		}
	}
	t.Setenv("PATH", dir)
}

func TestPreflightCmd(t *testing.T) {
	t.Run("unreadable config exits 2 with the config error", func(t *testing.T) {
		t.Setenv("BENCH_ROOT", t.TempDir())
		var stdout, stderr bytes.Buffer
		if got := dispatch([]string{"preflight", filepath.Join(t.TempDir(), "nope.toml")}, &stdout, &stderr); got != 2 {
			t.Errorf("exit code = %d, want 2", got)
		}
		if !strings.Contains(stderr.String(), "config:") {
			t.Errorf("stderr = %q, want the config error", stderr.String())
		}
	})

	// planConfig publishes nowhere and uses a local pack root, so the only
	// tools it needs are the clone-and-build set. binPath is empty here, so a
	// build is always assumed — hence go and cargo.
	t.Run("a config needing only the build tools passes", func(t *testing.T) {
		t.Setenv("BENCH_ROOT", t.TempDir())
		stubPATH(t, "git", "make", "go", "cargo")
		cfg := filepath.Join(t.TempDir(), "campaign.toml")
		if err := os.WriteFile(cfg, []byte(planConfig), 0o644); err != nil {
			t.Fatalf("write config: %v", err)
		}
		var stdout, stderr bytes.Buffer
		if got := dispatch([]string{"preflight", cfg}, &stdout, &stderr); got != 0 {
			t.Errorf("exit code = %d, want 0 (stdout: %s)", got, stdout.String())
		}
		if !strings.Contains(stdout.String(), "preflight: ok") {
			t.Errorf("stdout = %q, want 'preflight: ok'", stdout.String())
		}
		if strings.Contains(stdout.String(), "FAIL") {
			t.Errorf("stdout = %q, want no failures", stdout.String())
		}
	})

	t.Run("a missing tool fails, exit 1", func(t *testing.T) {
		t.Setenv("BENCH_ROOT", t.TempDir())
		stubPATH(t, "make", "go", "cargo")
		cfg := filepath.Join(t.TempDir(), "campaign.toml")
		if err := os.WriteFile(cfg, []byte(planConfig), 0o644); err != nil {
			t.Fatalf("write config: %v", err)
		}
		var stdout, stderr bytes.Buffer
		if got := dispatch([]string{"preflight", cfg}, &stdout, &stderr); got != 1 {
			t.Errorf("exit code = %d, want 1 (stdout: %s)", got, stdout.String())
		}
		if !strings.Contains(stdout.String(), "preflight: FAIL — git not found in PATH") {
			t.Errorf("stdout = %q, want the git failure line", stdout.String())
		}
	})
}

func TestPublishCmd(t *testing.T) {
	// A gcloud that reports an empty prefix and accepts the upload, so the
	// whole subcommand runs end to end without touching a bucket.
	stubGcloud := func(t *testing.T) {
		t.Helper()
		dir := t.TempDir()
		script := "#!/bin/sh\ncase \"$2\" in\nls) echo 'ERROR: One or more URLs matched no objects.' >&2; exit 1 ;;\n*) exit 0 ;;\nesac\n"
		if err := os.WriteFile(filepath.Join(dir, "gcloud"), []byte(script), 0o755); err != nil {
			t.Fatalf("write fake gcloud: %v", err)
		}
		t.Setenv("PATH", dir)
	}
	bundleDir := func(t *testing.T) string {
		t.Helper()
		dir := filepath.Join(t.TempDir(), "phase4-6f35679f-20260715T101500Z")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir bundle: %v", err)
		}
		return dir
	}

	t.Run("a bundle and $PUBLISH_URI are enough", func(t *testing.T) {
		stubGcloud(t)
		dir := bundleDir(t)
		t.Setenv("PUBLISH_URI", "gs://bucket/results")
		var stdout, stderr bytes.Buffer
		if got := dispatch([]string{"publish", dir}, &stdout, &stderr); got != 0 {
			t.Errorf("exit code = %d, want 0 (stderr: %s)", got, stderr.String())
		}
		want := "published: gs://bucket/results/" + filepath.Base(dir) + "/"
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("stdout missing %q, got:\n%s", want, stdout.String())
		}
	})

	t.Run("no destination anywhere exits 2", func(t *testing.T) {
		t.Setenv("PUBLISH_URI", "")
		var stdout, stderr bytes.Buffer
		if got := dispatch([]string{"publish", bundleDir(t)}, &stdout, &stderr); got != 2 {
			t.Errorf("exit code = %d, want 2", got)
		}
		if !strings.Contains(stderr.String(), "no destination: pass <dest-root-uri> or set PUBLISH_URI") {
			t.Errorf("stderr = %q, want the no-destination error", stderr.String())
		}
	})

	t.Run("a missing bundle is an operational failure, exit 1", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		if got := dispatch([]string{"publish", filepath.Join(t.TempDir(), "nope"), "gs://bucket/results"}, &stdout, &stderr); got != 1 {
			t.Errorf("exit code = %d, want 1", got)
		}
		if !strings.Contains(stderr.String(), "error: results dir not found: ") {
			t.Errorf("stderr = %q, want the missing-bundle error", stderr.String())
		}
	})

	t.Run("--dry-run prints the commands and runs none", func(t *testing.T) {
		stubPATH(t) // an empty PATH: a dry run must not need the CLI at all
		dir := bundleDir(t)
		var stdout, stderr bytes.Buffer
		if got := dispatch([]string{"publish", dir, "gs://bucket/results", "--dry-run"}, &stdout, &stderr); got != 0 {
			t.Errorf("exit code = %d, want 0 (stderr: %s)", got, stderr.String())
		}
		for _, want := range []string{"  $ gcloud storage ls ", "  $ gcloud storage rsync -r ", "dry run complete"} {
			if !strings.Contains(stdout.String(), want) {
				t.Errorf("stdout missing %q, got:\n%s", want, stdout.String())
			}
		}
	})
}

func TestRunDispatch(t *testing.T) {
	cases := []struct {
		name      string
		args      []string
		exit      int
		stderr    []string // substrings expected on stderr
		notStderr []string // substrings that must not appear on stderr
		stdout    []string // substrings expected on stdout
		noStderr  bool
	}{
		{
			name:   "no args prints top usage",
			args:   nil,
			exit:   2,
			stderr: []string{"usage: campaign <subcommand>", "run", "plan", "preflight", "publish", "BENCH_ROOT"},
		},
		{
			name:     "--help prints top usage on stdout",
			args:     []string{"--help"},
			exit:     0,
			stdout:   []string{"usage: campaign <subcommand>", "preflight"},
			noStderr: true,
		},
		{
			name:   "unknown subcommand names the bad argument",
			args:   []string{"benchmark"},
			exit:   2,
			stderr: []string{"error: unknown subcommand: benchmark", "usage: campaign <subcommand>"},
		},
		{
			name:   "run without a config names what is missing",
			args:   []string{"run"},
			exit:   2,
			stderr: []string{"usage: campaign run <config.toml>", "--resume", "error: run needs exactly one config path"},
		},
		{
			name:      "run -h prints its usage and succeeds",
			args:      []string{"run", "-h"},
			exit:      0,
			stderr:    []string{"usage: campaign run <config.toml>", "--no-preflight"},
			notStderr: []string{"error:"},
		},
		{
			name:   "unknown flag names the flag",
			args:   []string{"run", "--bogus"},
			exit:   2,
			stderr: []string{"not defined: -bogus", "usage: campaign run <config.toml>"},
		},
		{
			name:   "unknown flag after a positional names the flag",
			args:   []string{"run", "cfg.toml", "--bogus"},
			exit:   2,
			stderr: []string{"not defined: -bogus", "usage: campaign run <config.toml>"},
		},
		{
			name:   "plan without a config names what is missing",
			args:   []string{"plan"},
			exit:   2,
			stderr: []string{"usage: campaign plan <config.toml>", "error: plan needs exactly one config path"},
		},
		{
			name:   "preflight without a config names what is missing",
			args:   []string{"preflight"},
			exit:   2,
			stderr: []string{"usage: campaign preflight <config.toml>", "error: preflight needs exactly one config path"},
		},
		{
			name:   "publish without a bundle names what is missing",
			args:   []string{"publish"},
			exit:   2,
			stderr: []string{"usage: campaign publish <results-dir>", "--force", "error: publish needs a results directory"},
		},
		{
			name:   "publish with a third positional names it",
			args:   []string{"publish", "results", "gs://bucket", "extra"},
			exit:   2,
			stderr: []string{"error: unexpected extra argument: extra"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			got := dispatch(tc.args, &stdout, &stderr)
			if got != tc.exit {
				t.Errorf("exit code = %d, want %d", got, tc.exit)
			}
			for _, want := range tc.stderr {
				if !strings.Contains(stderr.String(), want) {
					t.Errorf("stderr missing %q, got:\n%s", want, stderr.String())
				}
			}
			for _, unwanted := range tc.notStderr {
				if strings.Contains(stderr.String(), unwanted) {
					t.Errorf("stderr contains %q, want it absent, got:\n%s", unwanted, stderr.String())
				}
			}
			for _, want := range tc.stdout {
				if !strings.Contains(stdout.String(), want) {
					t.Errorf("stdout missing %q, got:\n%s", want, stdout.String())
				}
			}
			if tc.noStderr && stderr.Len() != 0 {
				t.Errorf("stderr = %q, want empty", stderr.String())
			}
		})
	}
}
