# How the query targets were derived

This is the record behind the numbers in [`targets.json`](targets.json) `query_load`. Derived 2026-09-01 from the team RPC SLA doc, sections 3.3, 3.4 and 4.1. Change a number here and in `targets.json` together; nothing else in this repo hardcodes one.

The benchmark judges **two separate requirements**. They are measured at different rates and against different numbers, and the reports keep them apart. Do not fold them into one verdict.

## Family 1 — the SLA

### Request rates

Section 3.4 states a sustained request-rate watermark per service tier, for one RPC node: Light approximately 250 rps, Standard approximately 500 rps, Heavy approximately 1000 rps. It also states the traffic mix that watermark assumes.

| Endpoint | Mix share | Floor at Standard (rps) |
|---|---|---|
| getTransaction (`txhash`) | 0.60 | 300 |
| getEvents (`events`) | 0.20 | 100 |
| getTransactions (`txpage`) | 0.15 | 75 |
| getLedgers (`ledgers`) | 0.05 | 25 |

Each floor is that endpoint's mix share times the Standard watermark of 500 rps. The mix is a property of the traffic, not of the dataset or the ledger close time, so one floor per endpoint holds in every phase and every dataset profile.

The `0.5x / 1x / 2x` ladder every leg sweeps IS the tier set: `0.5x` is Light, `1x` is Standard, `2x` is Heavy. The verdict is read at `1x`; the other two rungs show where the endpoint sits relative to the tier above and below.

Section 3.4 calls these watermarks conservative placeholders that need a load-test backstop before they ship. These benchmarks are that backstop.

### Latency targets

Section 4.1 states a P75 and a P99 per endpoint, per data-age window. The benchmark judges the P99: it is the SLA's binding number, and the bench emits p50, p90 and p99, so there is no p75 to compare against.

The two data-age windows the boxes can measure map onto the two tiers the bench already runs.

| SLA window | Bench tier | Why it matches |
|---|---|---|
| Live | `hot` | The active-chunk RocksDB store. The bench warms it before measuring, which is the hot-cache state the window assumes. |
| Recent | `cold` | Frozen artifacts on the local NVMe. The bench drops them from the page cache before measuring, which is the cold-cache state the window assumes. |
| Historical | none | Served from frozen artifacts on EBS. The benchmark boxes carry no EBS tier, so no leg measures this window. It is out of scope. |

| Endpoint | Live (hot) P99 | Recent (cold) P99 |
|---|---|---|
| getTransaction (`txhash`) | 20 ms | 30 ms |
| getTransactions (`txpage`) | 60 ms | 80 ms |
| getEvents (`events`) | 40 ms | 60 ms |
| getLedgers (`ledgers`) | 150 ms | 200 ms |

One number deviates from section 4.1: the getEvents Recent (cold) P99. Section 4.1 states 40 ms for both windows; every other endpoint gives the Recent window headroom over Live (1.33–1.5×), and the cold tier serves frozen artifacts after a page-cache drop, so a target equal to the hot one does not model the tier. Set to 60 ms (1.5× the Live target, the txpage ratio) on 2026-09-04, decision by Marwen. Runs converted before that date bake the 40 ms target and keep their original verdicts.

### Request shapes

The section 4.1 rows state the request each latency applies to, so the bench drives the same shapes.

- getLedgers: 10 ledgers per request.
- getTransactions: 200 transactions per page.
- getEvents: a small page, 10 matches — the shape the section 3.4 mix names.

## Family 2 — the end-to-end-budget probe

getTransaction also runs at the demand-derived floors of work item 856, per dataset profile and per phase: sac 300 / 500 / 1000 rps, custom_token 200 / 400 / 600, soroswap 75 / 150 / 300.

That leg answers for one number only: an in-RPC P99 of 10 ms. This is getTransaction's slice of the end-to-end transaction-lifecycle budget in `targets.json` `fixed_estimates`, so it measures the time inside the RPC and excludes the client's queueing and the assumed network legs. The scheduled P99 of the same cell is reported for context and is not judged: the arrival rate here is a demand estimate, not a rate the SLA states a tail for.

This is a different requirement from the SLA, at a different rate, against a different number. The two must not be conflated.

getTransaction therefore sweeps both ladders in ONE leg — `bench-query --target-rps` takes a list and emits a cell per rate — as the sorted, deduplicated union of the two. Where the two floors coincide, as they do for sac at phase 1, the shared cell carries both verdicts.

## Rejected alternative, kept for the record

Before section 3.4 was available, the floors were derived by Little's law from the section 3.3 per-endpoint concurrency caps: floor = cap / P99. getLedgers gave 32 / 0.200 s = 160 rps, and the other endpoints gave figures of the same order.

That method is discarded. Section 3.4 states the intended sustained load directly, so there is no need to infer it from a cap. The concurrency caps remain background context: they bound what a node accepts at once, not what it is expected to sustain.

## Known modeling caveat

Each leg drives one endpoint at a time. The mix rates are therefore applied per endpoint in sequence, not as one blended 500 rps stream, so contention between endpoints is not reproduced. A blended-mix leg would measure that; no leg does today.
