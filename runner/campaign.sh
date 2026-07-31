#!/usr/bin/env bash
#
# Config-driven benchmark campaign runner for stellar-rpc's full-history bench
# subcommands. It treats stellar-rpc as a black box: it reads a campaign
# config, validates it, maintains a persistent build clone of $REPO at
# $BENCH_ROOT/src, builds the requested ref into a versioned binary, prepares
# each dataset's cold pack tree, and runs the configured ingest and query
# loops. Every benchmark invocation is a fresh process with its own --out
# directory.
#
# Usage:
#   ./runner/campaign.sh <path/to/campaign.cfg> [--dry-run] [--resume <results-dir>]
#
# --dry-run prints every command the campaign would execute, with resolved
# paths and flags. It performs no builds, downloads, or benchmark runs.
#
# --resume continues an interrupted campaign into an existing results
# directory instead of starting a new one. The run id (the directory's
# basename) is reused, and every timed leg whose --out directory already holds
# a finished benchmark is skipped; a leg that was mid-flight when the campaign
# died is wiped and re-run. The directory must belong to this config's NAME and
# to the commit REF resolves to right now — resuming onto a different commit
# would mix binaries inside one bundle, so it is refused. --dry-run --resume
# prints the plan a resume would follow against the real directory. Resume is
# same-boot only: $BENCH_ROOT is instance-store scratch, so a stopped instance
# takes the results directory (and the hot DBs) with it.
#
# Environment:
#   BENCH_ROOT  storage root for the build clone, datasets, scratch space, and
#          results (default /mnt/nvme/bench, the benchmark machine's NVMe; on
#          other machines set it to a writable path, e.g. BENCH_ROOT=/tmp/bench)
#
# Results land in $BENCH_ROOT/results/<NAME>-<sha>-<stamp>/ together with the
# campaign config, the benchmarked binary's identity (binary.txt),
# machine-metadata.txt, the runner's own console log (campaign.log), and
# metadata.json — written as soon as the directory exists and rewritten with
# finished_at at the end, so a campaign that is killed still leaves a
# parseable bundle. The results directory is bundled to
# /tmp/bench-results-<NAME>-<sha>-<stamp>.tgz (the EBS root on the benchmark
# machine, so the bundle survives an instance stop). When PUBLISH_URI is set
# the bundle is also uploaded to <PUBLISH_URI>/<NAME>-<sha>-<stamp>/ by
# publish.sh.
#
# Config keys (the config is a bash fragment that is checked before it is
# sourced: only comments and assignments to these keys are accepted):
#   NAME             campaign name (required; charset [A-Za-z0-9._-])
#   REPO             where stellar-rpc comes from: a git URL or an absolute
#                    local path (default https://github.com/stellar/stellar-rpc.git).
#                    The persistent build clone at $BENCH_ROOT/src is
#                    cloned/fetched from it each campaign; $REPO itself is
#                    never modified. To benchmark local work-in-progress,
#                    point REPO at a local stellar-rpc checkout — only
#                    committed state is benchmarkable.
#   REF              git ref to benchmark, resolved inside $BENCH_ROOT/src
#                    after fetching $REPO's branches and tags (default
#                    feature/full-history). Built into
#                    $BENCH_ROOT/bin/stellar-rpc-<sha>.
#   INGEST           cold | hot | both | none (required)
#   QUERY            yes | no (required). Query-cold runs against each
#                    dataset's frozen pack root. Query-hot needs the hot DB a
#                    hot ingest leaves behind, so it only runs when INGEST is
#                    hot or both.
#   CLOSE_INTERVAL   bench-ingest hot --close-interval (default 0 = unpaced
#                    catch-up; e.g. 2s, 1s, 600ms for phase pacing)
#   RUNS             repetitions per (dataset, chunk) cell (default 5)
#   QC               query concurrency sweep list (default 1,4,16)
#   COLD_ITERS       bench-query cold --iters (default 100)
#   HOT_ITERS        bench-query hot --iters (default 200)
#   WORKERS          bench-ingest cold --workers (default 1)
#   HOT_NUM_LEDGERS  bench-ingest hot --num-ledgers (default 0 = whole range)
#   PUBLISH_URI      object-storage root to publish the finished bundle to
#                    (default empty = no publish). Must be gs:// or s3://; the
#                    bundle lands at <PUBLISH_URI>/<NAME>-<sha>-<stamp>/.
#   DATASETS         bash array of "name|kind|location|chunks" entries.
#                    kind=packs-local: location is a local cold pack root
#                      (the directory that contains ledgers/, events/,
#                      txhash/).
#                    kind=packs-gs: location is a gs:// prefix of the same
#                      tree; fetched once into $BENCH_ROOT/golden/<name>/.
#                    kind=bsb-s3: location is an S3 bucket path; an untimed
#                      cold backfill materializes $BENCH_ROOT/golden/<name>/.
#                    kind=fixture: location is the per-chunk ledger count for
#                      bench-ingest fixture (0 = whole chunk; a partial chunk
#                      cannot be frozen, so the count must be 0 or >= 10000).
#                      A generated fixture pack plus an untimed cold ingest
#                      materialize $BENCH_ROOT/golden/<name>/.
#                    chunks is a space-separated chunk-ID list.
#
# To force a re-fetch of a golden dataset: rm -rf $BENCH_ROOT/golden/<name>.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BENCH_ROOT="${BENCH_ROOT:-/mnt/nvme/bench}"

die() { echo "error: $*" >&2; exit 1; }
note() { echo "== [$(date -u +%H:%M:%S)] $*"; }

# run CMD...: print the command, then execute it (skipped under --dry-run).
run() {
  printf '  $ %s\n' "$*"
  if [ "$DRY" -eq 0 ]; then
    "$@"
  fi
}

# --- arguments -----------------------------------------------------------------
[ $# -ge 1 ] || die "usage: campaign.sh <path/to/campaign.cfg> [--dry-run] [--resume <results-dir>]"
CFG_ARG=$1
shift
DRY=0
RESUME_DIR=
SESSION=start
while [ $# -gt 0 ]; do
  case "$1" in
    --dry-run) DRY=1 ;;
    --resume)
      [ $# -ge 2 ] || die "--resume needs a results directory"
      shift
      [ -d "$1" ] || die "--resume: results directory not found: $1"
      RESUME_DIR=$(cd "$1" && pwd)
      SESSION=resume
      ;;
    *) die "unknown argument: $1" ;;
  esac
  shift
done
[ -f "$CFG_ARG" ] || die "config not found: $CFG_ARG"
CFG="$(cd "$(dirname "$CFG_ARG")" && pwd)/$(basename "$CFG_ARG")"

if [ "$BENCH_ROOT" = /mnt/nvme/bench ] && command -v mountpoint >/dev/null 2>&1; then
  mountpoint -q /mnt/nvme || die "/mnt/nvme not mounted — run bootstrap.sh first, or set BENCH_ROOT"
fi

# --- config: defaults, source, key validation -----------------------------------
NAME=
REPO=https://github.com/stellar/stellar-rpc.git
REF=feature/full-history
INGEST=
QUERY=
CLOSE_INTERVAL=0
RUNS=5
QC=1,4,16
COLD_ITERS=100
HOT_ITERS=200
WORKERS=1
HOT_NUM_LEDGERS=0
PUBLISH_URI=
DATASETS=()

CFG_KEYS='NAME|REPO|REF|INGEST|QUERY|CLOSE_INTERVAL|RUNS|QC|COLD_ITERS|HOT_ITERS|WORKERS|HOT_NUM_LEDGERS|PUBLISH_URI|DATASETS'
# The config is sourced, so an unexpected assignment would silently overwrite
# one of this script's own variables (BENCH_ROOT, SRC, BIN, ...). Check the
# file's text before sourcing it: blank lines, comments, and assignments to
# the documented keys only (plus the continuation lines of the DATASETS array).
_re_cfg_key="^($CFG_KEYS)="
_in_array=0
_lineno=0
while IFS= read -r _line || [ -n "$_line" ]; do
  _lineno=$((_lineno + 1))
  _stripped=${_line#"${_line%%[![:space:]]*}"}
  if [ "$_in_array" -eq 1 ]; then
    case "$_stripped" in *')'*) _in_array=0 ;; esac
    continue
  fi
  case "$_stripped" in '' | '#'*) continue ;; esac
  [[ $_stripped =~ $_re_cfg_key ]] ||
    die "config: line $_lineno is not a comment or an assignment to a documented key: '$_line' (allowed keys: ${CFG_KEYS//|/ })"
  _value=${_stripped#*=}
  if [ "${_value:0:1}" = '(' ] && [[ $_value != *')'* ]]; then
    _in_array=1
  fi
done <"$CFG"
[ "$_in_array" -eq 0 ] || die "config: unterminated array assignment (missing ')')"
# shellcheck disable=SC1090
source "$CFG"

re_name='^[A-Za-z0-9._-]+$'
re_int='^[0-9]+$'
re_qc='^[0-9]+(,[0-9]+)*$'
re_chunks='^[0-9]+( [0-9]+)*$'
re_dur='^(0|([0-9]+(\.[0-9]+)?(ns|us|ms|s|m|h))+)$'

[ -n "$NAME" ] || die "config: NAME is required"
[[ $NAME =~ $re_name ]] || die "config: NAME must match [A-Za-z0-9._-]+ (got '$NAME')"
[ -n "$REPO" ] || die "config: REPO must not be empty"
if [[ $REPO != *://* && $REPO != *@*:* ]]; then
  # Not a URL: must be an absolute path to a local git repository. Relative
  # paths are refused — they would silently depend on the invocation cwd.
  [[ $REPO == /* ]] || die "config: REPO must be a git URL or an absolute local path (got '$REPO')"
  git -C "$REPO" rev-parse --git-dir >/dev/null 2>&1 || die "config: REPO path '$REPO' is not a git repository"
fi
[ -n "$REF" ] || die "config: REF must not be empty"
case "$INGEST" in
  cold | hot | both | none) ;;
  *) die "config: INGEST must be cold|hot|both|none (got '${INGEST:-<unset>}')" ;;
esac
case "$QUERY" in
  yes | no) ;;
  *) die "config: QUERY must be yes|no (got '${QUERY:-<unset>}')" ;;
esac
[[ $CLOSE_INTERVAL =~ $re_dur ]] || die "config: CLOSE_INTERVAL must be a Go duration or 0 (got '$CLOSE_INTERVAL')"
for k in RUNS COLD_ITERS HOT_ITERS WORKERS; do
  { [[ ${!k} =~ $re_int ]] && [ "${!k}" -ge 1 ]; } || die "config: $k must be an integer >= 1 (got '${!k}')"
done
[[ $HOT_NUM_LEDGERS =~ $re_int ]] || die "config: HOT_NUM_LEDGERS must be an integer >= 0 (got '$HOT_NUM_LEDGERS')"
[[ $QC =~ $re_qc ]] || die "config: QC must be a comma-separated integer list (got '$QC')"
[ -z "$PUBLISH_URI" ] || [[ $PUBLISH_URI =~ ^(gs|s3):// ]] || die "config: PUBLISH_URI must be a gs:// or s3:// URI (got '$PUBLISH_URI')"
[ "${#DATASETS[@]}" -ge 1 ] || die "config: DATASETS must list at least one dataset"

# Parse "name|kind|location|chunks" entries into parallel arrays.
DS_NAME=()
DS_KIND=()
DS_LOC=()
DS_CHUNKS=()
DS_ROOT=()
for entry in "${DATASETS[@]}"; do
  IFS='|' read -r d_name d_kind d_loc d_chunks d_extra <<<"$entry"
  [ -z "${d_extra:-}" ] || die "config: dataset entry has more than 4 fields: '$entry'"
  [ -n "${d_chunks:-}" ] || die "config: dataset entry needs 4 pipe-separated fields (name|kind|location|chunks): '$entry'"
  [[ $d_name =~ $re_name ]] || die "config: dataset name must match [A-Za-z0-9._-]+ (got '$d_name')"
  case " ${DS_NAME[*]:-} " in
    *" $d_name "*) die "config: duplicate dataset name '$d_name'" ;;
  esac
  [[ $d_chunks =~ $re_chunks ]] || die "config: dataset '$d_name': chunks must be a space-separated chunk-ID list (got '$d_chunks')"
  case "$d_kind" in
    packs-local)
      d_root=$d_loc
      ;;
    packs-gs)
      [[ $d_loc == gs://* ]] || die "config: dataset '$d_name': packs-gs location must start with gs:// (got '$d_loc')"
      d_root=$BENCH_ROOT/golden/$d_name
      ;;
    bsb-s3)
      [ -n "$d_loc" ] || die "config: dataset '$d_name': bsb-s3 location must be an S3 bucket path"
      d_root=$BENCH_ROOT/golden/$d_name
      ;;
    fixture)
      [[ $d_loc =~ $re_int ]] || die "config: dataset '$d_name': fixture location must be the per-chunk ledger count (got '$d_loc')"
      [ "$d_loc" -eq 0 ] || [ "$d_loc" -ge 10000 ] || die "config: dataset '$d_name': fixture ledger count must be 0 or >= 10000 — the cold freeze streams the whole 10,000-ledger chunk (got '$d_loc')"
      d_root=$BENCH_ROOT/golden/$d_name
      ;;
    *)
      die "config: dataset '$d_name': kind must be packs-local|packs-gs|bsb-s3|fixture (got '$d_kind')"
      ;;
  esac
  DS_NAME+=("$d_name")
  DS_KIND+=("$d_kind")
  DS_LOC+=("$d_loc")
  DS_CHUNKS+=("$d_chunks")
  DS_ROOT+=("$d_root")
done

QUERY_COLD=0
QUERY_HOT=0
if [ "$QUERY" = yes ]; then
  QUERY_COLD=1
  case "$INGEST" in
    hot | both) QUERY_HOT=1 ;;
    *) note "QUERY=yes with INGEST=$INGEST leaves no hot DB — running the cold query suite only" ;;
  esac
fi
if [ "$INGEST" = none ] && [ "$QUERY" = no ]; then
  note "INGEST=none and QUERY=no — this campaign only prepares datasets"
fi

# --- source clone & binary under test --------------------------------------------
SRC=$BENCH_ROOT/src

# ensure_src converges the persistent build clone at $SRC onto $REPO: clone
# once, then per campaign point origin at $REPO (it may have changed since the
# clone was made), fetch its branches and tags, and hard-reset. Gitignored
# build caches (cargo target/, Go cache) survive the reset — clean -fd,
# deliberately no -x — so rebuilding a nearby commit is incremental. $REPO
# itself is never modified.
ensure_src() {
  if [ ! -d "$SRC/.git" ]; then
    run git clone "$REPO" "$SRC"
  fi
  run git -C "$SRC" remote set-url origin "$REPO"
  run git -C "$SRC" fetch -q --prune origin '+refs/heads/*:refs/remotes/origin/*' '+refs/tags/*:refs/tags/*'
  run git -C "$SRC" reset -q --hard
  run git -C "$SRC" clean -qfd
}

# resolve_ref prints the commit REF resolves to inside $SRC (which may not
# exist yet under --dry-run). Remote-tracking branches are tried first so a
# stale local ref never shadows the fetched branch tip; the fallback covers
# tags and raw commit hashes.
resolve_ref() {
  [ -d "$SRC/.git" ] || return 1
  git -C "$SRC" rev-parse --verify --quiet "refs/remotes/origin/$REF^{commit}" ||
    git -C "$SRC" rev-parse --verify --quiet "$REF^{commit}"
}

build_binary() {
  if [ "$DRY" -eq 0 ] && [ -x "$BIN" ]; then
    note "binary $BIN already built — skipping build"
    return
  fi
  note "build $REF ($SHA) → $BIN"
  run git -C "$SRC" -c advice.detachedHead=false checkout -q --detach "$BUILT_COMMIT"
  run make -C "$SRC" build-libs
  # build-rpc-v2 goes through the Makefile so the binary carries the repo's
  # GOLDFLAGS (version, commit, branch, build timestamp) that
  # `stellar-rpc-v2 version` and invocation.json report. The target writes
  # ./stellar-rpc-v2 in the clone root; move it into the versioned path the
  # campaign runs.
  run make -C "$SRC" build-rpc-v2
  run mv "$SRC/stellar-rpc-v2" "$BIN"
}

# --- dataset preparation: converge every kind on a local cold pack root ---------
golden_present() { # golden_present DIR: true if DIR exists and is non-empty
  [ -d "$1" ] && [ -n "$(find "$1" -mindepth 1 -print -quit 2>/dev/null)" ]
}

prepare_dataset() { # prepare_dataset INDEX
  local name=${DS_NAME[$1]} kind=${DS_KIND[$1]} loc=${DS_LOC[$1]} root=${DS_ROOT[$1]}
  local chunks c stage
  read -r -a chunks <<<"${DS_CHUNKS[$1]}"
  case "$kind" in
    packs-local)
      note "dataset $name: local cold pack root $root"
      if [ "$DRY" -eq 0 ]; then
        [ -d "$root/ledgers" ] || die "dataset '$name': $root/ledgers not found — location must be a cold pack root"
      fi
      ;;
    packs-gs)
      if golden_present "$root"; then
        note "dataset $name: golden packs already at $root — skipping fetch"
      else
        note "dataset $name: fetch $loc"
        # golden_present was false, so $root is absent or an empty leftover:
        # clear it, or the mv below would nest the partial inside it. The
        # partial itself is kept — rsync resumes into a half-fetched tree.
        run rm -rf "$root"
        run mkdir -p "$root.partial"
        run gcloud storage rsync -r "$loc" "$root.partial"
        run mv "$root.partial" "$root"
      fi
      ;;
    bsb-s3)
      if golden_present "$root"; then
        note "dataset $name: golden packs already at $root — skipping backfill"
      else
        run rm -rf "$root" "$root.partial"
        for c in "${chunks[@]}"; do
          note "dataset $name: golden backfill of chunk $c from S3 (untimed)"
          # AWS_EC2_METADATA_DISABLED is set on this command only: without it
          # the SDK signs requests with the machine's IAM role and the public
          # bucket 403s, but exporting it globally would also hide those same
          # instance-role credentials from publish.sh's `aws s3` calls.
          run env AWS_EC2_METADATA_DISABLED=true \
            "$BIN" bench-ingest cold \
            --source=bsb --datastore-type=S3 --region=us-east-2 \
            --bucket-path="$loc" \
            --start-chunk="$c" --num-chunks=1 \
            --cold-out-dir="$root.partial" \
            --out="$RES/golden-$name-c$c"
        done
        run mv "$root.partial" "$root"
      fi
      ;;
    fixture)
      if golden_present "$root"; then
        note "dataset $name: golden packs already at $root — skipping generation"
      else
        stage=$BENCH_ROOT/fixture/$name/ledgers
        note "dataset $name: generate a fixture pack tree"
        run rm -rf "$BENCH_ROOT/fixture/$name" "$root" "$root.partial"
        for c in "${chunks[@]}"; do
          note "dataset $name: generate fixture chunk $c ($loc ledgers)"
          run "$BIN" bench-ingest fixture \
            --pack-dir="$stage" --chunk="$c" --num-ledgers="$loc" --seed=1
        done
        for c in "${chunks[@]}"; do
          note "dataset $name: freeze fixture chunk $c into golden packs (untimed)"
          run "$BIN" bench-ingest cold \
            --source=pack --pack-dir="$stage" \
            --start-chunk="$c" --num-chunks=1 \
            --cold-out-dir="$root.partial" \
            --out="$RES/golden-$name-c$c"
        done
        run mv "$root.partial" "$root"
      fi
      ;;
  esac
  if [ "$DRY" -eq 0 ]; then
    [ -d "$root/ledgers" ] || die "dataset '$name': $root/ledgers missing after preparation"
  fi
}

# --- benchmark loops: one fresh process and one fresh --out dir per run ---------

# resume_skip OUT: on a resumed campaign, true when OUT already holds a leg an
# earlier session finished successfully. The bench subcommands write
# invocation.json as the run completes, next to the driver.csv they stream
# during it — but a FAILED run also writes invocation.json, with an `error`
# field (stellar-rpc#907) — so completion means: both files present AND no
# error recorded. Anything else (mid-flight kill, recorded failure, unreadable
# manifest) is wiped so the leg re-runs into a clean --out. Outside a resume
# this is always false and no existing output is inspected.
resume_skip() {
  local err
  [ -n "$RESUME_DIR" ] && [ -d "$1" ] || return 1
  if [ -f "$1/invocation.json" ] && [ -f "$1/driver.csv" ]; then
    err=$(jq -r '.error // empty' "$1/invocation.json" 2>/dev/null || echo "unreadable invocation.json")
    if [ -z "$err" ]; then
      note "resume: $(basename "$1") already complete — skipping"
      return 0
    fi
    note "resume: $(basename "$1") failed in an earlier session ($err) — wiping and re-running"
  else
    note "resume: $(basename "$1") is a partial leg — wiping and re-running"
  fi
  run rm -rf "$1"
  return 1
}

run_ingest_cold() {
  local i c r name root chunks out
  for i in "${!DS_NAME[@]}"; do
    name=${DS_NAME[$i]} root=${DS_ROOT[$i]}
    read -r -a chunks <<<"${DS_CHUNKS[$i]}"
    for c in "${chunks[@]}"; do
      for r in $(seq 1 "$RUNS"); do
        note "ingest-cold $name chunk $c run $r/$RUNS"
        out=$RES/ingest-cold-$name-c$c-run$r
        if resume_skip "$out"; then continue; fi
        run rm -rf "$BENCH_ROOT/scratch/$name/$c"
        run "$BIN" bench-ingest cold \
          --source=pack --pack-dir="$root/ledgers" \
          --start-chunk="$c" --num-chunks=1 --workers="$WORKERS" \
          --cold-out-dir="$BENCH_ROOT/scratch/$name/$c" \
          --out="$out"
      done
    done
  done
}

# The hot DB is deleted before each run; the last run's DB is kept because
# the hot query suite reads it. On a resumed campaign that is the last rep that
# actually ran — every rep of a cell ingests the same chunk, so whichever one
# it is leaves an equivalent DB.
run_ingest_hot() {
  local i c r name root chunks cmd out
  for i in "${!DS_NAME[@]}"; do
    name=${DS_NAME[$i]} root=${DS_ROOT[$i]}
    read -r -a chunks <<<"${DS_CHUNKS[$i]}"
    for c in "${chunks[@]}"; do
      for r in $(seq 1 "$RUNS"); do
        note "ingest-hot $name chunk $c run $r/$RUNS"
        out=$RES/ingest-hot-$name-c$c-run$r
        if resume_skip "$out"; then continue; fi
        run rm -rf "$BENCH_ROOT/hot/$name/$c"
        cmd=("$BIN" bench-ingest hot
          --source=pack --pack-dir="$root/ledgers"
          --start-chunk="$c" --hot-dir="$BENCH_ROOT/hot/$name/$c"
          --close-interval="$CLOSE_INTERVAL")
        if [ "$HOT_NUM_LEDGERS" -gt 0 ]; then
          cmd+=(--num-ledgers="$HOT_NUM_LEDGERS")
        fi
        cmd+=(--out="$out")
        run "${cmd[@]}"
      done
    done
  done
}

run_query_cold() {
  local i c r name root chunks out
  for i in "${!DS_NAME[@]}"; do
    name=${DS_NAME[$i]} root=${DS_ROOT[$i]}
    read -r -a chunks <<<"${DS_CHUNKS[$i]}"
    for c in "${chunks[@]}"; do
      for r in $(seq 1 "$RUNS"); do
        note "query-cold $name chunk $c run $r/$RUNS"
        out=$RES/query-cold-$name-c$c-run$r
        if resume_skip "$out"; then continue; fi
        run "$BIN" bench-query cold \
          --cold-dir="$root" --start-chunk="$c" --num-chunks=1 \
          --types=ledgers,txpage,txhash,events \
          --query-concurrency="$QC" --iters="$COLD_ITERS" \
          --out="$out"
      done
    done
  done
}

run_query_hot() {
  local i c r name chunks cmd out hot
  for i in "${!DS_NAME[@]}"; do
    name=${DS_NAME[$i]}
    read -r -a chunks <<<"${DS_CHUNKS[$i]}"
    for c in "${chunks[@]}"; do
      hot=$BENCH_ROOT/hot/$name/$c
      for r in $(seq 1 "$RUNS"); do
        note "query-hot $name chunk $c run $r/$RUNS"
        out=$RES/query-hot-$name-c$c-run$r
        if resume_skip "$out"; then continue; fi
        # This suite reads the DB the last hot-ingest rep left behind. A resume
        # that skipped every one of those legs needs it to have survived from
        # the original session; it sits on the same instance-store scratch as
        # $RES, so in practice either both are there or neither is.
        if [ -n "$RESUME_DIR" ] && [ "$DRY" -eq 0 ] && [ ! -d "$hot" ]; then
          die "resume: hot DB $hot is gone — re-run the hot ingest for $name chunk $c (rm -rf $RES/ingest-hot-$name-c$c-run* and resume again) or start a fresh campaign"
        fi
        cmd=("$BIN" bench-query hot
          --hot-dir="$hot" --chunk="$c"
          "--types=ledgers,txpage,txhash,events"
          --query-concurrency="$QC" --iters="$HOT_ITERS" --warmup=20)
        # A capped hot ingest leaves a truncated DB; keep the query sampler
        # inside what was ingested.
        if [ "$HOT_NUM_LEDGERS" -gt 0 ]; then
          cmd+=(--sample-ledgers="$HOT_NUM_LEDGERS")
        fi
        cmd+=(--out="$out")
        run "${cmd[@]}"
      done
    done
  done
}

# --- provenance and machine metadata ---------------------------------------------
write_binary_info() {
  {
    echo "binary: $BIN"
    echo "commit: $BUILT_COMMIT"
    echo "ref:    $REF"
    echo "repo:   $REPO"
    "$BIN" version 2>&1 | head -3
  } >"$RES/binary.txt"
}

write_machine_metadata() {
  note "machine metadata"
  {
    date -u
    if TOKEN=$(curl -m 2 -sf -X PUT http://169.254.169.254/latest/api/token \
      -H 'X-aws-ec2-metadata-token-ttl-seconds: 60' 2>/dev/null); then
      echo "instance-type: $(curl -m 2 -sH "X-aws-ec2-metadata-token: $TOKEN" http://169.254.169.254/latest/meta-data/instance-type)"
      echo "instance-id:   $(curl -m 2 -sH "X-aws-ec2-metadata-token: $TOKEN" http://169.254.169.254/latest/meta-data/instance-id)"
    fi
    uname -a
    lsb_release -ds 2>/dev/null || true
    { lscpu | grep -E 'Model name|^CPU\(s\)'; } 2>/dev/null || true
    sysctl -n machdep.cpu.brand_string hw.memsize hw.ncpu 2>/dev/null || true
    { free -h | head -2; } 2>/dev/null || true
    lsblk -o NAME,SIZE,MODEL 2>/dev/null || true
    echo "repo: $REPO"
    echo "ref: $REF ($BUILT_COMMIT)"
    echo "binary: $BIN (commit $BUILT_COMMIT)"
    "$BIN" version 2>&1 | head -3
    go version 2>/dev/null || true
    { rustc --version || "$HOME/.cargo/bin/rustc" --version; } 2>/dev/null || true
    echo "campaign: $NAME · ingest: $INGEST · query: $QUERY · runs: $RUNS · concurrency: $QC"
    echo "cold-iters: $COLD_ITERS · hot-iters: $HOT_ITERS · close-interval: $CLOSE_INTERVAL · workers: $WORKERS · hot-num-ledgers: $HOT_NUM_LEDGERS"
    echo -n "fsync probe: "
    if probe=$(dd if=/dev/zero of="$BENCH_ROOT/.fsync-probe" bs=4k count=2000 oflag=dsync 2>&1); then
      echo "$probe" | tail -1
    else
      echo "unavailable (dd has no oflag=dsync on this platform)"
    fi
    rm -f "$BENCH_ROOT/.fsync-probe"
  } >"$RES/machine-metadata.txt" 2>&1
}

# write_campaign_metadata emits metadata.json, the machine-readable campaign
# manifest: run identity, campaign config, datasets, and hardware facts.
# Per-invocation detail (resolved flags, binary identity, timings) lives in
# each --out directory's invocation.json; this file records what no single
# invocation knows. Its shape is a cross-repo contract with the converter —
# see runner/README.md and SCHEMA.md § Inputs before changing it.
#
# write_campaign_metadata final writes the finished manifest; with any other
# argument (or none) finished_at is left out. The file is written twice — once
# as soon as $RES exists, so a campaign that is killed mid-flight still leaves a
# parseable bundle and a started_at for --resume to recover, and once at the end
# with finished_at.
write_campaign_metadata() {
  local i token datasets_json hardware_json
  local itype='' iid='' cpus='' mem='' finished_at='' resumed=false
  local -a chunks
  [ "${1:-}" != final ] || finished_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
  [ -z "$RESUME_DIR" ] || resumed=true
  datasets_json=$(
    for i in "${!DS_NAME[@]}"; do
      read -r -a chunks <<<"${DS_CHUNKS[$i]}"
      jq -n --arg name "${DS_NAME[$i]}" --arg kind "${DS_KIND[$i]}" \
        --arg location "${DS_LOC[$i]}" --args \
        '{name: $name, kind: $kind, location: $location, chunks: ($ARGS.positional | map(tonumber))}' \
        "${chunks[@]}"
    done | jq -s .
  )
  if token=$(curl -m 2 -sf -X PUT http://169.254.169.254/latest/api/token \
    -H 'X-aws-ec2-metadata-token-ttl-seconds: 60' 2>/dev/null); then
    itype=$(curl -m 2 -sH "X-aws-ec2-metadata-token: $token" http://169.254.169.254/latest/meta-data/instance-type 2>/dev/null || true)
    iid=$(curl -m 2 -sH "X-aws-ec2-metadata-token: $token" http://169.254.169.254/latest/meta-data/instance-id 2>/dev/null || true)
  fi
  cpus=$(nproc 2>/dev/null || sysctl -n hw.ncpu 2>/dev/null || true)
  if [ -f /proc/meminfo ]; then
    mem=$(awk '/^MemTotal:/ {print $2}' /proc/meminfo)
  fi
  # Empty fields are dropped, so an unavailable fact is absent rather than "".
  hardware_json=$(jq -n \
    --arg instance_type "$itype" --arg instance_id "$iid" \
    --arg uname "$(uname -srm)" --arg cpus "$cpus" --arg mem_total_kb "$mem" \
    '[{instance_type: $instance_type}, {instance_id: $instance_id}, {uname: $uname},
      {cpus: ($cpus | if . == "" then null else tonumber end)},
      {mem_total_kb: ($mem_total_kb | if . == "" then null else tonumber end)}]
     | add | with_entries(select(.value != null and .value != ""))')
  jq -n \
    --arg run_id "$NAME-$SHA-$STAMP" \
    --arg name "$NAME" \
    --arg config_file "$(basename "$CFG")" \
    --arg ref "$REF" \
    --arg built_commit "$BUILT_COMMIT" \
    --arg ingest "$INGEST" \
    --arg query "$QUERY" \
    --arg close_interval "$CLOSE_INTERVAL" \
    --argjson runs "$RUNS" \
    --arg query_concurrency "$QC" \
    --argjson cold_iters "$COLD_ITERS" \
    --argjson hot_iters "$HOT_ITERS" \
    --argjson workers "$WORKERS" \
    --argjson hot_num_ledgers "$HOT_NUM_LEDGERS" \
    --argjson datasets "$datasets_json" \
    --argjson hardware "$hardware_json" \
    --arg hostname "$(hostname)" \
    --arg started_at "$STARTED_AT" \
    --arg finished_at "$finished_at" \
    --argjson resumed "$resumed" \
    '{
      schema_version: 1,
      run_id: $run_id,
      campaign: {
        name: $name,
        config_file: $config_file,
        ref: $ref,
        built_commit: $built_commit,
        ingest: $ingest,
        query: $query,
        close_interval: $close_interval,
        runs: $runs,
        query_concurrency: $query_concurrency,
        cold_iters: $cold_iters,
        hot_iters: $hot_iters,
        workers: $workers,
        hot_num_ledgers: $hot_num_ledgers,
        resumed: $resumed
      },
      datasets: $datasets,
      hardware: $hardware,
      hostname: $hostname,
      started_at: $started_at,
      finished_at: $finished_at
    }
    | if $resumed then . else del(.campaign.resumed) end
    | if $finished_at == "" then del(.finished_at) else . end' >"$RES/metadata.json"
}

# --- campaign --------------------------------------------------------------------
if [ "$DRY" -eq 1 ]; then
  note "dry run: printing commands only — nothing is built, downloaded, or executed"
fi

note "source: $REPO @ $REF → $SRC"
ensure_src
if BUILT_COMMIT=$(resolve_ref); then
  SHA=$(git -C "$SRC" rev-parse --short=8 "$BUILT_COMMIT")
elif [ "$DRY" -eq 1 ]; then
  # --dry-run cloned and fetched nothing, so REF may not resolve locally yet:
  # plan with the ref itself and a placeholder sha in derived paths. The
  # placeholder must be 8 hex digits, or --dry-run --resume rejects its own
  # run ids as malformed.
  note "dry run: REF '$REF' not resolvable without the clone — using placeholder sha 'deadbeef' in paths"
  BUILT_COMMIT=$REF
  SHA=deadbeef
else
  die "REF '$REF' does not resolve to a commit in $REPO"
fi

BIN=$BENCH_ROOT/bin/stellar-rpc-$SHA
STAMP=
STARTED_AT=
if [ -n "$RESUME_DIR" ]; then
  # The bundle basename is the run id; reusing it is the whole point of a
  # resume, so it has to describe this campaign and this binary. NAME may
  # contain '-', so the sha and stamp are matched as the fixed tail.
  resume_base=$(basename "$RESUME_DIR")
  [[ $resume_base =~ ^(.+)-([0-9a-f]{8})-([0-9]{8}T[0-9]{6}Z)$ ]] ||
    die "--resume: '$resume_base' is not a <NAME>-<sha>-<stamp> results directory"
  [ "${BASH_REMATCH[1]}" = "$NAME" ] ||
    die "--resume: '$resume_base' belongs to campaign '${BASH_REMATCH[1]}', but this config's NAME is '$NAME'"
  [ "${BASH_REMATCH[2]}" = "$SHA" ] ||
    die "--resume: '$resume_base' was benchmarked with commit ${BASH_REMATCH[2]}, but REF '$REF' now resolves to $SHA — resuming would mix two binaries in one bundle; check out the same ref or start a fresh campaign"
  [ "$RESUME_DIR" = "$BENCH_ROOT/results/$resume_base" ] ||
    die "--resume: '$RESUME_DIR' is not this BENCH_ROOT's results directory (expected $BENCH_ROOT/results/$resume_base) — set BENCH_ROOT to the original campaign's root"
  STAMP=${BASH_REMATCH[3]}
  note "resume: continuing $resume_base — finished legs are skipped"
  # started_at comes from the bundle so metadata.json still spans the whole
  # campaign. Bundles written before metadata.json was written up front don't
  # have one; those record this session's start instead.
  STARTED_AT=$(jq -r '.started_at // empty' "$RESUME_DIR/metadata.json" 2>/dev/null || true)
  [ -n "$STARTED_AT" ] ||
    note "resume: no started_at in $resume_base/metadata.json — recording this session's start"
fi
[ -n "$STAMP" ] || STAMP=$(date -u +%Y%m%dT%H%M%SZ)
[ -n "$STARTED_AT" ] || STARTED_AT=$(date -u +%Y-%m-%dT%H:%M:%SZ)
RES=$BENCH_ROOT/results/$NAME-$SHA-$STAMP
TARBALL=/tmp/bench-results-$NAME-$SHA-$STAMP.tgz

note "campaign $NAME → $RES"
if [ "$DRY" -eq 0 ]; then
  mkdir -p "$BENCH_ROOT"/bin "$BENCH_ROOT"/golden "$BENCH_ROOT"/scratch "$BENCH_ROOT"/hot "$BENCH_ROOT"/fixture "$RES"
  cp "$CFG" "$RES/"
  # From here on the runner's console is also part of the bundle: on a campaign
  # that dies it is the only record of how far it got. Appended, so the
  # sessions of a resumed campaign accumulate in one file.
  exec > >(tee -a "$RES/campaign.log") 2>&1
  note "session $SESSION $(date -u +%Y-%m-%dT%H:%M:%SZ) — logging to $RES/campaign.log"
  # A manifest up front makes a killed campaign's partial bundle parseable;
  # the end-of-campaign rewrite adds finished_at.
  write_campaign_metadata
fi

build_binary
if [ "$DRY" -eq 0 ]; then
  write_binary_info
fi

for i in "${!DS_NAME[@]}"; do
  prepare_dataset "$i"
done

case "$INGEST" in cold | both) run_ingest_cold ;; esac
case "$INGEST" in hot | both) run_ingest_hot ;; esac
if [ "$QUERY_COLD" -eq 1 ]; then
  run_query_cold
fi
if [ "$QUERY_HOT" -eq 1 ]; then
  run_query_hot
fi

if [ "$DRY" -eq 1 ]; then
  if [ -n "$PUBLISH_URI" ]; then
    run "$SCRIPT_DIR/publish.sh" "$RES" "$PUBLISH_URI"
  fi
  note "dry run complete"
  exit 0
fi

write_machine_metadata
write_campaign_metadata final
tar -C "$BENCH_ROOT/results" -czf "$TARBALL" "$NAME-$SHA-$STAMP"
note "campaign done: $TARBALL"

# Publishing is a separate final step: the data is already safe in $RES and
# $TARBALL, so a publish failure is not a benchmark failure — it exits 1 with
# the exact retry command rather than corrupting the "campaign done" signal.
if [ -n "$PUBLISH_URI" ]; then
  if ! "$SCRIPT_DIR/publish.sh" "$RES" "$PUBLISH_URI"; then
    note "publish failed — data is safe in $RES and $TARBALL; retry with: publish.sh $RES $PUBLISH_URI"
    exit 1
  fi
  note "published: ${PUBLISH_URI%/}/$NAME-$SHA-$STAMP/"
fi
