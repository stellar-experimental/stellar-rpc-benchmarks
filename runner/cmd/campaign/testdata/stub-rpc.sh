#!/bin/sh
# Stub stellar-rpc binary for the campaign runner's end-to-end test
# (cmd/campaign/e2e_test.go). It answers the bench subcommands the campaign
# drives, materializes the directories they would materialize, and writes the
# CSV and invocation.json shapes converter/tests/fixtures.py documents — enough
# for the real converter to convert a bundle this stub produced.
#
# Environment:
#   STUB_RPC_COMMIT   40-hex commit to report as the build identity. It must be
#                     the sha the test's fake git resolves to, or the converter
#                     warns about a binary/manifest commit mismatch.
#   STUB_RPC_CONTROL  optional control file, one `fail <out-basename>` or
#                     `hang <out-basename>` line per leg to derail. A failed leg
#                     writes a partial driver.csv and an invocation.json
#                     carrying an error (stellar-rpc#907) and exits 1; a hung
#                     leg writes nothing and sleeps until it is killed.
#
# Flags are read in the --key=value form the runner emits, and only the ones
# that decide what this stub writes.
set -eu

version=v20.3.1-999-gstubbed
branch=stub/e2e
commit=${STUB_RPC_COMMIT:-0123456789abcdef0123456789abcdef01234567}
header='stage,n,n_items,total_ns,p50_ns,p90_ns,p99_ns,max_ns'

sub=${1:-}
tier=${2:-}

if [ "$sub" = version ]; then
	echo "stellar-rpc $version"
	echo "commit: $commit"
	echo "branch: $branch"
	exit 0
fi

out=
pack_dir=
cold_out_dir=
hot_dir=
close_interval=
types=ledgers,txpage,txhash,events
concurrency=1
for arg in "$@"; do
	case $arg in
	--out=*) out=${arg#--out=} ;;
	--pack-dir=*) pack_dir=${arg#--pack-dir=} ;;
	--cold-out-dir=*) cold_out_dir=${arg#--cold-out-dir=} ;;
	--hot-dir=*) hot_dir=${arg#--hot-dir=} ;;
	--close-interval=*) close_interval=${arg#--close-interval=} ;;
	--types=*) types=${arg#--types=} ;;
	--query-concurrency=*) concurrency=${arg#--query-concurrency=} ;;
	esac
done

# csv_start writes the header; csv_row appends one row. Splitting them keeps the
# per-leg row lists readable as the tables they are.
csv_start() { echo "$header" >"$1"; }
csv_row() { echo "$2" >>"$1"; }

# write_invocation mirrors stellar-rpc's own writer: camelCase keys, and an
# `error` field on the runs that failed.
write_invocation() {
	inv_error=${1:-}
	{
		echo '{'
		echo '  "schemaVersion": 1,'
		printf '  "command": "stellar-rpc %s %s",\n' "$sub" "$tier"
		echo '  "flags": {'
		if [ -n "$close_interval" ]; then
			printf '    "close-interval": "%s",\n' "$close_interval"
		fi
		printf '    "out": "%s"\n' "$out"
		echo '  },'
		echo '  "binary": {'
		printf '    "version": "%s",\n' "$version"
		printf '    "commitHash": "%s",\n' "$commit"
		echo '    "buildTimestamp": "2026-07-30T00:00:00",'
		printf '    "branch": "%s"\n' "$branch"
		echo '  },'
		echo '  "hostname": "e2e-stub",'
		echo '  "startedAt": "2026-07-30T01:00:00Z",'
		if [ -n "$inv_error" ]; then
			printf '  "error": "%s",\n' "$inv_error"
		fi
		echo '  "finishedAt": "2026-07-30T01:00:01Z"'
		echo '}'
	} >"$out/invocation.json"
}

# control_mode is what the control file says to do with this leg: run, fail, or
# hang. The last matching line wins.
control_mode() {
	mode=run
	if [ -n "${STUB_RPC_CONTROL:-}" ] && [ -f "$STUB_RPC_CONTROL" ]; then
		while read -r verb target; do
			if [ "$target" = "$1" ] && { [ "$verb" = fail ] || [ "$verb" = hang ]; }; then
				mode=$verb
			fi
		done <"$STUB_RPC_CONTROL"
	fi
	echo "$mode"
}

if [ -n "$out" ]; then
	case "$(control_mode "${out##*/}")" in
	hang)
		# Nothing on disk: the leg the test kills must look partial, not failed.
		sleep 600
		exit 0
		;;
	fail)
		mkdir -p "$out"
		csv_start "$out/driver.csv"
		csv_row "$out/driver.csv" 'backfill_wall,1,0,50000,50000,50000,50000,50000'
		write_invocation 'induced failure'
		exit 1
		;;
	esac
fi

case "$sub $tier" in
"bench-ingest fixture")
	# Generation only stages a pack tree; it is untimed and writes no --out.
	mkdir -p "$pack_dir"
	echo 'stub fixture pack' >"$pack_dir/pack-0001.stub"
	;;

"bench-ingest cold")
	# One code path for the untimed golden freeze and the timed cold legs: both
	# read a pack tree and write a cold store.
	mkdir -p "$cold_out_dir/ledgers"
	echo 'stub cold store' >"$cold_out_dir/ledgers/chunk-0001.stub"
	mkdir -p "$out"
	csv_start "$out/driver.csv"
	csv_row "$out/driver.csv" 'backfill_wall,1,0,50000,50000,50000,50000,50000'
	csv_row "$out/driver.csv" 'index_rebuild,1,0,3000,3000,3000,3000,3000'
	csv_row "$out/driver.csv" 'chunk_total,1,0,40000,40000,40000,40000,40000'
	csv_row "$out/driver.csv" 'ledgers_total,100,100,8000,80,90,99,120'
	csv_row "$out/driver.csv" 'txhash_total,600,600,6000,60,70,90,100'
	csv_row "$out/driver.csv" 'events_total,600,600,9000,90,95,99,110'
	csv_row "$out/driver.csv" 'cold_extract,1,0,7000,70,80,90,100'
	csv_row "$out/driver.csv" \
		'peak_rss_bytes,1,0,20000000000,20000000000,20000000000,20000000000,20000000000'
	csv_start "$out/events.csv"
	csv_row "$out/events.csv" 'term_index,600,600,4000,40,45,49,60'
	csv_row "$out/events.csv" 'write,600,600,3000,30,35,39,50'
	csv_start "$out/ledgers.csv"
	csv_row "$out/ledgers.csv" 'write,100,100,3000,30,35,39,50'
	csv_start "$out/txhash.csv"
	csv_row "$out/txhash.csv" 'finalize,1,0,1500,1500,1500,1500,1500'
	write_invocation
	;;

"bench-ingest hot")
	mkdir -p "$hot_dir"
	echo 'stub hot db' >"$hot_dir/hot.stub"
	mkdir -p "$out"
	csv_start "$out/driver.csv"
	csv_row "$out/driver.csv" 'ingest_total,100,100,2000,100,200,500,900'
	csv_row "$out/driver.csv" 'run_wall,1,100,60000,60000,60000,60000,60000'
	csv_row "$out/driver.csv" \
		'peak_rss_bytes,1,0,14000000000,14000000000,14000000000,14000000000,14000000000'
	csv_start "$out/hot.csv"
	csv_row "$out/hot.csv" 'extract,100,0,300,30,40,50,80'
	csv_row "$out/hot.csv" 'ledgers,100,100,400,40,50,60,90'
	csv_row "$out/hot.csv" 'txhash,100,600,200,20,25,30,50'
	csv_row "$out/hot.csv" 'events,100,600,500,50,55,60,90'
	csv_row "$out/hot.csv" 'commit,100,0,800,80,90,120,200'
	csv_row "$out/hot.csv" 'apply,100,0,250,25,30,40,70'
	write_invocation
	;;

"bench-query cold" | "bench-query hot")
	mkdir -p "$out"
	csv_start "$out/driver.csv"
	csv_row "$out/driver.csv" 'open,1,0,5000,5000,5000,5000,5000'
	# The comma-separated --types and --query-concurrency lists are split on
	# purpose: one per-type CSV, one column set per concurrency level.
	IFS=,
	for qt in $types; do
		csv_start "$out/$qt.csv"
		for w in $concurrency; do
			csv_row "$out/driver.csv" "${qt}_c${w},1,0,1000000,1000000,1000000,1000000,1000000"
			csv_row "$out/$qt.csv" "total_c${w},100,100,900000,9000,11000,13000,20000"
		done
	done
	unset IFS
	write_invocation
	;;

*)
	echo "stub-rpc: unsupported command: $*" >&2
	exit 64
	;;
esac
