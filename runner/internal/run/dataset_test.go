package run

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stellar/stellar-rpc-benchmarks/runner/internal/config"
	"github.com/stellar/stellar-rpc-benchmarks/runner/internal/plan"
)

// dsStep is a dataset step whose commands are shell scripts standing in for
// gcloud rsync and the bench binary: they write into <root>.partial exactly
// where the real tools do, which is all the choreography under test cares
// about. PreClean mirrors what plan.Build populates for the kind, since that
// list — not the executor — is what decides which directories get wiped.
func dsStep(name, kind, root string, argv ...[]string) plan.Step {
	if argv == nil {
		argv = [][]string{}
	}
	s := plan.Step{
		ID:      "dataset-" + name,
		Kind:    plan.KindDataset,
		Argv:    argv,
		Dataset: &plan.DatasetSpec{Name: name, Kind: kind, Location: "gs://bucket/cold", Root: root},
	}
	switch kind {
	case config.KindPacksGS:
		s.PreClean = []string{root}
	case config.KindBSBS3:
		s.PreClean = []string{root, root + ".partial"}
	}
	return s
}

// fixtureStep is dsStep for the one kind that also owns a staging pack tree,
// which the plan wipes by its parent along with both roots.
func fixtureStep(name, root, stage string, argv ...[]string) plan.Step {
	s := dsStep(name, config.KindFixture, root, argv...)
	s.Dataset.Stage = stage
	s.PreClean = []string{filepath.Dir(stage), root, root + ".partial"}
	return s
}

// materialize is a command that fills dir with a pack tree, the way a fetch, a
// backfill, or a freeze would.
func materialize(dir string) []string {
	return []string{"/bin/sh", "-c", `mkdir -p "$1/ledgers" && : > "$1/ledgers/chunk-1.pack"`, "sh", dir}
}

// marker is a command that records that it ran. Preparations that should have
// been short-circuited are proven by its absence.
func marker(path string) []string {
	return []string{"/bin/sh", "-c", `: > "$1"`, "sh", path}
}

func prepare(t *testing.T, step plan.Step) outcome {
	t.Helper()
	return walk(t, &plan.Plan{Steps: []plan.Step{step}}, Options{})
}

func TestPrepareDatasetPacksLocal(t *testing.T) {
	t.Run("a root holding packs is accepted as it is", func(t *testing.T) {
		root := t.TempDir()
		mustWrite(t, filepath.Join(root, "ledgers", "chunk-1.pack"), "packs")
		got := prepare(t, dsStep("local", config.KindPacksLocal, root))
		got.assertStatuses(t, StatusOK)
		got.assertLogHas(t, "dataset local: local cold pack root "+root)
	})

	t.Run("a root without ledgers/ is refused in the operator's own words", func(t *testing.T) {
		root := t.TempDir()
		got := prepare(t, dsStep("local", config.KindPacksLocal, root))
		got.assertStatuses(t, StatusFailed)
		want := "dataset 'local': " + root + "/ledgers not found — location must be a cold pack root"
		if err := got.results[0].Err; err == nil || err.Error() != want {
			t.Errorf("error = %v, want %q", err, want)
		}
	})
}

func TestPrepareDatasetPacksGS(t *testing.T) {
	t.Run("an empty leftover root is cleared and the partial renamed onto it", func(t *testing.T) {
		tmp := t.TempDir()
		root := filepath.Join(tmp, "golden", "pubnet")
		mustMkdir(t, root) // the leftover of an earlier, cleared-out fetch
		// A half-fetched tree from a killed session: the partial is deliberately
		// kept, because rsync resumes into it.
		mustWrite(t, root+".partial/half.pack", "resumable")

		got := prepare(t, dsStep("pubnet", config.KindPacksGS, root, materialize(root+".partial")))
		got.assertStatuses(t, StatusOK)
		got.assertLogHas(t, "dataset pubnet: fetch gs://bucket/cold")
		got.assertLogHas(t, "  $ rm -rf "+root+"\n")
		got.assertLogHas(t, "  $ mkdir -p "+root+".partial")
		got.assertLogHas(t, "  $ mv "+root+".partial "+root)

		assertExists(t, filepath.Join(root, "ledgers", "chunk-1.pack"))
		assertExists(t, filepath.Join(root, "half.pack")) // the resumed bytes
		assertGone(t, root+".partial")
		assertGone(t, filepath.Join(root, "pubnet.partial")) // never nested
	})

	t.Run("golden packs already there short-circuit the fetch", func(t *testing.T) {
		tmp := t.TempDir()
		root := filepath.Join(tmp, "golden", "pubnet")
		mustWrite(t, filepath.Join(root, "ledgers", "chunk-1.pack"), "packs")
		ran := filepath.Join(tmp, "ran.txt")

		got := prepare(t, dsStep("pubnet", config.KindPacksGS, root, marker(ran)))
		got.assertStatuses(t, StatusOK)
		got.assertLogHas(t, "dataset pubnet: golden packs already at "+root+" — skipping fetch")
		assertGone(t, ran)
		// The short-circuit comes before the wipes: present golden packs are
		// never touched, whatever pre_clean says.
		got.assertLogLacks(t, "rm -rf")
		assertExists(t, filepath.Join(root, "ledgers", "chunk-1.pack"))
	})

	t.Run("a preparation that leaves no ledgers/ fails", func(t *testing.T) {
		tmp := t.TempDir()
		root := filepath.Join(tmp, "golden", "pubnet")
		got := prepare(t, dsStep("pubnet", config.KindPacksGS, root, marker(root+".partial/events.db")))
		got.assertStatuses(t, StatusFailed)
		want := "dataset 'pubnet': " + root + "/ledgers missing after preparation"
		if err := got.results[0].Err; err == nil || err.Error() != want {
			t.Errorf("error = %v, want %q", err, want)
		}
	})

	t.Run("a failed fetch leaves the partial behind and never renames it", func(t *testing.T) {
		tmp := t.TempDir()
		root := filepath.Join(tmp, "golden", "pubnet")
		got := prepare(t, dsStep("pubnet", config.KindPacksGS, root, []string{"/bin/sh", "-c", "exit 1"}))
		got.assertStatuses(t, StatusFailed)
		assertGone(t, root)
		assertExists(t, root+".partial")
	})
}

func TestPrepareDatasetBSBS3(t *testing.T) {
	t.Run("a stale partial is wiped before the backfill", func(t *testing.T) {
		tmp := t.TempDir()
		root := filepath.Join(tmp, "golden", "bsb")
		stale := root + ".partial/stale.pack"
		mustWrite(t, stale, "from a backfill that died")

		// The backfill only materializes when the stale bytes are gone: a
		// resumed cold backfill would double-write the pack tree.
		argv := []string{"/bin/sh", "-c", `test ! -e "$2" && mkdir -p "$1/ledgers"`, "sh", root + ".partial", stale}
		got := prepare(t, dsStep("bsb", config.KindBSBS3, root, argv))
		got.assertStatuses(t, StatusOK)
		got.assertLogHas(t, "  $ rm -rf "+root+" "+root+".partial")
		assertExists(t, filepath.Join(root, "ledgers"))
		assertGone(t, filepath.Join(root, "stale.pack"))
	})

	t.Run("golden packs already there short-circuit the backfill", func(t *testing.T) {
		tmp := t.TempDir()
		root := filepath.Join(tmp, "golden", "bsb")
		mustWrite(t, filepath.Join(root, "ledgers", "chunk-1.pack"), "packs")
		ran := filepath.Join(tmp, "ran.txt")

		got := prepare(t, dsStep("bsb", config.KindBSBS3, root, marker(ran)))
		got.assertStatuses(t, StatusOK)
		got.assertLogHas(t, "dataset bsb: golden packs already at "+root+" — skipping backfill")
		assertGone(t, ran)
	})

	t.Run("the S3 env is set on the backfill commands", func(t *testing.T) {
		tmp := t.TempDir()
		root := filepath.Join(tmp, "golden", "bsb")
		argv := []string{"/bin/sh", "-c", `test "$AWS_EC2_METADATA_DISABLED" = true && mkdir -p "$1/ledgers"`, "sh", root + ".partial"}
		step := dsStep("bsb", config.KindBSBS3, root, argv)
		step.Env = map[string]string{"AWS_EC2_METADATA_DISABLED": "true"}

		got := prepare(t, step)
		got.assertStatuses(t, StatusOK)
		got.assertLogHas(t, "  $ env AWS_EC2_METADATA_DISABLED=true /bin/sh -c")
	})
}

func TestPrepareDatasetFixture(t *testing.T) {
	t.Run("a stale staging tree is wiped before generation", func(t *testing.T) {
		tmp := t.TempDir()
		root := filepath.Join(tmp, "golden", "fix")
		stage := filepath.Join(tmp, "fixture", "fix", "ledgers")
		stale := filepath.Join(tmp, "fixture", "fix", "ledgers", "chunk-1.pack")
		mustWrite(t, stale, "half a chunk from a killed generation")

		// Generation refuses to run on top of the stale chunk; the freeze then
		// fills the partial.
		generate := []string{"/bin/sh", "-c", `test ! -e "$2" && mkdir -p "$1"`, "sh", stage, stale}
		freeze := materialize(root + ".partial")
		got := prepare(t, fixtureStep("fix", root, stage, generate, freeze))
		got.assertStatuses(t, StatusOK)
		got.assertLogHas(t, "dataset fix: generate a fixture pack tree")
		got.assertLogHas(t, "  $ rm -rf "+filepath.Join(tmp, "fixture", "fix")+" "+root+" "+root+".partial")
		assertExists(t, filepath.Join(root, "ledgers", "chunk-1.pack"))
	})

	t.Run("golden packs already there short-circuit the generation", func(t *testing.T) {
		tmp := t.TempDir()
		root := filepath.Join(tmp, "golden", "fix")
		mustWrite(t, filepath.Join(root, "ledgers", "chunk-1.pack"), "packs")
		ran := filepath.Join(tmp, "ran.txt")
		stage := filepath.Join(tmp, "fixture", "fix", "ledgers")

		staged := filepath.Join(stage, "chunk-1.pack")
		mustWrite(t, staged, "left by the generation that made these packs")

		got := prepare(t, fixtureStep("fix", root, stage, marker(ran)))
		got.assertStatuses(t, StatusOK)
		got.assertLogHas(t, "dataset fix: golden packs already at "+root+" — skipping generation")
		assertGone(t, ran)
		assertExists(t, staged) // the short-circuit precedes the staging wipe
	})
}

// TestPrepareDatasetWipesWhatThePlanSays pins where the wipe list lives: the
// executor removes the step's pre_clean and nothing else, so a plan that asks
// for the partial gets it removed even for the kind that normally resumes.
func TestPrepareDatasetWipesWhatThePlanSays(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "golden", "pubnet")
	mustWrite(t, root+".partial/half.pack", "resumable, but this plan says otherwise")

	step := dsStep("pubnet", config.KindPacksGS, root, materialize(root+".partial"))
	step.PreClean = []string{root, root + ".partial"}

	got := prepare(t, step)
	got.assertStatuses(t, StatusOK)
	got.assertLogHas(t, "  $ rm -rf "+root+" "+root+".partial")
	assertGone(t, filepath.Join(root, "half.pack"))
}

func TestPrepareDatasetRejectsAnUnknownKind(t *testing.T) {
	got := prepare(t, dsStep("odd", "packs-ftp", filepath.Join(t.TempDir(), "golden", "odd")))
	got.assertStatuses(t, StatusFailed)
	if err := got.results[0].Err; err == nil || !strings.Contains(err.Error(), `unknown kind "packs-ftp"`) {
		t.Errorf("error = %v, want it to name the unknown kind", err)
	}
}

func TestRunDatasetWithoutASpec(t *testing.T) {
	got := prepare(t, plan.Step{ID: "dataset-x", Kind: plan.KindDataset, Argv: [][]string{}})
	got.assertStatuses(t, StatusFailed)
}
