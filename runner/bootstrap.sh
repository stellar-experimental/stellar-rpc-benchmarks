#!/usr/bin/env bash
#
# Idempotent bootstrap for a full-history benchmark machine: an EC2 instance
# with a local NVMe instance store (e.g. m6id.2xlarge) running Ubuntu 24.04.
# It only provisions — NVMe mount, apt packages, Go, Rust, native libs, env;
# campaign.sh does all cloning-current and building. Safe to re-run any time —
# in particular after an instance stop/start, which wipes the NVMe instance
# store (golden packs are re-downloaded and the build clone re-created by the
# next bootstrap/campaign run).
#
# Usage (on the machine):
#   ./runner/bootstrap.sh
#
# Overridable: NVME_DEV (default /dev/nvme1n1), BENCH_ROOT (default
# /mnt/nvme/bench), REPO (git URL or local path of stellar-rpc, default
# https://github.com/stellar/stellar-rpc.git).
#
set -euo pipefail

NVME_DEV="${NVME_DEV:-/dev/nvme1n1}"
MOUNT=/mnt/nvme
BENCH_ROOT="${BENCH_ROOT:-$MOUNT/bench}"
REPO="${REPO:-https://github.com/stellar/stellar-rpc.git}"
SRC=$BENCH_ROOT/src

note() { echo "== $*"; }

# --- NVMe instance store: format if raw, mount if unmounted -----------------
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
case "$probe" in
  *GB/s*) echo "WARNING: GB/s-scale dsync writes — fsync is being absorbed; hot-commit numbers would be fiction" >&2 ;;
esac

# --- system packages ---------------------------------------------------------
note "apt packages"
sudo apt-get update -qq
sudo apt-get install -y -qq build-essential git jq pkg-config cmake ninja-build \
  tmux libsnappy-dev liblz4-dev zlib1g-dev

# --- cloud CLIs: only some campaigns need them, so warn rather than fail -----
# gcloud: packs-gs datasets and gs:// publishing. aws: bsb-s3 datasets and
# s3:// publishing. Neither ships in apt in a form worth installing here.
command -v gcloud >/dev/null 2>&1 ||
  echo "WARNING: gcloud not found — packs-gs datasets and gs:// PUBLISH_URI will fail; install it: https://cloud.google.com/sdk/docs/install" >&2
command -v aws >/dev/null 2>&1 ||
  echo "WARNING: aws not found — bsb-s3 datasets and s3:// PUBLISH_URI will fail; install it: https://docs.aws.amazon.com/cli/latest/userguide/getting-started-install.html" >&2

# --- Go (>= 1.26; Noble's apt Go is too old) ---------------------------------
if ! /usr/local/go/bin/go version 2>/dev/null | grep -Eq 'go1\.(2[6-9]|[3-9][0-9])'; then
  note "installing Go"
  GOVER=$(curl -fsSL 'https://go.dev/VERSION?m=text' | head -1)
  curl -fsSL "https://go.dev/dl/${GOVER}.linux-amd64.tar.gz" -o /tmp/go.tgz
  # decompress as the user: sudo'd tar cannot always exec gzip
  gunzip -f /tmp/go.tgz
  sudo rm -rf /usr/local/go && sudo tar -C /usr/local -xf /tmp/go.tar
fi

# --- Rust --------------------------------------------------------------------
if [ ! -x "$HOME/.cargo/bin/rustc" ]; then
  note "installing Rust"
  curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs | sh -s -- -y
fi

# --- build clone --------------------------------------------------------------
# The box needs no standalone stellar-rpc checkout: seed the persistent build
# clone campaign.sh maintains at $BENCH_ROOT/src, and run the native-lib
# install scripts below from it. campaign.sh re-points, fetches, and checks
# out this clone per campaign (and re-clones it itself if this step is ever
# skipped).
if [ ! -d "$SRC/.git" ]; then
  note "cloning $REPO into $SRC"
  git clone "$REPO" "$SRC"
fi

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

note "bootstrap OK — campaign.sh builds the benchmark binary on first run"
