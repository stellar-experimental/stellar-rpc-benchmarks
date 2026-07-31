#!/usr/bin/env bash
# Stamp a content hash onto every viewer asset URL in the published HTML.
#
# Pages serves app.js/styles.css with a 10-min max-age and browsers cache them,
# so viewer code changes weren't visible without a manual cache clear. The
# stamped URL changes only when the file's bytes change, so run-only deploys
# keep the cache warm while viewer edits force a re-fetch.
#
# Both deploy-pages.yml and pr-preview.yml call this, always on their ephemeral
# checkout — main stays unstamped, so `make serve` and the smoke test are
# unaffected. It matters most for the PR preview, whose URL is constant across
# pushes to a PR: without stamping a browser keeps serving the cached assets
# while you iterate. Harmless on a PR close event (nothing is deployed then).
#
# Usage: cache-bust.sh [docs-dir]   (default: docs, i.e. run from the repo root)
set -euo pipefail

docs="${1:-docs}"

sha8() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | cut -c1-8
  else
    shasum -a 256 "$1" | cut -c1-8
  fi
}

# stamp <html> <attr> <asset> — rewrite <attr>="<asset>" to <attr>="<asset>?v=<hash>".
# Every reference below must be present: a missing one means the HTML drifted
# away from this list, and deploying a silently unstamped asset is exactly the
# staleness this script exists to prevent, so fail loudly instead.
stamp() {
  local html="$1" attr="$2" asset="$3"
  local page="$docs/$html" file="$docs/$asset" ref pattern hash

  [ -f "$page" ] || { echo "cache-bust: no such page: $page" >&2; return 1; }
  [ -f "$file" ] || { echo "cache-bust: no such asset: $file" >&2; return 1; }

  ref="$attr=\"$asset\""
  grep -qF -- "$ref" "$page" || {
    echo "cache-bust: $html no longer references $ref" >&2
    return 1
  }

  pattern="$attr=\"${asset//./\\.}\""
  hash=$(sha8 "$file")
  sed -i.bak "s|$pattern|$attr=\"$asset?v=$hash\"|" "$page"
  rm -f "$page.bak"
  echo "cache-bust: $html -> $asset?v=$hash"
}

stamp index.html         href styles.css
stamp index.html         src  app.js
stamp summary.html       href styles.css
stamp summary.html       href summary.css
stamp summary.html       src  summary.js
stamp tx-submission.html href styles.css
stamp tx-submission.html src  txsub.js
# latency-model.html builds its own charts from an inline <script>, but it does
# pull the shared stylesheet, so it needs the styles.css stamp too.
stamp latency-model.html href styles.css
