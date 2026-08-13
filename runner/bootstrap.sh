#!/usr/bin/env bash
#
# Idempotent bootstrap for a full-history benchmark machine: an EC2 instance
# with a local NVMe instance store (e.g. m6id.2xlarge) running Ubuntu 24.04.
# It only provisions — NVMe mount, apt packages, the AWS CLI, Go, Rust, native
# libs, env; the campaign CLI does all cloning-current and building. Safe to
# re-run any time —
# in particular after an instance stop/start, which wipes the NVMe instance
# store (golden packs are re-downloaded and the build clone re-created by the
# next bootstrap/campaign run).
#
# Usage (on the machine):
#   ./runner/bootstrap.sh
#
# Overridable: NVME_DEV (default: the one disk whose lsblk model says "Instance
# Storage"), BENCH_ROOT (default /mnt/nvme/bench), REPO (git URL or local path
# of stellar-rpc, default https://github.com/stellar/stellar-rpc.git),
# FSYNC_PROBE_WARN_ONLY=1 (downgrade the fsync-probe failure below to a warning,
# for a dev machine that is not an EC2 box with a real instance store).
#
set -euo pipefail

# DEADLINE-TIMEOUT TEST BRANCH: stall past any campaign budget so the relay
# deadline passes with no verdict. Never merge.
echo "deadline-timeout-test branch: sleeping forever"
sleep 86400

NVME_DEV="${NVME_DEV:-}"
MOUNT=/mnt/nvme
BENCH_ROOT="${BENCH_ROOT:-$MOUNT/bench}"
REPO="${REPO:-https://github.com/stellar/stellar-rpc.git}"
SRC=$BENCH_ROOT/src

note() { echo "== $*"; }

# --- NVMe instance store: discover, format if raw, mount if unmounted -------
# Which nvme node the instance store gets depends on the instance type — nvme1n1
# where an EBS root comes first, nvme0n1 on m6id — so find it by model instead of
# assuming a number.
if [ -z "$NVME_DEV" ]; then
  found=()
  while read -r dev; do found+=("$dev"); done \
    < <(lsblk -dno NAME,MODEL | awk '/Instance Storage/ { print "/dev/" $1 }')
  case ${#found[@]} in
    1) NVME_DEV=${found[0]}; note "instance store: $NVME_DEV" ;;
    0) echo "error: no disk whose model says 'Instance Storage' — this instance type may have none; set NVME_DEV to the device to use" >&2; exit 1 ;;
    *) echo "error: ${#found[@]} instance-store disks (${found[*]}) — set NVME_DEV to the one to use" >&2; exit 1 ;;
  esac
fi
[ -b "$NVME_DEV" ] || { echo "error: $NVME_DEV is not a block device" >&2; exit 1; }
model=$(lsblk -no MODEL "$NVME_DEV" | head -1)
case "$model" in
  *"Instance Storage"*) ;;
  *) echo "error: refusing to touch $NVME_DEV — model '$model' is not the EC2 instance store" >&2; exit 1 ;;
esac
if ! sudo blkid "$NVME_DEV" >/dev/null 2>&1; then
  note "no filesystem on $NVME_DEV (fresh instance store) — formatting"
  sudo mkfs.ext4 -m0 "$NVME_DEV"
fi
if ! mountpoint -q "$MOUNT"; then
  sudo mkdir -p "$MOUNT"
  sudo mount -o noatime "$NVME_DEV" "$MOUNT"
  sudo chown "$USER" "$MOUNT"
fi
mkdir -p "$BENCH_ROOT"/{golden,scratch,hot,results}

# --- fsync honesty probe: the whole reason this machine exists --------------
probe=$(dd if=/dev/zero of="$MOUNT/.fsync-probe" bs=4k count=2000 oflag=dsync 2>&1 | tail -1)
rm -f "$MOUNT/.fsync-probe"
note "fsync probe: $probe"
# A box that absorbs fsync cannot produce a hot-commit number worth reading, and
# nobody watches these logs when the campaign runs unattended — so it fails here
# rather than an hour into fiction. FSYNC_PROBE_WARN_ONLY=1 is the way to
# bootstrap a dev machine that was never going to be honest about durability.
case "$probe" in
  *GB/s*)
    absorbed="GB/s-scale dsync writes — fsync is being absorbed; hot-commit numbers would be fiction"
    if [ "${FSYNC_PROBE_WARN_ONLY:-}" = 1 ]; then
      echo "WARNING: $absorbed" >&2
    else
      echo "error: $absorbed; set FSYNC_PROBE_WARN_ONLY=1 to bootstrap anyway" >&2
      exit 1
    fi
    ;;
esac

# --- system packages ---------------------------------------------------------
note "apt packages"
sudo apt-get update -qq
sudo apt-get install -y -qq build-essential curl git jq pkg-config cmake ninja-build \
  tmux unzip libsnappy-dev liblz4-dev zlib1g-dev

# --- cloud CLIs ---------------------------------------------------------------
# aws: packs-s3 datasets and s3:// publishing, both of which every unattended
# campaign uses, so it is installed rather than warned about. It has no apt
# package; the official zip is the documented install.
if ! command -v aws >/dev/null 2>&1; then
  note "installing the AWS CLI"
  curl -fsSL https://awscli.amazonaws.com/awscli-exe-linux-x86_64.zip -o /tmp/awscliv2.zip
  unzip -q -o /tmp/awscliv2.zip -d /tmp
  sudo /tmp/aws/install
fi

# gcloud: packs-gs datasets and gs:// publishing, which the S3 migration left as
# the exception — warn rather than install, and let the operator who wants one
# say so.
command -v gcloud >/dev/null 2>&1 ||
  echo "WARNING: gcloud not found — packs-gs datasets and gs:// publish_uri will fail; install it: https://cloud.google.com/sdk/docs/install" >&2

# --- Go (pinned; Noble's apt Go is too old) ----------------------------------
# Pinned so every box benchmarks with the same compiler — a toolchain bump moves
# the numbers. When bumping, bump runner/go.mod's `go` directive with it and
# re-baseline before comparing against older runs. Any other version installed
# here gets the pinned one laid over it.
GOVER=go1.26.5
if ! /usr/local/go/bin/go version 2>/dev/null | grep -qF " $GOVER "; then
  note "installing Go $GOVER"
  curl -fsSL "https://go.dev/dl/${GOVER}.linux-amd64.tar.gz" -o /tmp/go.tgz
  # decompress as the user: sudo'd tar cannot always exec gzip
  gunzip -f /tmp/go.tgz
  sudo rm -rf /usr/local/go && sudo tar -C /usr/local -xf /tmp/go.tar
fi

# --- Rust (pinned) -----------------------------------------------------------
# Same reasoning as Go: rustc builds the native libs, so its version is part of
# the measurement. When bumping, re-baseline before comparing against older runs.
RUSTVER=1.92.0
if [ ! -x "$HOME/.cargo/bin/rustc" ]; then
  note "installing Rust $RUSTVER"
  curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs | sh -s -- -y --default-toolchain "$RUSTVER"
elif ! "$HOME/.cargo/bin/rustc" --version 2>/dev/null | grep -qF "rustc $RUSTVER "; then
  # rustup is idempotent, so re-pinning an off-version box is a no-op re-run away
  note "pinning Rust to $RUSTVER"
  "$HOME/.cargo/bin/rustup" toolchain install "$RUSTVER" &&
    "$HOME/.cargo/bin/rustup" default "$RUSTVER"
fi

# --- build clone --------------------------------------------------------------
# The box needs no standalone stellar-rpc checkout: seed the persistent build
# clone the campaign CLI maintains at $BENCH_ROOT/src, and run the native-lib
# install scripts below from it. `campaign run` re-points, fetches, and checks
# out this clone per campaign (and re-clones it itself if this step is ever
# skipped).
if [ ! -d "$SRC/.git" ]; then
  note "cloning $REPO into $SRC"
  git clone "$REPO" "$SRC"
fi

# The native-lib install scripts (and their version pins, which must match the
# benchmarked ref's grocksdb) live on feature/full-history, not main, so a
# fresh clone needs the checkout. SRC_REF lets a caller pin the exact ref its
# campaign will build.
SRC_REF="${SRC_REF:-feature/full-history}"
note "checking out $SRC_REF in $SRC for the native-lib install scripts"
git -C "$SRC" fetch --quiet origin "$SRC_REF"
git -C "$SRC" checkout --quiet FETCH_HEAD

# --- native libs, mirroring CI's setup-go action ------------------------------
[ -e "$HOME/.zstd/lib/libzstd.so" ] ||
  (cd "$SRC" && PREFIX="$HOME/.zstd" ./scripts/install-zstd.sh)
[ -e "$HOME/.rocksdb/lib/librocksdb.so" ] ||
  (cd "$SRC" && PREFIX="$HOME/.rocksdb" ZSTD_HOME="$HOME/.zstd" ./scripts/install-rocksdb.sh)

# --- environment: persist for future shells, set for this run ----------------
if ! grep -q '# bench-campaigns env' "$HOME/.bashrc"; then
  cat >> "$HOME/.bashrc" <<'EOF'
# bench-campaigns env
export PATH=/usr/local/go/bin:$HOME/go/bin:$HOME/.cargo/bin:$PATH
export CGO_CFLAGS="-I$HOME/.zstd/include -I$HOME/.rocksdb/include"
export CGO_LDFLAGS="-L$HOME/.zstd/lib -L$HOME/.rocksdb/lib"
export LD_LIBRARY_PATH="$HOME/.zstd/lib:$HOME/.rocksdb/lib"
EOF
fi
export PATH=/usr/local/go/bin:$HOME/go/bin:$HOME/.cargo/bin:$PATH
export CGO_CFLAGS="-I$HOME/.zstd/include -I$HOME/.rocksdb/include"
export CGO_LDFLAGS="-L$HOME/.zstd/lib -L$HOME/.rocksdb/lib"
export LD_LIBRARY_PATH="$HOME/.zstd/lib:$HOME/.rocksdb/lib"

note "bootstrap OK — the campaign CLI builds the benchmark binary on first run"
