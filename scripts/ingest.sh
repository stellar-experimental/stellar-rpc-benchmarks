#!/usr/bin/env bash
#
# ingest.sh — consumer half of the benchmark publish pipeline.
#
# Fetch a campaign result bundle, convert it into a run JSON with
# converter/convert.py, and stage the site update as a PR branch.
# This is the counterpart to the producer that uploads bundles to a bucket.
#
# USAGE
#   scripts/ingest.sh <bundle> --dataset-kind <pubnet|synthetic> \
#                     [--dry-run|--local] [--force] [-- <extra convert.py args>]
#
#   <bundle> is auto-detected and may be:
#     - a gs://.../<run_id>/ or s3://.../<run_id>/ URI (fetched into a temp dir),
#     - a local bundle directory (basename is the run_id), or
#     - a local bench-results-<run_id>.tgz tarball (tar of the <run_id>/ dir).
#
#   Everything after a literal `--` is passed verbatim to convert.py
#   (e.g. -- --run-name "..." --notes "...").
#
# MODES
#   --dry-run  Fetch (local sources only; remote URIs just print the fetch
#              command so the dry-run works offline), convert into a TEMP
#              out-dir (never docs/runs), and print the converter output, the
#              would-be branch name, and the git/gh commands full mode would
#              run. Executes none of them.
#   --local    Convert into docs/runs/, create branch run/<run_id> from HEAD,
#              git add the two changed files and commit. NO push, NO PR.
#   (default)  Full mode: --local plus `git push -u origin run/<run_id>` and
#              `gh pr create` with a generated body.
#
# EXAMPLES
#   # Inspect a remote bundle without touching anything (prints the fetch cmd):
#   scripts/ingest.sh gs://rpc-full-history/benchmarks/phase1-... \
#     --dataset-kind synthetic --dry-run
#
#   # Ingest a local tarball and stage a commit on a run/ branch (no push):
#   scripts/ingest.sh ./bench-results-phase1-...-20260722.tgz \
#     --dataset-kind synthetic --local
#
# CI NOTE
#   The future CI ingest workflow (.github/workflows/ingest.yml) is expected to
#   call this same script once bucket credentials / OIDC (the dev-hubble WIF
#   setup) exist — the script is the single source of truth for the fetch →
#   convert → PR flow, invoked locally today and from CI later.

set -euo pipefail

# ------------------------------------------------------------------ helpers
PROG="$(basename "$0")"
die()   { echo "$PROG: error: $*" >&2; exit 1; }
usage() { sed -n '3,45p' "$0" | sed 's/^# \{0,1\}//'; }

TMPDIRS=()
cleanup() {
  local d
  for d in ${TMPDIRS[@]+"${TMPDIRS[@]}"}; do
    [ -n "$d" ] && [ -d "$d" ] && rm -rf "$d"
  done
}
trap cleanup EXIT

# ------------------------------------------------------------------ arguments
BUNDLE=""
DATASET_KIND=""
MODE="full"
MODE_SET=0
FORCE=0
EXTRA=()

while [ $# -gt 0 ]; do
  case "$1" in
    --) shift; EXTRA=("$@"); break ;;
    --dataset-kind) DATASET_KIND="${2:-}"; shift 2 ;;
    --dataset-kind=*) DATASET_KIND="${1#*=}"; shift ;;
    --dry-run) [ "$MODE_SET" -eq 1 ] && die "pick only one of --dry-run/--local"; MODE="dry-run"; MODE_SET=1; shift ;;
    --local)   [ "$MODE_SET" -eq 1 ] && die "pick only one of --dry-run/--local"; MODE="local";   MODE_SET=1; shift ;;
    --force)   FORCE=1; shift ;;
    -h|--help) usage; exit 0 ;;
    -*) die "unknown option: $1 (use -- to pass args to convert.py)" ;;
    *)  if [ -z "$BUNDLE" ]; then BUNDLE="$1"; shift
        else die "unexpected argument: $1"; fi ;;
  esac
done

[ -n "$BUNDLE" ] || die "missing <bundle> (a gs://../s3:// URI, a local dir, or a .tgz)"
case "$DATASET_KIND" in
  pubnet|synthetic) ;;
  "") die "missing --dataset-kind <pubnet|synthetic>" ;;
  *)  die "invalid --dataset-kind '$DATASET_KIND' (want pubnet or synthetic)" ;;
esac

# Resolve a local bundle path to absolute BEFORE we cd into the repo, so a
# relative <bundle> given from the caller's cwd still resolves. URIs pass through.
case "$BUNDLE" in
  gs://*|s3://*) ;;
  *) [ -e "$BUNDLE" ] || die "bundle not found: $BUNDLE"
     BUNDLE="$(cd "$(dirname "$BUNDLE")" && pwd)/$(basename "$BUNDLE")" ;;
esac

# Run from the repo root regardless of the caller's cwd.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
cd "$REPO_ROOT"
[ -f converter/convert.py ] || die "converter/convert.py not found under $REPO_ROOT"

WORK="$(mktemp -d)"; TMPDIRS+=("$WORK")

# ------------------------------------------------------------------ source type
case "$BUNDLE" in
  gs://*) SRC_TYPE="remote-gs" ;;
  s3://*) SRC_TYPE="remote-s3" ;;
  *.tgz|*.tar.gz) SRC_TYPE="tarball"; [ -f "$BUNDLE" ] || die "tarball not found: $BUNDLE" ;;
  *) [ -d "$BUNDLE" ] || die "not a directory (and not a .tgz or gs://../s3:// URI): $BUNDLE"
     SRC_TYPE="localdir" ;;
esac

# run_id from a URI's last path component (trailing slash stripped).
uri_run_id() { local u="${1%/}"; echo "${u##*/}"; }

# metadata.json run_id for a fetched/local bundle dir ("" if none/unreadable).
meta_run_id() {
  python3 -c 'import json,os,sys
p=os.path.join(sys.argv[1],"metadata.json")
try:
    print(json.load(open(p)).get("run_id","") if os.path.isfile(p) else "")
except Exception:
    print("")' "$1" 2>/dev/null || true
}

# The fetch command a remote source needs (printed in dry-run, run otherwise).
fetch_cmd() {
  case "$SRC_TYPE" in
    remote-gs) printf 'gcloud storage rsync -r %q %q' "$1" "$2" ;;
    remote-s3) printf 'aws s3 sync %q %q' "$1" "$2" ;;
  esac
}

# ------------------------------------------------------------------ acquire bundle
# For local sources we always have a real BUNDLE_DIR. For remote sources we
# fetch in --local/full; in --dry-run we skip the fetch (offline) and leave
# BUNDLE_DIR empty, deriving the run_id from the URI instead.
BUNDLE_DIR=""
case "$SRC_TYPE" in
  localdir)
    BUNDLE_DIR="$BUNDLE"
    ;;
  tarball)
    ex="$(mktemp -d)"; TMPDIRS+=("$ex")
    tar -xzf "$BUNDLE" -C "$ex"
    BUNDLE_DIR="$(find "$ex" -mindepth 1 -maxdepth 1 -type d | head -1)"
    [ -n "$BUNDLE_DIR" ] || die "tarball has no top-level directory: $BUNDLE"
    ;;
  remote-gs|remote-s3)
    rid="$(uri_run_id "$BUNDLE")"
    dest="$WORK/$rid"
    if [ "$MODE" = "dry-run" ]; then
      echo "== fetch (dry-run: not executed) =="
      fetch_cmd "$BUNDLE" "$dest"; echo
      echo
    else
      mkdir -p "$dest"
      echo "== fetch =="
      cmd="$(fetch_cmd "$BUNDLE" "$dest")"
      echo "$cmd"
      eval "$cmd"
      BUNDLE_DIR="$dest"
    fi
    ;;
esac

# ------------------------------------------------------------------ resolve run_id
# Mirror convert.py's resolution so the branch name and the changed-file paths
# we predict match what the converter actually writes:
#   1) an explicit --run-id passed through after `--`
#   2) metadata.json run_id (when a local bundle is in hand)
#   3) the bundle/URI basename
RUN_ID_OVERRIDE=""
if [ "${#EXTRA[@]}" -gt 0 ]; then
  i=0
  while [ "$i" -lt "${#EXTRA[@]}" ]; do
    case "${EXTRA[$i]}" in
      --run-id)   j=$((i+1)); [ "$j" -lt "${#EXTRA[@]}" ] && RUN_ID_OVERRIDE="${EXTRA[$j]}" ;;
      --run-id=*) RUN_ID_OVERRIDE="${EXTRA[$i]#*=}" ;;
    esac
    i=$((i+1))
  done
fi

RUN_ID=""
if [ -n "$RUN_ID_OVERRIDE" ]; then
  RUN_ID="$RUN_ID_OVERRIDE"
elif [ -n "$BUNDLE_DIR" ]; then
  RUN_ID="$(meta_run_id "$BUNDLE_DIR")"
  [ -n "$RUN_ID" ] || RUN_ID="$(basename "$BUNDLE_DIR")"
else
  RUN_ID="$(uri_run_id "$BUNDLE")"
fi
[ -n "$RUN_ID" ] || die "could not determine run_id"

BRANCH="run/$RUN_ID"
RUN_JSON="docs/runs/$RUN_ID.json"
INDEX_JSON="docs/runs/index.json"

# ------------------------------------------------------------------ body builders
# Shared PR/commit body: metadata one-liners (or run_id only when absent),
# captured converter warnings, and a reviewer hint.
build_body() {
  local warnings="$1"
  local meta_lines=""
  if [ -n "$BUNDLE_DIR" ] && [ -f "$BUNDLE_DIR/metadata.json" ]; then
    meta_lines="$(python3 - "$BUNDLE_DIR" <<'PY'
import json, os, sys
m = json.load(open(os.path.join(sys.argv[1], "metadata.json")))
c = m.get("campaign", {})
name, cfg, ci = c.get("name", ""), c.get("config_file", ""), c.get("close_interval", "")
ds = ", ".join(d.get("name", "?") for d in m.get("datasets", []))
hw = m.get("hardware", {})
parts = []
if hw.get("instance_type"):
    parts.append(str(hw["instance_type"]))
if isinstance(hw.get("cpus"), int):
    parts.append("%d vCPU" % hw["cpus"])
if isinstance(hw.get("mem_total_kb"), int):
    parts.append("%dGi mem" % round(hw["mem_total_kb"] / (1024 * 1024)))
if name or cfg:
    print("Campaign: %s%s" % (name, (" (%s)" % cfg) if cfg else ""))
if ci:
    print("Close interval: %s" % ci)
if ds:
    print("Datasets: %s" % ds)
if parts:
    print("Hardware: %s" % ", ".join(parts))
PY
)" || meta_lines=""
  fi

  echo "Run: $RUN_ID"
  if [ -n "$meta_lines" ]; then
    echo "$meta_lines"
  fi
  echo
  if [ -n "$warnings" ]; then
    echo "Converter warnings:"
    echo "${warnings//WARN: /- }"
    echo
  fi
  echo "Reviewer: open the PR preview, select \"$RUN_ID\" in the run selector, then append ?view=hot (or use the hot toggle)."
}

# ------------------------------------------------------------------ convert
CONVERT_WARNINGS=""
run_convert() {
  local out_dir="$1" so se rc
  so="$WORK/convert.out"; se="$WORK/convert.err"
  echo "== convert =="
  set +e
  python3 converter/convert.py "$BUNDLE_DIR" \
    --dataset-kind "$DATASET_KIND" \
    --out-dir "$out_dir" \
    ${EXTRA[@]+"${EXTRA[@]}"} >"$so" 2>"$se"
  rc=$?
  set -e
  cat "$so"
  cat "$se" >&2
  [ "$rc" -eq 0 ] || die "converter failed (exit $rc)"
  CONVERT_WARNINGS="$(grep '^WARN:' "$se" || true)"
}

# ------------------------------------------------------------------ dry-run
if [ "$MODE" = "dry-run" ]; then
  if [ -n "$BUNDLE_DIR" ]; then
    dry_out="$WORK/dry-runs"; mkdir -p "$dry_out"
    run_convert "$dry_out"
  else
    echo "== convert =="
    echo "(skipped: remote bundle not fetched in --dry-run; runs after fetch in full mode)"
  fi
  echo
  echo "== would-be commit/PR body =="
  BODY="$(build_body "$CONVERT_WARNINGS")"
  echo "$BODY"
  echo
  echo "== would-be branch =="
  echo "$BRANCH"
  echo
  echo "== commands full mode would run (none executed) =="
  echo "git checkout -b $BRANCH"
  echo "git add $RUN_JSON $INDEX_JSON"
  echo "git commit -F <message>   # subject: runs: add $RUN_ID"
  echo "git push -u origin $BRANCH"
  echo "gh pr create --title \"runs: add $RUN_ID\" --body-file <body>"
  exit 0
fi

# ------------------------------------------------------------------ safety rails (local/full)
if ! git diff --quiet || ! git diff --cached --quiet; then
  die "working tree has uncommitted tracked changes; commit or stash before ingesting"
fi
if [ -f "$RUN_JSON" ] && [ "$FORCE" -ne 1 ]; then
  die "$RUN_JSON already exists (run already ingested); pass --force to re-ingest"
fi
[ -n "$BUNDLE_DIR" ] || die "no bundle directory to convert"

# Convert on the current branch, then branch off HEAD and commit there.
run_convert "docs/runs"

SUBJECT="runs: add $RUN_ID"
BODY="$(build_body "$CONVERT_WARNINGS")"

MSG_FILE="$WORK/commit-msg.txt"
{
  echo "$SUBJECT"
  echo
  echo "$BODY"
  echo
  echo "Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
} > "$MSG_FILE"

# Idempotent branch handling: reuse run/<run_id> if it already exists (e.g. a
# --force re-ingest), otherwise create it from the current HEAD.
if git show-ref --verify --quiet "refs/heads/$BRANCH"; then
  git checkout "$BRANCH"
else
  git checkout -b "$BRANCH"
fi

git add "$RUN_JSON" "$INDEX_JSON"
if git diff --cached --quiet; then
  echo "converted run is byte-identical to the committed one on $BRANCH; nothing to commit."
  exit 0
fi
git commit -F "$MSG_FILE"

echo
echo "Committed to branch $BRANCH:"
git show --stat --oneline -s HEAD
echo

if [ "$MODE" = "local" ]; then
  echo "Local mode: no push, no PR. Next steps:"
  echo "  git push -u origin $BRANCH"
  echo "  gh pr create --title \"$SUBJECT\" --body-file <(...)   # body is the commit message above"
  exit 0
fi

# ------------------------------------------------------------------ full mode: push + PR
PR_BODY_FILE="$WORK/pr-body.md"
{
  echo "$BODY"
  echo
  echo "🤖 Generated with [Claude Code](https://claude.com/claude-code)"
} > "$PR_BODY_FILE"

echo "== push =="
git push -u origin "$BRANCH"

echo "== gh pr create =="
gh pr create --title "$SUBJECT" --body-file "$PR_BODY_FILE"
