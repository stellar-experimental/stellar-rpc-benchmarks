package preflight

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stellar/stellar-rpc-benchmarks/runner/internal/config"
)

// testConfig is a campaign that needs nothing beyond a toolchain: a local pack
// root, no publishing. Each test mutates the one field its check reads.
func testConfig(mods ...func(*config.Config)) *config.Config {
	cfg := &config.Config{
		Name: "pf",
		Repo: config.DefaultRepo,
		Ref:  config.DefaultRef,
		Datasets: []config.Dataset{
			{Name: "ds", Kind: config.KindPacksLocal, Location: "/packs/ds", Chunks: []int{1}},
		},
	}
	for _, mod := range mods {
		mod(cfg)
	}
	return cfg
}

// okDeps is an environment where every check passes, so a test that changes
// one dep sees exactly one failure.
func okDeps() Deps {
	return Deps{
		LookPath:   func(file string) (string, error) { return "/usr/bin/" + file, nil },
		ListRoot:   func(string) error { return nil },
		Mountpoint: func(string) error { return nil },
		DiskFree:   func(string) (uint64, error) { return 500 * gib, nil },
	}
}

// missing returns a LookPath that finds everything except the named tools.
func missing(tools ...string) func(string) (string, error) {
	return func(file string) (string, error) {
		for _, t := range tools {
			if t == file {
				return "", os.ErrNotExist
			}
		}
		return "/usr/bin/" + file, nil
	}
}

func has(t *testing.T, msgs []string, want string) {
	t.Helper()
	for _, m := range msgs {
		if strings.Contains(m, want) {
			return
		}
	}
	t.Errorf("no message contains %q, got %q", want, msgs)
}

// stubPATH points PATH at a directory of no-op executables, so the real
// exec.LookPath finds exactly the named tools and nothing else.
func stubPATH(t *testing.T, tools ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, tool := range tools {
		writeScript(t, filepath.Join(dir, tool), "#!/bin/sh\nexit 0\n")
	}
	t.Setenv("PATH", dir)
	return dir
}

func writeScript(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestToolChecks(t *testing.T) {
	cases := []struct {
		name   string
		absent []string
		want   string // substring of the expected failure
	}{
		{name: "git", absent: []string{"git"}, want: "git not found in PATH — the campaign clones and builds repo '" + config.DefaultRepo + "'"},
		{name: "make", absent: []string{"make"}, want: "make not found in PATH — the campaign builds repo '" + config.DefaultRepo + "'"},
		{name: "go", absent: []string{"go"}, want: "go not found in PATH — building ref '" + config.DefaultRef + "'"},
		{name: "cargo", absent: []string{"cargo"}, want: "cargo not found in PATH or "},
	}
	for _, tc := range cases {
		t.Run("missing "+tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir()) // no rustup install to fall back to
			d := okDeps()
			d.LookPath = missing(tc.absent...)
			res := Run(testConfig(), t.TempDir(), "", d)
			if len(res.Failures) != 1 {
				t.Fatalf("failures = %q, want exactly one", res.Failures)
			}
			has(t, res.Failures, tc.want)
		})
	}
}

func TestCargoFoundUnderHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeScript(t, filepath.Join(home, ".cargo", "bin", "cargo"), "#!/bin/sh\nexit 0\n")

	d := okDeps()
	d.LookPath = missing("cargo")
	res := Run(testConfig(), t.TempDir(), "", d)
	if len(res.Failures) != 0 {
		t.Errorf("failures = %q, want none: rustup's cargo is outside PATH but usable", res.Failures)
	}
}

func TestExistingBinarySkipsToolchainChecks(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "stellar-rpc-deadbeef")
	writeScript(t, bin, "#!/bin/sh\nexit 0\n")
	t.Setenv("HOME", t.TempDir())

	d := okDeps()
	d.LookPath = missing("go", "cargo", "git", "make")
	res := Run(testConfig(), t.TempDir(), bin, d)

	// Nothing is built, so no toolchain is needed — but the clone and the
	// Makefile-driven steps still are.
	if len(res.Failures) != 2 {
		t.Fatalf("failures = %q, want git and make only", res.Failures)
	}
	has(t, res.Failures, "git not found")
	has(t, res.Failures, "make not found")

	t.Run("a non-executable binary still means a build", func(t *testing.T) {
		plain := filepath.Join(t.TempDir(), "stellar-rpc-deadbeef")
		if err := os.WriteFile(plain, []byte("stale"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		res := Run(testConfig(), t.TempDir(), plain, d)
		has(t, res.Failures, "go not found")
		has(t, res.Failures, "cargo not found")
	})
}

func TestCloudToolChecks(t *testing.T) {
	packsGS := func(c *config.Config) {
		c.Datasets = []config.Dataset{{Name: "packs", Kind: config.KindPacksGS, Location: "gs://bucket/cold", Chunks: []int{1}}}
	}
	bsbS3 := func(c *config.Config) {
		c.Datasets = []config.Dataset{{Name: "bsb", Kind: config.KindBSBS3, Location: "s3://bucket/ledgers", Chunks: []int{1}}}
	}
	publish := func(uri string) func(*config.Config) {
		return func(c *config.Config) { c.PublishURI = uri }
	}

	cases := []struct {
		name    string
		mods    []func(*config.Config)
		absent  []string
		want    []string
		noFails bool
	}{
		{
			name:   "packs-gs dataset needs gcloud",
			mods:   []func(*config.Config){packsGS},
			absent: []string{"gcloud"},
			want:   []string{"gcloud not found in PATH", "dataset 'packs' fetches its packs from 'gs://bucket/cold'"},
		},
		{
			name:   "gs:// publish_uri needs gcloud",
			mods:   []func(*config.Config){publish("gs://bucket/results")},
			absent: []string{"gcloud"},
			want:   []string{"gcloud not found in PATH", "publish_uri 'gs://bucket/results'"},
		},
		{
			name:    "no gs:// anywhere needs no gcloud",
			absent:  []string{"gcloud"},
			noFails: true,
		},
		{
			name:   "s3:// publish_uri needs aws",
			mods:   []func(*config.Config){publish("s3://bucket/results")},
			absent: []string{"aws"},
			want:   []string{"aws not found in PATH", "publish_uri 's3://bucket/results'"},
		},
		{
			// The bench binary's SDK reads the public bucket itself.
			name:    "bsb-s3 dataset needs no aws CLI",
			mods:    []func(*config.Config){bsbS3},
			absent:  []string{"aws", "gcloud"},
			noFails: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := okDeps()
			d.LookPath = missing(tc.absent...)
			res := Run(testConfig(tc.mods...), t.TempDir(), "", d)
			if tc.noFails {
				if len(res.Failures) != 0 {
					t.Fatalf("failures = %q, want none", res.Failures)
				}
				return
			}
			if len(res.Failures) != 1 {
				t.Fatalf("failures = %q, want exactly one", res.Failures)
			}
			for _, want := range tc.want {
				has(t, res.Failures, want)
			}
		})
	}
}

func TestPublishRootListable(t *testing.T) {
	t.Run("a listing error fails, naming publish_uri", func(t *testing.T) {
		d := okDeps()
		d.ListRoot = func(string) error { return os.ErrPermission }
		cfg := testConfig(func(c *config.Config) { c.PublishURI = "gs://bucket/results" })
		res := Run(cfg, t.TempDir(), "", d)
		if len(res.Failures) != 1 {
			t.Fatalf("failures = %q, want exactly one", res.Failures)
		}
		has(t, res.Failures, "cannot list publish_uri 'gs://bucket/results': permission denied")
		has(t, res.Failures, "hours from now")
	})

	t.Run("no publish_uri lists nothing", func(t *testing.T) {
		called := false
		d := okDeps()
		d.ListRoot = func(string) error { called = true; return nil }
		res := Run(testConfig(), t.TempDir(), "", d)
		if called {
			t.Error("ListRoot was called for a config that publishes nowhere")
		}
		if len(res.Failures) != 0 {
			t.Errorf("failures = %q, want none", res.Failures)
		}
	})

	t.Run("a missing CLI is reported once, not twice", func(t *testing.T) {
		called := false
		d := okDeps()
		d.LookPath = missing("gcloud")
		d.ListRoot = func(string) error { called = true; return os.ErrPermission }
		cfg := testConfig(func(c *config.Config) { c.PublishURI = "gs://bucket/results" })
		res := Run(cfg, t.TempDir(), "", d)
		if called {
			t.Error("ListRoot was called although gcloud is not installed")
		}
		if len(res.Failures) != 1 {
			t.Fatalf("failures = %q, want the missing gcloud only", res.Failures)
		}
		has(t, res.Failures, "gcloud not found in PATH")
	})
}

func TestMountCheck(t *testing.T) {
	t.Run("the default bench root must be mounted", func(t *testing.T) {
		d := okDeps()
		d.Mountpoint = func(string) error { return os.ErrNotExist }
		res := Run(testConfig(), defaultBenchRoot, "", d)
		if len(res.Failures) != 1 {
			t.Fatalf("failures = %q, want exactly one", res.Failures)
		}
		has(t, res.Failures, "/mnt/nvme not mounted — run bootstrap.sh first, or set BENCH_ROOT")
	})

	t.Run("a machine without mountpoint is not checked", func(t *testing.T) {
		called := false
		d := okDeps()
		d.LookPath = missing("mountpoint")
		d.Mountpoint = func(string) error { called = true; return os.ErrNotExist }
		res := Run(testConfig(), defaultBenchRoot, "", d)
		if called {
			t.Error("Mountpoint was called although no mountpoint command exists")
		}
		if len(res.Failures) != 0 {
			t.Errorf("failures = %q, want none", res.Failures)
		}
	})

	t.Run("a custom bench root is the operator's business", func(t *testing.T) {
		called := false
		d := okDeps()
		d.Mountpoint = func(string) error { called = true; return os.ErrNotExist }
		res := Run(testConfig(), t.TempDir(), "", d)
		if called {
			t.Error("Mountpoint was called for a non-default BENCH_ROOT")
		}
		if len(res.Failures) != 0 {
			t.Errorf("failures = %q, want none", res.Failures)
		}
	})
}

func TestDiskCheck(t *testing.T) {
	cases := []struct {
		name string
		free uint64
		err  error
		want string // substring of the expected warning, "" for no warning
	}{
		{name: "tight disk warns", free: 50 * gib, want: "only 50 GiB free under "},
		{name: "roomy disk is quiet", free: 200 * gib},
		{name: "an unmeasurable filesystem warns", err: os.ErrNotExist, want: "could not measure free disk under "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			d := okDeps()
			d.DiskFree = func(string) (uint64, error) { return tc.free, tc.err }
			res := Run(testConfig(), root, "", d)
			if len(res.Failures) != 0 {
				t.Errorf("failures = %q, want none: disk space is only ever a warning", res.Failures)
			}
			if tc.want == "" {
				if len(res.Warnings) != 0 {
					t.Fatalf("warnings = %q, want none", res.Warnings)
				}
				return
			}
			if len(res.Warnings) != 1 {
				t.Fatalf("warnings = %q, want exactly one", res.Warnings)
			}
			has(t, res.Warnings, tc.want+root)
		})
	}
}

// The whole check set through the real exec.LookPath, with PATH stubbed to a
// directory of fake tools.
func TestRunWithRealLookPath(t *testing.T) {
	d := Deps{
		ListRoot:   func(string) error { return nil },
		Mountpoint: func(string) error { return nil },
		DiskFree:   func(string) (uint64, error) { return 500 * gib, nil },
	}

	t.Run("a bare PATH is missing the toolchain", func(t *testing.T) {
		stubPATH(t, "git", "make")
		t.Setenv("HOME", t.TempDir())
		res := Run(testConfig(), t.TempDir(), "", d)
		if len(res.Failures) != 2 {
			t.Fatalf("failures = %q, want go and cargo", res.Failures)
		}
		has(t, res.Failures, "go not found")
		has(t, res.Failures, "cargo not found")
	})

	t.Run("a fully equipped PATH passes", func(t *testing.T) {
		stubPATH(t, "git", "make", "go", "cargo", "gcloud", "aws")
		t.Setenv("HOME", t.TempDir())
		cfg := testConfig(func(c *config.Config) { c.PublishURI = "gs://bucket/results" })
		res := Run(cfg, t.TempDir(), "", d)
		if len(res.Failures) != 0 {
			t.Fatalf("failures = %q, want none", res.Failures)
		}
		if len(res.Warnings) != 0 {
			t.Fatalf("warnings = %q, want none", res.Warnings)
		}
	})
}

// The default ListRoot, exercised against fake gcloud and aws binaries on
// PATH: it is the empty-vs-error distinction that matters, not the real CLIs.
func TestListRoot(t *testing.T) {
	cases := []struct {
		name    string
		tool    string
		script  string
		uri     string
		wantErr string // "" means the root must count as listable
	}{
		{
			name:   "gcloud lists a populated root",
			tool:   "gcloud",
			script: "#!/bin/sh\necho gs://bucket/results/run-1/\n",
			uri:    "gs://bucket/results",
		},
		{
			name:   "gcloud on an empty root",
			tool:   "gcloud",
			script: "#!/bin/sh\necho 'ERROR: One or more URLs matched no objects.' >&2\nexit 1\n",
			uri:    "gs://bucket/results",
		},
		{
			name:    "gcloud without credentials",
			tool:    "gcloud",
			script:  "#!/bin/sh\necho 'ERROR: HTTPError 403: AccessDenied' >&2\nexit 1\n",
			uri:     "gs://bucket/results",
			wantErr: "AccessDenied",
		},
		{
			// Silence is aws's empty signature, not gcloud's: gcloud always
			// says so, and treating its silence as empty would pass a
			// credential check that never ran.
			name:    "gcloud failing silently is a real failure",
			tool:    "gcloud",
			script:  "#!/bin/sh\nexit 1\n",
			uri:     "gs://bucket/results",
			wantErr: "no output",
		},
		{
			name:   "aws on an empty prefix says nothing at all",
			tool:   "aws",
			script: "#!/bin/sh\nexit 1\n",
			uri:    "s3://bucket/results",
		},
		{
			// Anything printed anywhere means the exit was not emptiness.
			name:    "aws failing on stdout only is a real failure",
			tool:    "aws",
			script:  "#!/bin/sh\necho 'An error occurred (AccessDenied)'\nexit 1\n",
			uri:     "s3://bucket/results",
			wantErr: "An error occurred (AccessDenied)",
		},
		{
			name:    "aws without credentials",
			tool:    "aws",
			script:  "#!/bin/sh\necho 'fatal error: Unable to locate credentials' >&2\nexit 1\n",
			uri:     "s3://bucket/results",
			wantErr: "Unable to locate credentials",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeScript(t, filepath.Join(dir, tc.tool), tc.script)
			t.Setenv("PATH", dir)
			err := ListRoot(tc.uri)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("ListRoot(%s) = %v, want nil", tc.uri, err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("ListRoot(%s) = nil, want an error mentioning %q", tc.uri, tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Fatalf("ListRoot(%s) = %v, want it to mention %q", tc.uri, err, tc.wantErr)
			}
		})
	}

	t.Run("an unsupported scheme is an error, not a listing", func(t *testing.T) {
		if err := ListRoot("https://example.com/results"); err == nil {
			t.Fatal("ListRoot(https://…) = nil, want an unsupported-scheme error")
		}
	})
}

func TestDiskFreeMeasuresTheNearestExistingParent(t *testing.T) {
	root := t.TempDir()
	free, err := diskFree(filepath.Join(root, "not", "created", "yet"))
	if err != nil {
		t.Fatalf("diskFree = error %v, want the parent's filesystem", err)
	}
	if free == 0 {
		t.Error("diskFree = 0 bytes, want the temp filesystem's free space")
	}
}
