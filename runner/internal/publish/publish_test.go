package publish

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// runID is the bundle basename every test publishes, and therefore the last
// path component of every destination below.
const runID = "phase4-6f35679f-20260715T101500Z"

// cli is a fake gcloud or aws on PATH. It appends every invocation's argv to a
// file, so a test can assert not only what the publish did but what it never
// ran — a skipped listing is the whole point of --force, and a skipped sync is
// the whole point of the immutability check.
type cli struct{ record string }

// stubCLI writes an executable named tool (gcloud or aws) into a temp dir and
// points PATH at it. lsBody is the shell run for the listing subcommand — the
// verb sits at $2 for both tools (`gcloud storage ls`, `aws s3 ls`); every
// other subcommand succeeds silently, standing in for the upload.
func stubCLI(t *testing.T, tool, lsBody string) *cli {
	t.Helper()
	dir := t.TempDir()
	rec := filepath.Join(dir, "argv.log")
	script := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$*" >> %q
case "$2" in
ls)
%s
;;
*) exit 0 ;;
esac
`, rec, lsBody)
	if err := os.WriteFile(filepath.Join(dir, tool), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake %s: %v", tool, err)
	}
	t.Setenv("PATH", dir)
	return &cli{record: rec}
}

// argv is every invocation the fake CLI saw, in order, as space-joined argv.
func (c *cli) argv(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(c.record)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("read recorded argv: %v", err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

// bundle makes a results directory named runID, with a metadata.json unless
// told otherwise.
func bundle(t *testing.T, withMetadata bool) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), runID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir bundle: %v", err)
	}
	if withMetadata {
		if err := os.WriteFile(filepath.Join(dir, metadataName), []byte(`{"run_id":"x"}`), 0o644); err != nil {
			t.Fatalf("write metadata: %v", err)
		}
	}
	return dir
}

// Empty-prefix signatures, one per tool, as each CLI really writes them.
const (
	gcloudEmpty  = "echo 'ERROR: One or more URLs matched no objects.' >&2\nexit 1"
	gcloudFull   = "echo gs://bucket/results/phase4-6f35679f-20260715T101500Z/metadata.json\nexit 0"
	gcloudDenied = "echo 'ERROR: (gcloud.storage.ls) HTTPError 403: AccessDenied' >&2\necho 'Try gcloud auth login' >&2\nexit 1"
	awsEmpty     = "exit 1"
)

func TestPublishesIntoAnEmptyPrefix(t *testing.T) {
	c := stubCLI(t, "gcloud", gcloudEmpty)
	dir := bundle(t, true)
	var out bytes.Buffer

	if err := Run(dir, "gs://bucket/results", false, false, &out); err != nil {
		t.Fatalf("Run = %v, want nil\n%s", err, out.String())
	}

	dest := "gs://bucket/results/" + runID + "/"
	want := []string{
		"storage ls " + dest,
		"storage rsync -r " + dir + " " + dest,
	}
	if got := c.argv(t); !equal(got, want) {
		t.Errorf("cloud commands = %q, want %q", got, want)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if last := lines[len(lines)-1]; last != "published: "+dest {
		t.Errorf("last line = %q, want %q — scripts grep for it", last, "published: "+dest)
	}
	if strings.Contains(out.String(), "pre-manifest bundle") {
		t.Errorf("output warns about a missing metadata.json although the bundle has one:\n%s", out.String())
	}
}

func TestRefusesANonEmptyDestination(t *testing.T) {
	c := stubCLI(t, "gcloud", gcloudFull)
	dir := bundle(t, true)
	var out bytes.Buffer

	err := Run(dir, "gs://bucket/results", false, false, &out)
	if err == nil {
		t.Fatal("Run = nil, want the immutability refusal")
	}
	dest := "gs://bucket/results/" + runID + "/"
	want := "destination already has objects: " + dest + " — published runs are immutable; pass --force to overwrite"
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err, want)
	}
	if got := c.argv(t); !equal(got, []string{"storage ls " + dest}) {
		t.Errorf("cloud commands = %q, want the listing only — nothing may be uploaded over a published run", got)
	}
}

func TestAbortsOnAListingItCannotRead(t *testing.T) {
	c := stubCLI(t, "gcloud", gcloudDenied)
	dir := bundle(t, true)
	var out bytes.Buffer

	err := Run(dir, "gs://bucket/results", false, false, &out)
	if err == nil {
		t.Fatal("Run = nil, want the listing error — a credential failure must never read as an empty prefix")
	}
	for _, want := range []string{
		"cannot list destination gs://bucket/results/" + runID + "/",
		"(exit 1)",
		"HTTPError 403: AccessDenied",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to mention %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "Try gcloud auth login") {
		t.Errorf("error = %q, want the failure only, not the remediation prose", err)
	}
	if got := c.argv(t); len(got) != 1 {
		t.Errorf("cloud commands = %q, want the listing only", got)
	}
}

func TestForceSkipsTheListingAndUploads(t *testing.T) {
	// The listing would refuse this destination; --force must not run it at all.
	c := stubCLI(t, "gcloud", gcloudFull)
	dir := bundle(t, true)
	var out bytes.Buffer

	if err := Run(dir, "gs://bucket/results", false, true, &out); err != nil {
		t.Fatalf("Run = %v, want nil\n%s", err, out.String())
	}
	dest := "gs://bucket/results/" + runID + "/"
	if got := c.argv(t); !equal(got, []string{"storage rsync -r " + dir + " " + dest}) {
		t.Errorf("cloud commands = %q, want the sync only", got)
	}
}

func TestDryRunPrintsEverythingAndRunsNothing(t *testing.T) {
	c := stubCLI(t, "gcloud", gcloudEmpty)
	dir := bundle(t, true)
	var out bytes.Buffer

	if err := Run(dir, "gs://bucket/results", true, false, &out); err != nil {
		t.Fatalf("Run = %v, want nil\n%s", err, out.String())
	}
	if got := c.argv(t); got != nil {
		t.Errorf("cloud commands = %q, want none under --dry-run", got)
	}
	dest := "gs://bucket/results/" + runID + "/"
	for _, want := range []string{
		"  $ gcloud storage ls " + dest,
		"  $ gcloud storage rsync -r " + dir + " " + dest,
		"dry run complete",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output missing %q, got:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "published:") {
		t.Errorf("output claims a publish that never happened:\n%s", out.String())
	}
}

func TestS3Dispatch(t *testing.T) {
	// Silence is aws's empty-prefix signature: no stdout, no stderr, exit 1.
	c := stubCLI(t, "aws", awsEmpty)
	dir := bundle(t, true)
	var out bytes.Buffer

	if err := Run(dir, "s3://bucket/results/", false, false, &out); err != nil {
		t.Fatalf("Run = %v, want nil\n%s", err, out.String())
	}
	dest := "s3://bucket/results/" + runID + "/"
	want := []string{
		"s3 ls " + dest,
		"s3 sync " + dir + " " + dest,
	}
	if got := c.argv(t); !equal(got, want) {
		t.Errorf("cloud commands = %q, want %q", got, want)
	}
	if !strings.Contains(out.String(), "published: "+dest) {
		t.Errorf("output missing the published line, got:\n%s", out.String())
	}
}

func TestTrailingSlashesCollapse(t *testing.T) {
	c := stubCLI(t, "gcloud", gcloudEmpty)
	dir := bundle(t, true)
	var out bytes.Buffer

	// A results dir with a trailing slash still has the run id as its basename.
	if err := Run(dir+"/", "gs://bucket/results/", false, false, &out); err != nil {
		t.Fatalf("Run = %v, want nil\n%s", err, out.String())
	}
	dest := "gs://bucket/results/" + runID + "/"
	if got := c.argv(t); !equal(got, []string{"storage ls " + dest, "storage rsync -r " + dir + " " + dest}) {
		t.Errorf("cloud commands = %q, want them under %s", got, dest)
	}
}

func TestWarnsAboutAPreManifestBundle(t *testing.T) {
	stubCLI(t, "gcloud", gcloudEmpty)
	dir := bundle(t, false)
	var out bytes.Buffer

	if err := Run(dir, "gs://bucket/results", false, false, &out); err != nil {
		t.Fatalf("Run = %v, want nil\n%s", err, out.String())
	}
	want := "warning: " + filepath.Join(dir, metadataName) + " missing — pre-manifest bundle"
	if !strings.Contains(out.String(), want) {
		t.Errorf("output missing %q, got:\n%s", want, out.String())
	}
}

func TestRefusalsBeforeAnyCloudCall(t *testing.T) {
	cases := []struct {
		name     string
		dir      func(t *testing.T) string
		destRoot string
		want     string
	}{
		{
			name:     "a results dir that does not exist",
			dir:      func(t *testing.T) string { return filepath.Join(t.TempDir(), "nope") },
			destRoot: "gs://bucket/results",
			want:     "results dir not found: ",
		},
		{
			name: "a results path that is a file",
			dir: func(t *testing.T) string {
				path := filepath.Join(t.TempDir(), "bundle.tgz")
				if err := os.WriteFile(path, nil, 0o644); err != nil {
					t.Fatalf("write file: %v", err)
				}
				return path
			},
			destRoot: "gs://bucket/results",
			want:     "results dir not found: ",
		},
		{
			name:     "no destination at all",
			dir:      func(t *testing.T) string { return bundle(t, true) },
			destRoot: "",
			want:     "no destination: pass <dest-root-uri> or set PUBLISH_URI",
		},
		{
			name:     "a scheme neither CLI speaks",
			dir:      func(t *testing.T) string { return bundle(t, true) },
			destRoot: "https://example.com/results",
			want:     "unsupported destination scheme: https://example.com/results/" + runID + "/ (supported: gs://, s3://)",
		},
		{
			name:     "a bare path is not object storage",
			dir:      func(t *testing.T) string { return bundle(t, true) },
			destRoot: "/mnt/backup",
			want:     "unsupported destination scheme: /mnt/backup/" + runID + "/ (supported: gs://, s3://)",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := stubCLI(t, "gcloud", gcloudEmpty)
			var out bytes.Buffer
			err := Run(tc.dir(t), tc.destRoot, false, false, &out)
			if err == nil {
				t.Fatalf("Run = nil, want an error mentioning %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
			if got := c.argv(t); got != nil {
				t.Errorf("cloud commands = %q, want none", got)
			}
		})
	}
}

func TestListStates(t *testing.T) {
	cases := []struct {
		name    string
		tool    string
		body    string
		uri     string
		want    State
		wantErr string
	}{
		{name: "gcloud empty prefix", tool: "gcloud", body: gcloudEmpty, uri: "gs://bucket/results", want: Empty},
		{name: "gcloud populated prefix", tool: "gcloud", body: gcloudFull, uri: "gs://bucket/results", want: HasObjects},
		{name: "gcloud denied", tool: "gcloud", body: gcloudDenied, uri: "gs://bucket/results", wantErr: "AccessDenied"},
		{
			// Silence is aws's empty signature, not gcloud's: gcloud always
			// says so, and reading its silence as empty would skip the
			// immutability check on a listing that never worked.
			name: "gcloud silent failure", tool: "gcloud", body: "exit 1",
			uri: "gs://bucket/results", wantErr: "no output",
		},
		{name: "aws empty prefix says nothing at all", tool: "aws", body: awsEmpty, uri: "s3://bucket/results", want: Empty},
		{
			name: "aws complaining on stdout only", tool: "aws", body: "echo 'An error occurred (AccessDenied)'\nexit 1",
			uri: "s3://bucket/results", wantErr: "An error occurred (AccessDenied)",
		},
		{name: "an unsupported scheme", tool: "gcloud", body: gcloudEmpty, uri: "https://example.com", wantErr: "unsupported scheme"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stubCLI(t, tc.tool, tc.body)
			got, err := List(tc.uri)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("List(%s) = %v, nil; want an error mentioning %q", tc.uri, got, tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("List(%s) = error %v, want it to mention %q", tc.uri, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("List(%s) = error %v, want %v", tc.uri, err, tc.want)
			}
			if got != tc.want {
				t.Errorf("List(%s) = %v, want %v", tc.uri, got, tc.want)
			}
		})
	}
}

func TestDiagnosisKeepsTheErrorNotTheRemediation(t *testing.T) {
	stderr := "ERROR: (gcloud.storage.ls) There was a problem refreshing your current auth tokens\nPlease run:\n\n  $ gcloud auth login\n"
	if got, want := diagnosis(stderr), "ERROR: (gcloud.storage.ls) There was a problem refreshing your current auth tokens"; got != want {
		t.Errorf("diagnosis = %q, want %q", got, want)
	}
}

func TestUploadFailurePropagates(t *testing.T) {
	// An empty prefix, then a sync that fails: the publish must not claim it.
	dir := t.TempDir()
	script := "#!/bin/sh\ncase \"$2\" in\nls) echo 'ERROR: One or more URLs matched no objects.' >&2; exit 1 ;;\n*) echo 'ERROR: upload interrupted' >&2; exit 3 ;;\nesac\n"
	if err := os.WriteFile(filepath.Join(dir, "gcloud"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake gcloud: %v", err)
	}
	t.Setenv("PATH", dir)

	var out bytes.Buffer
	err := Run(bundle(t, true), "gs://bucket/results", false, false, &out)
	if err == nil {
		t.Fatal("Run = nil, want the sync's failure")
	}
	if !strings.Contains(err.Error(), "gcloud storage rsync") {
		t.Errorf("error = %q, want it to name the command that failed", err)
	}
	if strings.Contains(out.String(), "published:") {
		t.Errorf("output claims a publish that failed:\n%s", out.String())
	}
}

func equal(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
