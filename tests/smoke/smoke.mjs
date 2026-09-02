/* Headless smoke test for the docs/ benchmark-report viewer.
   No HTTP server: index.html is loaded into jsdom and window.fetch is shimmed
   to read runs/*.json straight from the docs/ directory. One page load per run
   in the manifest; exits nonzero on any failed assertion or console error. */
import { JSDOM, VirtualConsole } from "jsdom";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const HERE = path.dirname(fileURLToPath(import.meta.url));
const DOCS = path.resolve(HERE, "..", "..", "docs");
const FIXTURES = path.join(HERE, "fixtures");
const ORIGIN = "http://viewer.local";

const MIME = { ".json": "application/json", ".js": "text/javascript", ".css": "text/css", ".html": "text/html" };

const body200 = (body, ext) => ({
  ok: true, status: 200,
  headers: { get: (h) => (h.toLowerCase() === "content-type" ? MIME[ext] || "text/plain" : null) },
  text: async () => body,
  json: async () => JSON.parse(body),
});

/* `overlay` maps a request path ("runs/<id>.json") to a body served instead of
   the file under docs/. Everything not named in it falls through to docs/, so
   the manifest runs load byte-identically with or without one. */
function makeFetch(overlay) {
  return function (input) {
    const u = new URL(input, ORIGIN + "/");
    const rel = decodeURIComponent(u.pathname).replace(/^\/+/, "");
    if (overlay && Object.prototype.hasOwnProperty.call(overlay, rel)) {
      return Promise.resolve(body200(overlay[rel], ".json"));
    }
    const file = path.join(DOCS, rel);
    return new Promise((resolve) => {
      fs.readFile(file, "utf8", (err, body) => {
        if (err) {
          resolve({ ok: false, status: 404, headers: { get: () => null }, text: async () => "", json: async () => { throw new Error("404 " + u.pathname); } });
          return;
        }
        resolve(body200(body, path.extname(file)));
      });
    });
  };
}

const fetchShim = makeFetch(null);

async function loadViewer(query, opts = {}) {
  const page = opts.page || "index.html";
  const script = opts.script || "app.js";
  const errors = [];
  const vc = new VirtualConsole();
  vc.on("jsdomError", (e) => errors.push("jsdomError: " + (e.detail?.message || e.message || e)));
  vc.on("error", (...a) => errors.push("console.error: " + a.join(" ")));

  const html = fs.readFileSync(path.join(DOCS, page), "utf8");
  const dom = new JSDOM(html, {
    url: `${ORIGIN}/${query}`,
    runScripts: "dangerously",
    pretendToBeVisual: true,
    virtualConsole: vc,
  });
  const { window } = dom;
  // Environment stubs jsdom lacks (layout + media queries + fetch).
  window.fetch = opts.fetch || fetchShim;
  window.matchMedia = window.matchMedia || ((q) => ({ matches: false, media: q, addEventListener() {}, removeEventListener() {} }));
  Object.defineProperty(window.HTMLElement.prototype, "clientWidth", { get() { return 880; }, configurable: true });
  Object.defineProperty(window.Element.prototype, "getBoundingClientRect", {
    value: () => ({ width: 100, height: 30, top: 0, left: 0, right: 100, bottom: 30 }), configurable: true,
  });

  // The page references its script via <script src>, which jsdom does not fetch
  // without `resources: "usable"` (an HTTP loader we deliberately avoid) — inject it.
  const app = fs.readFileSync(path.join(DOCS, script), "utf8");
  const s = window.document.createElement("script");
  s.textContent = app;
  window.document.body.appendChild(s);

  // Wait for the async fetch + render to settle.
  for (let i = 0; i < 80; i++) {
    await new Promise((r) => setTimeout(r, 25));
    if (window.document.querySelector("#report .masthead")) break;
  }
  await new Promise((r) => setTimeout(r, 150));
  return { window, errors };
}

const txt = (el) => (el ? el.textContent.replace(/\s+/g, " ").trim() : "");

let pass = 0, fail = 0;
const failures = [];
function check(group, name, cond, detail) {
  if (cond) { pass++; console.log(`  ok   [${group}] ${name}`); }
  else { fail++; failures.push(`[${group}] ${name} — got: ${detail}`); console.log(`  FAIL [${group}] ${name} — got: ${detail}`); }
}

/* Per-kind structural expectations. Sections are counted as rendered
   <section> elements; ids assert the kind-specific sections exist. */
const EXPECT = {
  pubnet: { sections: 8, requiredIds: ["queries", "target"], minFigures: 11, minSvgs: 10 },
  synthetic: { sections: 7, requiredIds: ["target"], minFigures: 7, minSvgs: 7 },
};

/* Which query-cell generation a run carries (SCHEMA "Query cells: two
   generations"): "closed" = a c<W> concurrency sweep, "open" = r<rate> paced
   legs, null = no queries. A run carries one or the other, never both. */
const CONC_KEY = /^c\d+$/;
const RATE_KEY = /^r\d+(\.\d+)?$/;
const rateKeys = (qout) => Object.keys(qout).filter((k) => RATE_KEY.test(k)).sort((a, b) => +a.slice(1) - +b.slice(1));
/* Every (tier, unit, qtype) entry the run recorded, walked the way the viewer's
   queryGrid walks it. A run can be missing a whole (tier, unit) — the first
   unit's cold query legs may have failed while the other units' ran — so the
   grid is the union across all of them, never one corner's shape. */
function qEntries(D) {
  const out = [];
  for (const tier of Object.keys(D.queries || {})) {
    for (const u of D.dataset.unit_order) {
      const e = ((D.queries || {})[tier] || {})[u] || {};
      for (const qt of Object.keys(e)) if (qt !== "setup") out.push({ tier, u, qt, qout: e[qt] });
    }
  }
  return out;
}
const qTypes = (D) => [...new Set(qEntries(D).map((e) => e.qt))];
/* The first entry that carries cells: what decides the generation, and the
   concurrency axis for a closed-loop run. */
const qEntry = (D) => (qEntries(D).find((e) =>
  Object.keys(e.qout).some((k) => CONC_KEY.test(k) || RATE_KEY.test(k))) || { qout: {} }).qout;
function queryShape(D) {
  if (!D.queries) return null;
  const e = qEntry(D);
  if (Object.keys(e).some((k) => CONC_KEY.test(k))) return "closed";
  return rateKeys(e).length ? "open" : null;
}
/* The run's read-path latency target for one endpoint in one storage tier, and
   the rung a verdict table judges — both read exactly the way docs/app.js reads
   them (queryCheck + thresholdFor / judgedRate). A run states its target either
   per endpoint and tier (targets_ns, the SLA shape) or as one number for the
   whole run (threshold_ns, the published generation). */
const allChecks = (D) => D.checks_all || [D.checks || {}];
// Matched on kind first: two checks share applies_to "queries" once the
// families are split. The applies_to fallback is the legacy single check.
const queryCheck = (D) => allChecks(D).find((c) => c && c.kind === "query_sla")
  || allChecks(D).find((c) => c && c.applies_to === "queries") || {};
const queryThr = (D, qt, tier) => {
  const c = queryCheck(D);
  const byTier = c.targets_ns && c.targets_ns[qt];
  if (byTier && byTier[tier] != null) return byTier[tier];
  return c.threshold_ns || 500e6;
};
/* The two verdict families, read the way docs/app.js reads them. verdict_sla
   covers every endpoint at its SLA rate; verdict_e2e covers getTransaction at
   the demand-derived rate and judges the in-RPC time alone. Runs converted
   before the split carry one verdict_1x that plays the SLA role. */
const slaVerdict = (qout) => (qout && (qout.verdict_sla || qout.verdict_1x)) || null;
const e2eVerdict = (qout) => (qout && qout.verdict_e2e) || null;
const e2eEntries = (D) => qEntries(D).filter((e) => e2eVerdict(e.qout));
const hasE2EFamily = (D) => e2eEntries(D).length > 0;
const judgedKey = (qout) => {
  const v = slaVerdict(qout);
  if (v && v.rate && qout[v.rate]) return v.rate;
  const ks = rateKeys(qout);
  return ks.length ? ks[Math.floor((ks.length - 1) / 2)] : null;
};

function checkKind(kind, doc, group, data) {
  const reps = data.campaign.reps;
  const base = EXPECT[kind];
  if (!base) { check(group, `known kind (${kind})`, false, "no expectations defined"); return; }
  const exp = { ...base };
  // Single-rep runs hide the run-to-run variance section (one section, one
  // figure, one SVG fewer).
  if (kind === "synthetic" && reps === 1) { exp.sections -= 1; exp.minFigures -= 1; exp.minSvgs -= 1; }
  // Hot-only runs (ingest = "hot") hide the cold section and its three
  // figures, plus the cold-vs-hot rate figure in the hot section.
  if (kind === "synthetic" && !data.ingest_cold) { exp.sections -= 1; exp.minFigures -= 4; exp.minSvgs -= 4; }
  // A synthetic run that swept queries gains the queries section. Both cell
  // generations draw three charts; the closed-loop sweep adds one table figure
  // (setup/event scan), the open-loop one adds two (the 1x verdict table as
  // well). checkSyntheticQueries pins the exact per-section counts.
  if (kind === "synthetic" && data.queries) {
    exp.sections += 1;
    exp.minFigures += queryShape(data) === "open" ? 5 : 4;
    // A run that also carries the E2E-budget family draws its table as a
    // figure of its own — the two requirements never share one.
    if (hasE2EFamily(data)) exp.minFigures += 1;
    exp.minSvgs += 3;
  }
  const report = doc.getElementById("report");
  const sections = report.querySelectorAll("section").length;
  check(group, `${exp.sections} sections rendered`, sections === exp.sections, sections);
  if (kind === "synthetic") {
    check(group, reps === 1 ? "variance section hidden (single run)" : "variance section present",
      !!doc.getElementById("variance") === (reps > 1), reps + " reps, variance " + (doc.getElementById("variance") ? "present" : "absent"));
  }
  for (const id of exp.requiredIds) check(group, `section #${id} present`, !!doc.getElementById(id), "missing");
  const figs = report.querySelectorAll("figure.fig").length;
  check(group, `>= ${exp.minFigures} figures`, figs >= exp.minFigures, figs);
  const svgs = report.querySelectorAll("svg").length;
  check(group, `>= ${exp.minSvgs} SVG charts`, svgs >= exp.minSvgs, svgs);
  const targetRows = report.querySelectorAll("#target-table tr").length;
  check(group, "target/keep-up table populated", targetRows >= 2, targetRows + " rows");
  const meta = txt(doc.getElementById("machine-metadata"));
  check(group, "machine metadata block filled", meta.length > 100, meta.length + " chars");
  // Folded in from the retired ?view=hot: the six-phase guide and the
  // all-phases per-ledger latency chart now live in the default report.
  const guide = report.querySelector(".phase-guide");
  check(group, "phase guide present", !!guide && /IngestLedger/.test(txt(guide)), guide ? "no IngestLedger" : "missing");
  const f42 = txt(doc.querySelector("#fig42-tv"));
  check(group, "all phases in fig42 table", /extract/.test(f42) && /commit \(fsync\)/.test(f42) && /apply/.test(f42), f42.slice(0, 160));
  if (kind === "synthetic") checkSyntheticQueries(doc, group, data);
}

/* A synthetic run that carries a queries block must render the section. The two
   cell generations render entirely different sections, so the shape decides
   which assertions apply. A run without queries must render none of it. */
function checkSyntheticQueries(doc, group, D) {
  const sec = doc.getElementById("queries");
  if (!D.queries) { check(group, "no queries section without a queries block", !sec, "section present"); return; }
  check(group, "queries section present", !!sec, "missing");
  if (!sec) return;
  const shape = queryShape(D);
  // Exact per-section counts, both generations: three charts either way, plus
  // the table figures each shape adds — and one more when the run carries the
  // E2E-budget family, which gets a table of its own rather than a column in
  // the SLA one.
  const figs = sec.querySelectorAll("figure.fig").length;
  const svgs = sec.querySelectorAll("svg").length;
  const wantFigs = (shape === "open" ? 5 : 4) + (hasE2EFamily(D) ? 1 : 0);
  check(group, `queries section emits exactly ${wantFigs} figures (${shape}-loop)`, figs === wantFigs, figs + " figures");
  check(group, "queries section emits exactly 3 charts", svgs === 3, svgs + " svgs");
  const picker = sec.querySelectorAll("#profile-filter .chunk-btn").length;
  check(group, "one profile button per unit", picker === D.dataset.unit_order.length, picker + " buttons");
  if (shape === "open") { checkOpenLoopQueries(doc, sec, group, D); return; }

  const tiers = Object.keys(D.queries);
  const unit = D.dataset.unit_order[0];
  const types = qTypes(D);
  const concs = Object.keys(qEntry(D)).filter((k) => /^c\d+$/.test(k));

  const p99tv = txt(doc.querySelector("#figq2-tv"));
  check(group, "every query type in the p99 table", types.every((t) => p99tv.includes(t)), p99tv.slice(0, 160));
  check(group, "both tiers in the p99 table", tiers.every((t) => p99tv.includes(t)), p99tv.slice(0, 160));
  check(group, "whole concurrency sweep in the p99 table", concs.every((c) => p99tv.includes(c + " p99")), p99tv.slice(0, 200));
  // Setup rows: `open` on both tiers, `evict` on cold only.
  const setup = txt(doc.getElementById("query-setup-table"));
  check(group, "setup table names open", /open/.test(setup), setup.slice(0, 160));
  const coldHasEvict = (((D.queries.cold || {})[unit] || {}).setup || {}).evict;
  if (coldHasEvict) check(group, "setup table names evict (cold)", /evict/.test(setup), setup.slice(0, 160));
  check(group, "events per page column filled", /events \/ page/.test(setup), setup.slice(0, 200));

  // Read-path target table: one row per type × tier, one column per level.
  const rows = sec.querySelectorAll("#query-target-table tr").length;
  check(group, "query target table populated", rows === types.length * tiers.length + 1, rows + " rows");
  const foot = txt(doc.getElementById("query-target-footnote"));
  check(group, "query target verdict states cells met", /\d+ cells meet the target|of \d+ cells meet the target/.test(foot), foot.slice(0, 160));

  // The verdict must reconcile with a hand count of the run's own breaches. A
  // cell is judged by its worst unit, so several units can be over inside one
  // cell — the table names the worst and flags the rest, and the footnote
  // carries both counts. Recompute both from the data and pin them.
  const thr = queryThr(D);
  let cellBreaches = 0, unitBreaches = 0;
  for (const qt of types) for (const tier of tiers) for (const cc of concs) {
    const over = D.dataset.unit_order.filter((u) => {
      const c = (((D.queries[tier] || {})[u] || {})[qt] || {})[cc];
      return c && c.p99.m > thr;
    }).length;
    if (over) { cellBreaches++; unitBreaches += over; }
  }
  const total = types.length * tiers.length * concs.length;
  const expected = cellBreaches === 0
    ? `All ${total} cells meet the target.`
    : `${total - cellBreaches} of ${total} cells meet the target; ${cellBreaches} exceed it`;
  check(group, "verdict count matches a hand count of breaching cells", foot.includes(expected), foot.slice(0, 200));
  if (unitBreaches > cellBreaches) {
    check(group, "verdict also reports the per-unit breach count", foot.includes(`(${unitBreaches} `), foot.slice(0, 200));
    check(group, "a multi-unit breach is marked '+N more' in its cell",
      /\+\d+ more/.test(txt(doc.getElementById("query-target-table"))), "no +N more marker");
  }
}

/* The open-loop (paced-RPS) queries section. Everything asserted here is
   recomputed from the run's own data — the fixture's breach count, its
   saturated rung and its rates all follow from the JSON, so a regenerated
   fixture with different numbers still gates the same properties. */
const ENDPOINT = { txhash: "getTransaction", txpage: "getTransactions", ledgers: "getLedgers", events: "getEvents" };
// Mirrors the viewer's rate formatting (docs/app.js fmtRps).
const fmtRps = (v) => v >= 100 ? Math.round(v).toLocaleString("en-US")
  : v >= 10 ? String(+v.toFixed(1)) : String(+v.toFixed(2));
const SATURATED = 0.95;

function checkOpenLoopQueries(doc, sec, group, D) {
  const secTxt = txt(sec);
  const tiers = Object.keys(D.queries);
  const units = D.dataset.unit_order;
  const types = qTypes(D);

  // Human endpoint names everywhere; the CSV's machine names nowhere.
  check(group, "endpoint display names rendered", types.every((t) => !ENDPOINT[t] || secTxt.includes(ENDPOINT[t])),
    types.map((t) => ENDPOINT[t]).join(","));
  const rawTokens = secTxt.match(/\btxhash\b|\btxpage\b|\br\d+(?:\.\d+)?\b/g) || [];
  check(group, "no raw CSV tokens in the rendered section", rawTokens.length === 0, rawTokens.slice(0, 6).join(","));

  // Verdict table: one row per endpoint × profile × tier, and one chip per
  // budget the run actually judged — the headline against this endpoint and
  // tier's own target, plus getTransaction's in-RPC budget as its own verdict.
  const tbl = doc.getElementById("query-verdict-table");
  check(group, "verdict table rendered", !!tbl, "missing");
  if (!tbl) return;
  let checks = 0, fails = 0, rows = 0, unfloored = 0;
  const judged = [], thresholds = [];
  for (const qt of types) for (const u of units) for (const tier of tiers) {
    const qout = ((D.queries[tier] || {})[u] || {})[qt];
    if (!qout) continue;
    const v = slaVerdict(qout);
    const key = judgedKey(qout);
    if (!key) continue;
    rows++;
    if (!v) unfloored++;
    const cell = qout[key] || {};
    judged.push({ qt, u, tier, qout, cell, v });
    // A row with no phase floor still gets a headline chip — the viewer judges
    // it on this endpoint and tier's own latency target, with no rate to meet
    // it at (docs/app.js openVerdictTable), so the hand count judges it the
    // same way rather than against one number for the whole run. The in-RPC
    // chip is counted here ONLY on the published runs that fold that budget
    // into the same verdict; where the run splits the families it is judged in
    // the E2E-budget table instead, and this table shows it as context.
    const p99 = v && v.p99_ns != null ? v.p99_ns : (cell.p99 || {}).m;
    const rowThr = v && v.threshold_ns != null ? v.threshold_ns : queryThr(D, qt, tier);
    thresholds.push(rowThr);
    for (const ok of [p99 == null ? null : (v ? v.pass : p99 <= rowThr),
      v && v.in_rpc ? v.in_rpc.pass : null]) {
      if (ok === null) continue;
      checks++; if (!ok) fails++;
    }
  }
  check(group, `verdict table has one row per endpoint × profile × tier (${rows})`,
    tbl.querySelectorAll("tr").length === rows + 1, tbl.querySelectorAll("tr").length + " rows");
  const chipFails = tbl.querySelectorAll(".q-fail").length;
  const chipPass = tbl.querySelectorAll(".q-pass").length;
  check(group, `breach chips match a hand count of the run's 1× verdicts (${fails})`,
    chipFails === fails, chipFails + " ▲ chips, expected " + fails);
  check(group, `passing chips match the hand count (${checks - fails})`,
    chipPass === checks - fails, chipPass + " ✓ chips");
  const foot = txt(doc.getElementById("query-verdict-footnote"));
  const wantFoot = fails === 0
    ? `All ${checks} checks pass at the 1× rate.`
    : `${checks - fails} of ${checks} checks pass at the 1× rate; ${fails} breach.`;
  check(group, "verdict footnote reconciles with the chips", foot.includes(wantFoot), foot.slice(0, 200));
  // Rows with no phase floor are named as such, in the table and in the footnote,
  // so a reader never mistakes a target-only pass for a load-model one.
  if (unfloored) {
    const noted = [...tbl.querySelectorAll("tr")].slice(1).filter((r) => /\(no floor\)/.test(txt(r))).length;
    check(group, `every unfloored row carries the (no floor) note (${unfloored})`, noted === unfloored, noted + " noted");
    check(group, "footnote states the unfloored row count",
      foot.includes(`${unfloored} of ${rows} rows carry no phase floor`), foot.slice(0, 260));
  }
  // The in-RPC budget and the mean page stay their own columns, never folded
  // into the headline verdict. The headline column heads no single threshold:
  // the SLA sets one per endpoint and tier, so every row prints its own.
  const heads = [...tbl.querySelectorAll("th")].map(txt);
  check(group, "in-RPC budget and mean page are their own columns",
    heads.some((h) => /In-RPC p99/.test(h)) && heads.some((h) => /Mean page/.test(h)), heads.join(" | "));
  // Every row prints the headline target it was judged against, in its own
  // marked note (.q-thr) so it cannot be confused with the in-RPC one beside
  // it. The rendered set has to be as varied as the data: as many distinct
  // targets on the page as the run's own thresholds imply.
  const verdictRows = [...tbl.querySelectorAll("tr")].slice(1);
  const printed = verdictRows.map((r) => [...r.querySelectorAll(".q-thr")].map(txt));
  check(group, `every row prints its own headline target (${verdictRows.length} rows)`,
    printed.length > 0 && printed.every((n) => n.length === 1 && /^\s*\(≤ \S+.*\)$/.test(n[0])),
    printed.filter((n) => n.length !== 1).length + " rows without exactly one target note");
  const wantDistinct = new Set(thresholds).size;
  const gotDistinct = new Set(printed.flat()).size;
  check(group, `the page prints as many distinct targets as the run defines (${wantDistinct})`,
    gotDistinct === wantDistinct, gotDistinct + " distinct printed");
  // The SLA shape is the point: a run keyed by endpoint and tier must not
  // collapse to one number on the page.
  if (queryCheck(D).targets_ns) {
    check(group, "a per-endpoint run prints more than one distinct target",
      wantDistinct > 1, wantDistinct + " distinct");
  }

  // Saturation at the top rung is legible: the ladder names the shortfall and
  // both the offered and served rates for the profile the picker opens on.
  const first = units[0];
  const sat = [];
  for (const qt of types) for (const tier of tiers) {
    const qout = ((D.queries[tier] || {})[first] || {})[qt] || {};
    for (const k of rateKeys(qout)) {
      const c = qout[k], target = c.target_rps != null ? c.target_rps : +k.slice(1);
      if (c.achieved_rps && c.achieved_rps.m < target * SATURATED) sat.push({ qt, tier, target, got: c.achieved_rps.m });
    }
  }
  const ladder = txt(doc.querySelector("#figq2-tv"));
  const marks = (ladder.match(/% under target/g) || []).length;
  check(group, `ladder flags every saturated rung of the opening profile (${sat.length})`, marks === sat.length, marks + " flagged");
  check(group, "ladder shows the saturated rung's offered and served rates",
    sat.length === 0 || sat.every((s) => ladder.includes(fmtRps(s.target) + " rps") && ladder.includes(fmtRps(s.got) + " rps")),
    ladder.slice(0, 200));
  // Every rung of every endpoint × tier is listed, labelled by multiplier —
  // off the floor the run names, else off the campaign ladder by position. A
  // leg that swept MORE rungs than the ladder has (getTransaction sweeping both
  // families with no floor to anchor them) is labelled by rate instead, so it
  // is not counted here.
  const ladderSteps = ((D.campaign || {}).query_load || {}).ladder;
  const anchored = (j) => j.v || (Array.isArray(ladderSteps) && rateKeys(j.qout).length === ladderSteps.length);
  const rungRows = judged.filter((j) => j.u === first && anchored(j)).length;
  check(group, `ladder table lists 1× for every anchored endpoint × tier (${rungRows})`,
    (ladder.match(/1×/g) || []).length >= rungRows, ladder.slice(0, 160));

  // Latency detail at 1×: the scheduled distribution plus the in-RPC column.
  const detail = txt(doc.querySelector("#figq4-tv"));
  check(group, "1× latency table carries p50/p90/p99/max + in-RPC p99",
    /p50/.test(detail) && /p99/.test(detail) && /In-RPC p99/.test(detail), detail.slice(0, 160));
  // The E2E-budget probe is a SECOND requirement, measured at a different rate
  // and judged on a different number. It must have a table of its own, and the
  // SLA table above must not chip getTransaction's in-RPC time as if the two
  // were one verdict.
  const e2eTbl = doc.getElementById("query-e2e-table");
  const wantE2E = e2eEntries(D);
  check(group, wantE2E.length ? "E2E-budget table rendered" : "no E2E-budget table without the family",
    !!e2eTbl === wantE2E.length > 0, e2eTbl ? "present" : "missing");
  if (e2eTbl) {
    const e2eRows = [...e2eTbl.querySelectorAll("tr")].slice(1);
    check(group, `E2E table has one row per profile × tier with a probe verdict (${wantE2E.length})`,
      e2eRows.length === wantE2E.length, e2eRows.length + " rows");
    const wantFails = wantE2E.filter((e) => !e2eVerdict(e.qout).in_rpc.pass).length;
    check(group, `E2E breach chips match a hand count of the probe verdicts (${wantFails})`,
      e2eTbl.querySelectorAll(".e2e-fail").length === wantFails,
      e2eTbl.querySelectorAll(".e2e-fail").length + " ▲ chips");
    check(group, "E2E table judges the in-RPC p99 and reports the scheduled one",
      [...e2eTbl.querySelectorAll("th")].some((h) => /In-RPC p99/.test(txt(h)))
        && e2eRows.every((r) => /not judged/.test(txt(r))),
      [...e2eTbl.querySelectorAll("th")].map(txt).join(" | "));
    // The families never share a row: the SLA table shows getTransaction's
    // in-RPC time without a verdict chip beside it.
    // "getTransactions" starts with "getTransaction", so the label is matched
    // up to its separator rather than as a substring.
    const slaTxhashCells = [...tbl.querySelectorAll("tr")].slice(1)
      .filter((r) => /^getTransaction · /.test(txt(r.querySelector("td"))));
    check(group, "the SLA table chips no in-RPC budget once the families are split",
      slaTxhashCells.length > 0 && slaTxhashCells.every((r) => /judged as the E2E budget/.test(txt(r))),
      slaTxhashCells.map((r) => txt(r)).join(" | ").slice(0, 200));
    // Different requirement, different rate: the offered rates must not all be
    // the SLA floor, or the split would be cosmetic.
    const slaRates = new Set(wantE2E.map((e) => slaVerdict(e.qout).target_rps));
    const probeRates = new Set(wantE2E.map((e) => e2eVerdict(e.qout).target_rps));
    check(group, "the probe rate is read from the demand model, not the SLA floor",
      [...probeRates].some((r) => !slaRates.has(r)),
      `sla=[${[...slaRates].join(",")}] probe=[${[...probeRates].join(",")}]`);
  }

  // Setup/event-scan table still works off `setup`.
  const setup = txt(doc.getElementById("query-setup-table"));
  check(group, "setup table names open", /open/.test(setup), setup.slice(0, 160));
  check(group, "setup table carries the 1× event columns", /mean page 1×/.test(setup), setup.slice(0, 200));
}

/* Expected phase-goal verdict, derived from the run data the same way the
   viewer derives it (docs/app.js + docs/summary.js): count the units whose
   hot ingest_total p99 median is within the paced phase's ingest_p99_target_ns.
   Returns null when the run carries no phase or no goal, so callers skip the
   check rather than pin a verdict that the data does not define. */
function expectedGoal(D) {
  const camp = D && D.campaign;
  if (!camp || camp.phase == null || !Array.isArray(camp.phase_targets)) return null;
  const sel = camp.phase_targets.find(p => p.phase === camp.phase);
  const goalNs = sel && sel.ingest_p99_target_ns;
  if (!goalNs) return null;
  const hot = D.ingest_hot || {};
  const units = Object.keys(hot).filter(u => hot[u].driver && hot[u].driver.ingest_total && hot[u].driver.ingest_total.p99);
  if (!units.length) return null;
  const pass = units.filter(u => hot[u].driver.ingest_total.p99.m <= goalNs).length;
  return { pass, n: units.length, met: pass === units.length, goalNs };
}

/* Assert a goal banner's headline matches the verdict the run data implies:
   "<pass> / <n>" plus MEETS GOAL when every unit clears the goal, else MISS. */
function checkGoalBanner(group, bannerTxt, g, what) {
  const want = g.met ? "MEETS GOAL" : "MISS";
  const other = g.met ? "MISS" : "MEETS GOAL";
  check(group, `${what}: ${g.pass} / ${g.n} ${want} (from run data)`,
    new RegExp(`${g.pass} / ${g.n}`).test(bannerTxt) && new RegExp(want).test(bannerTxt) && !new RegExp(other).test(bannerTxt),
    bannerTxt.slice(0, 140));
}

function checkSanity(kind, doc, group, D) {
  const report = doc.getElementById("report");
  // Synthetic wraps its two verdict banners (phase goal + block model) in a
  // .banners container; pubnet has a single standalone .banner.
  const banner = txt(report.querySelector(".banners") || report.querySelector(".banner"));
  if (kind === "pubnet") {
    check(group, "banner 24 / 24 PASS", /24 \/ 24/.test(banner) && /PASS/.test(banner), banner.slice(0, 60));
    const f31 = txt(doc.querySelector("#fig31-tv"));
    check(group, "chunk 6345 cold wall ≈ 61.7 s", /61\.\d s/.test(f31), f31.slice(0, 120));
    const ticks = (txt(doc.getElementById("target-table")).match(/✓/g) || []).length;
    check(group, "24 passing target cells", ticks === 24, ticks + " ticks");
  } else if (kind === "synthetic") {
    const phase = D && D.campaign && D.campaign.phase;
    if (phase != null) {
      // Phase runs lead with the phase-goal verdict (ingest p99 vs target); the
      // block-model keep-up is shown as a secondary line, never the headline.
      // Both verdicts are two-state — the goal is MEETS GOAL or MISS, the keep-up
      // is KEEPS UP or OVER INTERVAL — and a run legitimately lands in any
      // combination, so assert a verdict is present, not which one it is. The
      // per-run blocks below pin the actual state for each committed run.
      check(group, "banner shows both goal + keep-up verdicts",
        /(MEETS GOAL|MISS)/.test(banner) && /(KEEPS UP|OVER INTERVAL)/.test(banner), banner.slice(0, 140));
      // The headline count and verdict follow from the data, not from the run's
      // phase: a Phase 2 run may miss on SAC (the 2026-07 runs) or clear all
      // three profiles (the 2026-08 runs), and both must pass this gate.
      const g = expectedGoal(D);
      if (g) checkGoalBanner(group, banner, g, `phase ${phase} headline`);
    } else {
      check(group, "banner 3 / 3 KEEPS UP", /3 \/ 3/.test(banner) && /KEEPS UP/.test(banner), banner.slice(0, 60));
    }
    const tgt = txt(doc.getElementById("target-table"));
    const f42 = txt(doc.querySelector("#fig42-tv"));
    // Sanity values are per-run: the 2026-07-16 apply-tail fix pulled sac's
    // ingest_total p99 back under one 600 ms interval (was 920 ms on 2026-07-15).
    if (group === "synthetic-2026-07-15") {
      check(group, "sac p99 920 ms flagged over interval", /920 ms ▲/.test(tgt), tgt.slice(0, 160));
      check(group, "sac end-to-end p50 ≈ 144 ms", /14[34](\.\d)? ms/.test(f42), f42.slice(0, 120));
    } else if (group === "synthetic-2026-07-16-apply-fix") {
      check(group, "sac p99 305 ms within interval (apply-tail fix)", /305 ms/.test(tgt) && !/305 ms ▲/.test(tgt), tgt.slice(0, 200));
      check(group, "sac end-to-end p50 ≈ 122 ms (apply-tail fix)", /122 ms/.test(f42), f42.slice(0, 120));
    } else if (group === "phase3-c6id8xl-c48a55c6-20260724T214257Z") {
      // Phase 3 paces at exactly the 600 ms interval, so every profile sustains it
      // with 1.0× headroom and none is credited as keeping up: the keep-up verdict
      // is 0 / 3 OVER INTERVAL, not KEEPS UP. sac is the profile whose tail also
      // overruns a single interval outright.
      check(group, "phase 3 keep-up verdict: 0 / 3 OVER INTERVAL (not KEEPS UP)",
        /0 \/ 3/.test(banner) && /3 OVER INTERVAL/.test(banner) && !/KEEPS UP/.test(banner), banner.slice(0, 140));
      check(group, "sac p99 749 ms flagged 1.2× interval", /749 ms ▲ 1\.2× interval/.test(tgt), tgt.slice(0, 200));
    } else if (group === "phase3-m6id2xl-c48a55c6-20260724T000926Z") {
      // Same shape on the smaller box, with a longer sac tail (858 ms vs 749 ms).
      check(group, "phase 3 keep-up verdict: 0 / 3 OVER INTERVAL (not KEEPS UP)",
        /0 \/ 3/.test(banner) && /3 OVER INTERVAL/.test(banner) && !/KEEPS UP/.test(banner), banner.slice(0, 140));
      check(group, "sac p99 858 ms flagged 1.4× interval", /858 ms ▲ 1\.4× interval/.test(tgt), tgt.slice(0, 200));
    }
  }
}

/* ---------------- main ---------------- */
const manifest = JSON.parse(fs.readFileSync(path.join(DOCS, "runs", "index.json"), "utf8"));
if (!Array.isArray(manifest.runs) || manifest.runs.length === 0) {
  console.error("manifest has no runs");
  process.exit(1);
}
const runJSON = (id) => JSON.parse(fs.readFileSync(path.join(DOCS, "runs", id + ".json"), "utf8"));

for (const run of manifest.runs) {
  const group = run.id;
  console.log(`\n=== ${run.id} (${run.kind}) ===`);
  const { window, errors } = await loadViewer(`?run=${run.id}`);
  const doc = window.document;
  check(group, "zero JS/console errors", errors.length === 0, errors.join(" | ") || "");
  check(group, "deep link kept in URL", window.location.search === `?run=${run.id}`, window.location.search);
  const h1 = txt(doc.querySelector("#report h1"));
  check(group, "run name rendered as h1", h1.includes(run.name.slice(0, 20)), h1);
  const options = [...doc.querySelectorAll("#run-select option")].map((o) => o.value);
  check(group, "dropdown lists every manifest run", manifest.runs.every((r) => options.includes(r.id)), options.join(","));
  // Report toolbar: run navigation shown, global pages tucked away on the listing.
  check(group, "run navigation visible on a report", !doc.getElementById("run-select").hidden
    && !doc.getElementById("all-runs-link").hidden && !doc.getElementById("summary-link").hidden, "hidden");
  check(group, "summary link carries the run id",
    doc.getElementById("summary-link").getAttribute("href") === `summary.html?run=${encodeURIComponent(run.id)}`,
    doc.getElementById("summary-link").getAttribute("href"));
  checkKind(run.kind, doc, group, runJSON(run.id));
  checkSanity(run.kind, doc, group, runJSON(run.id));
  window.close();
}

/* ---------------- summary-page section helpers ---------------- */
/* The summary's two conditional sections, decided from the run JSON exactly as
   summary.js decides them: the go / no-go budget section needs a phase with a
   declared E2E budget, and the query section needs at least one leg carrying a
   1x verdict. */
const QT_ENDPOINT = { txhash: "getTransaction", txpage: "getTransactions", ledgers: "getLedgers", events: "getEvents" };
const SUMMARY_QT_ORDER = ["txhash", "txpage", "ledgers", "events"];
const summaryTrim = (x) => x >= 100 ? x.toFixed(0) : x >= 10 ? x.toFixed(1) : x.toFixed(2);
const summaryFmtNs = (ns) => ns >= 1e9 ? summaryTrim(ns / 1e9) + " s"
  : ns >= 1e6 ? summaryTrim(ns / 1e6) + " ms"
  : ns >= 1e3 ? summaryTrim(ns / 1e3) + " µs" : Math.round(ns) + " ns";
const summaryFmtRps = (v) => (v >= 100 ? Math.round(v).toLocaleString("en-US")
  : v >= 10 ? String(+v.toFixed(1)) : String(+v.toFixed(2))) + " rps";
const summaryProfileName = (D, unit) => {
  if ((D.campaign || {}).phase == null) return unit;
  const u = String(unit).toLowerCase();
  if (u.startsWith("sac")) return "SAC transfers";
  if (u.includes("token")) return "OZ token transfers";
  if (u.includes("soroswap")) return "Soroswap swaps";
  return unit;
};
const SUMMARY_SECTION_LABEL = {
  budget: "E2E budget",
  method: "Methodology",
  goal: "Ingestion goal",
  phases: "Ingestion phases",
  queries: "Query latency",
  machine: "Machine metadata",
};
/* Every (tier, unit, endpoint) leg with a 1x verdict, grouped by tier — the
   rows the summary's per-tier table must carry, one for one. */
function summaryQueryRows(D) {
  const out = {};
  for (const tier of ["hot", "cold"]) {
    const t = (D.queries || {})[tier];
    if (!t) continue;
    const rows = [];
    for (const u of D.dataset.unit_order) {
      for (const [qt, qout] of Object.entries(t[u] || {})) {
        if (qt === "setup") continue;
        const v = slaVerdict(qout);
        if (v && v.p99_ns != null) rows.push({ unit: u, qt, v, e2e: e2eVerdict(qout) });
      }
    }
    if (rows.length) out[tier] = rows;
  }
  return out;
}
/* The budget section's own gate: summary.js needs a matched phase carrying an
   ingestion target, a block time and an E2E budget (the fixed network and
   handler constants come from targets.json, which every run reads). */
function summaryHasBudget(D) {
  const targets = JSON.parse(fs.readFileSync(path.join(DOCS, "targets.json"), "utf8"));
  const phase = (D.campaign || {}).phase;
  if (phase == null) return false;
  const p = (targets.phases || []).find((x) => x.phase === phase);
  const fx = targets.fixed_estimates || {};
  if (!p || !p.block_time_ns || !p.e2e_budget_ns) return false;
  if (!fx.network_rtt_ns || !fx.send_tx_p99_ns || !fx.get_tx_p99_ns) return false;
  // Phase 2 omits its ingestion target; summary.js derives it the same way.
  const derived = p.ingest_p99_target_ns != null ? p.ingest_p99_target_ns
    : p.e2e_budget_ns - (p.block_count || 2) * p.block_time_ns
      - (Math.floor(3 * fx.network_rtt_ns / 2) + fx.send_tx_p99_ns + fx.get_tx_p99_ns);
  return derived > 0 && (D.ingest_hot ? Object.keys(D.ingest_hot).length > 0 : false);
}
/* The summary's own section assertions, shared by the manifest runs and the
   generated fixtures. `text` is the rendered page with #queries cut out — the
   only place the tier words are allowed to appear. */
function checkSummarySections(sdoc, group, D) {
  const sreport = sdoc.getElementById("report");
  const wantBudget = summaryHasBudget(D);
  const QR = summaryQueryRows(D);
  // getTransaction is measured only when a live-store leg recorded an in-RPC
  // p99 — from the E2E-budget probe where the run has one, else folded into the
  // single legacy verdict. Otherwise its allocation stands.
  const measuredGet = (QR.hot || []).some((r) => r.qt === "txhash" && (r.e2e ? r.e2e.in_rpc : r.v.in_rpc));
  const probedGet = (QR.hot || []).some((r) => r.qt === "txhash" && r.e2e);
  const qTiers = ["hot", "cold"].filter((t) => QR[t]);
  const wantQueries = qTiers.length > 0;

  // Vocabulary: the tier words are section-scoped, everything else is banned
  // outright. Clone the report and drop #queries before reading the text.
  const clone = sreport.cloneNode(true);
  const qsec = clone.querySelector("#queries");
  if (qsec) qsec.remove();
  const outside = txt(clone);
  const banned = outside.match(/\b(h[o]t|pod|platform|team)\b/i);
  check(group, "no internal vocabulary outside the query section", !banned, banned ? banned[0] : "");
  const everywhere = txt(sreport).match(/\b(pod|platform|team)\b/i);
  check(group, "no pod/platform/team vocabulary anywhere", !everywhere, everywhere ? everywhere[0] : "");

  // Section order, with the two conditional sections in their declared slots
  // and machine metadata always last.
  const want = ["budget", "method", "goal", "phases", "queries", "machine"]
    .filter((id) => (id === "budget" ? wantBudget : id === "queries" ? wantQueries : true));
  const secs = [...sreport.querySelectorAll("section")].map((s) => s.id);
  check(group, `section order ${want.join("→")}`, secs.join(",") === want.join(","), secs.join(","));
  const nav = sdoc.getElementById("summary-nav");
  const navLinks = nav ? [...nav.querySelectorAll(".summary-nav-link")] : [];
  check(group, "summary section nav is labelled and visible",
    !!nav && nav.getAttribute("aria-label") === "Summary sections" && !nav.hidden,
    nav ? `${nav.getAttribute("aria-label")} hidden=${nav.hidden}` : "missing");
  check(group, "summary nav order, hrefs, labels, and numbers match rendered sections",
    navLinks.length === want.length && navLinks.every((link, i) =>
      link.getAttribute("href") === `#${want[i]}`
      && txt(link.querySelector(".summary-nav-num")) === String(i + 1).padStart(2, "0")
      && txt(link.querySelector(".summary-nav-text")) === SUMMARY_SECTION_LABEL[want[i]]),
    navLinks.map(txt).join(" | "));
  check(group, "summary nav has no dead section targets",
    navLinks.every(link => !!sdoc.getElementById(link.getAttribute("href").slice(1))),
    navLinks.map(link => link.getAttribute("href")).join(","));
  const current = navLinks.filter(link => link.getAttribute("aria-current") === "location");
  check(group, "summary nav initializes one accessible active link",
    current.length === 1 && current[0] === navLinks[0], current.map(txt).join(" | "));
  if (navLinks.length > 1) {
    nav.addEventListener("click", event => event.preventDefault(), { capture: true, once: true });
    navLinks[1].dispatchEvent(new sdoc.defaultView.MouseEvent("click", { bubbles: true, cancelable: true }));
    check(group, "summary nav click updates aria-current without observer support",
      navLinks[1].getAttribute("aria-current") === "location"
        && navLinks.filter(link => link.hasAttribute("aria-current")).length === 1,
      navLinks.filter(link => link.hasAttribute("aria-current")).map(txt).join(" | "));
  }

  if (wantBudget) {
    const bud = sdoc.getElementById("budget");
    check(group, "budget figure lives inside the budget section", !!(bud && bud.querySelector("#fig11")), bud ? "fig11 elsewhere" : "no section");
    const lead = txt(bud && bud.querySelector(".banner .lead"));
    check(group, "budget verdict is GO or NO-GO", /\b(GO|NO-GO)\b/.test(lead) && /of the .* budget/.test(lead), lead.slice(0, 140));
    const tiles = bud ? bud.querySelectorAll(".slice-tile").length : 0;
    check(group, "three slice tiles", tiles === 3, tiles + " tiles");
    const tileTxt = txt(bud && bud.querySelector(".slice-tiles"));
    check(group, "tiles name the three slices and their allocations",
      /sendTransaction/.test(tileTxt) && /ingestion/.test(tileTxt) && /getTransaction/.test(tileTxt)
      && (tileTxt.match(/allocation/g) || []).length === 3, tileTxt.slice(0, 160));
    const f11t = txt(sdoc.querySelector("#fig11-tv"));
    const getRow = f11t.split("getTransaction round trip")[1] || "";
    check(group, measuredGet ? "budget table: getTransaction allocated → measured" : "budget table: getTransaction stays an estimate",
      measuredGet ? /allocated → measured/.test(getRow) : !/getTransaction[^|]*allocated → measured/.test(getRow.split("end-to-end")[0] || ""),
      getRow.slice(0, 160));
  } else {
    check(group, "no budget section without a phase E2E budget", !sdoc.getElementById("budget"), "section present");
  }

  if (!wantQueries) {
    check(group, "no query section without 1x query verdicts", !sdoc.getElementById("queries"), "section present");
    return;
  }
  const qsecLive = sdoc.getElementById("queries");
  const phase = (D.campaign || {}).phase;
  const heading = txt(qsecLive.querySelector(".sec-head h2"));
  check(group, "query heading names the phase load",
    heading === `Query latency at ${phase == null ? "target" : `Phase ${phase}`} load`, heading);
  const intro = txt(qsecLive.querySelector(":scope > .sec-intro"));
  check(group, "query intro describes client-observed p99 without a read-path goal",
    /Each endpoint ran/.test(intro) && /client-observed p99 latency/.test(intro) && !/\bgoal\b/i.test(intro),
    intro);
  const subs = qsecLive.querySelectorAll("h3.method-sub").length;
  check(group, `one subsection per tier (${qTiers.length})`, subs === qTiers.length, subs + " subheads");
  const tables = qsecLive.querySelectorAll("table").length;
  check(group, `one table per tier (${qTiers.length})`, tables === qTiers.length, tables + " tables");
  const units = D.dataset.unit_order;
  for (const tier of qTiers) {
    const t = sdoc.getElementById(`q-table-${tier}`);
    const rows = t ? [...t.querySelectorAll("tbody tr")] : [];
    check(group, `${tier} matrix has four endpoint rows`, rows.length === SUMMARY_QT_ORDER.length, rows.length + " rows");
    const headers = t ? [...t.querySelectorAll("thead th")].map(txt) : [];
    const wantHeaders = ["Endpoint", ...units.map((u) => summaryProfileName(D, u))];
    check(group, `${tier} matrix columns follow workload order`,
      headers.join("|") === wantHeaders.join("|"), headers.join(" | "));
    const cells = new Map(QR[tier].map((r) => [`${r.qt}\0${r.unit}`, r.v]));
    for (const [ri, qt] of SUMMARY_QT_ORDER.entries()) {
      const row = rows[ri];
      const rowHead = txt(row && row.querySelector("th[scope=row]"));
      check(group, `${tier} matrix row ${ri + 1} is ${QT_ENDPOINT[qt]}`,
        rowHead === QT_ENDPOINT[qt], rowHead);
      const data = row ? [...row.querySelectorAll("td")] : [];
      check(group, `${tier} ${QT_ENDPOINT[qt]} has one cell per workload`,
        data.length === units.length, data.length + " cells");
      for (const [ui, unit] of units.entries()) {
        const v = cells.get(`${qt}\0${unit}`);
        const cell = data[ui];
        if (!v) {
          check(group, `${tier} ${qt}/${unit}: missing combination is an em dash`,
            txt(cell) === "—" && !!cell.querySelector(".q-missing"), txt(cell));
          continue;
        }
        const latency = cell.querySelector(".q-latency");
        const rate = cell.querySelector(".q-rate");
        check(group, `${tier} ${qt}/${unit}: p99 and rate match the source`,
          latency && latency.tagName === "STRONG" && txt(latency) === summaryFmtNs(v.p99_ns)
          && txt(rate) === summaryFmtRps(v.target_rps),
          txt(cell));
      }
    }
  }
  const qTxt = txt(qsecLive);
  check(group, "no raw query-cell tokens in the section", !/\b(txhash|txpage)\b/.test(qTxt), (qTxt.match(/\b(txhash|txpage)\b/) || [""])[0]);
  check(group, "query section has no read-goal verdict UI",
    !/read-path goal|within the goal|Goal ≤/i.test(qTxt)
      && !qsecLive.querySelector(".chip, .q-count"),
    qTxt.slice(0, 180));
  const tierNotes = [...qsecLive.querySelectorAll("h3.method-sub + .sec-intro")].map(txt);
  check(group, "tier descriptions use the concise store wording",
    tierNotes.includes("Recent ledgers are served from the live store with a warm page cache.")
      && (!qTiers.includes("cold") || tierNotes.includes("Archived ledgers are served from immutable files and event indexes after eviction from the page cache.")),
    tierNotes.join(" | "));
  if (D.run_id === "fixture-rps-phase3") {
    const hotCell = sdoc.querySelector("#q-table-hot tbody tr:nth-child(1) td:nth-child(2)");
    const coldCell = sdoc.querySelector("#q-table-cold tbody tr:nth-child(2) td:nth-child(2)");
    check(group, "representative hot and cold matrix values render",
      txt(hotCell && hotCell.querySelector(".q-latency")) === "12.0 ms"
        && txt(hotCell && hotCell.querySelector(".q-rate")) === "300 rps"
        && txt(coldCell && coldCell.querySelector(".q-latency")) === "640 ms"
        && txt(coldCell && coldCell.querySelector(".q-rate")) === "75 rps",
      `${txt(hotCell)} | ${txt(coldCell)}`);
  }
  if (wantBudget && measuredGet) {
    // A run with the probe family names the probe leg and its rate in the note.
    const want = new RegExp("^The end-to-end budget in §01 uses the highest measured getTransaction p99"
      + (probedGet ? ", taken from its end-to-end probe leg at [\\d,.]+ rps" : "") + "\\.$");
    check(group, "hot matrix links its highest getTransaction p99 to the E2E budget",
      want.test(txt(qsecLive.querySelector(".q-foot"))), txt(qsecLive.querySelector(".q-foot")));
  }
}


/* ---------------- generated fixture runs (tests/smoke/fixtures) ---------------- */
/* Shapes the converter can emit that no committed run carries yet — today the
   open-loop (paced-RPS) queries block, in three variations. `make smoke`
   regenerates them with gen-fixtures.py; they are never committed and never
   reach docs/runs/, which is the published site. Served through a fetch overlay:
   the run file itself, plus a manifest with its entry appended so the viewer
   resolves ?run=<id> rather than silently falling back to the first real run.

   Everything the shared open-loop assertions cannot express — a fixture is built
   for one property — lives here, keyed by run id. */
const FIXTURE_CHECKS = {
  "fixture-rps-phase3": (doc, group, D) => {
    const es = qEntries(D);
    check(group, "every endpoint × profile × tier carries an SLA verdict",
      es.length > 0 && es.every((e) => e.qout.verdict_sla),
      es.filter((e) => !e.qout.verdict_sla).length + " without a verdict");
    // The second family covers getTransaction and nothing else.
    const probe = es.filter((e) => e.qout.verdict_e2e);
    check(group, `only getTransaction carries an E2E-budget verdict (${probe.length})`,
      probe.length > 0 && probe.every((e) => e.qt === "txhash"),
      probe.map((e) => e.qt).join(","));
    check(group, "the run names its goal phase", D.campaign.phase === 3, String(D.campaign.phase));
  },

  /* No phase floor: the run is paced off-phase and names no query_phase, so the
     converter emits r-cells with no verdict at all. The section must still
     render and must judge every row on that endpoint and tier's own latency
     target — never on one run-wide number — saying so. */
  "fixture-rps-unfloored": (doc, group, D) => {
    const es = qEntries(D);
    check(group, "no run phase and no 1× floor anywhere",
      D.campaign.phase == null && es.every((e) => !slaVerdict(e.qout) && !e2eVerdict(e.qout)),
      "phase " + D.campaign.phase);
    const tbl = doc.getElementById("query-verdict-table");
    const rows = [...tbl.querySelectorAll("tr")].slice(1);
    check(group, "every verdict row is noted (no floor)",
      rows.length > 0 && rows.every((r) => /\(no floor\)/.test(txt(r))), rows.length + " rows");
    check(group, "no row claims a 1× floor", !rows.some((r) => /\(1× floor\)/.test(txt(r))), "floor note present");
    // One headline chip per row, each judged against the target its own
    // endpoint carries in its own tier — the check's targets_ns table, the same
    // number a verdict would have stated had the run earned one.
    const want = es.filter((e) => judgedKey(e.qout)).length;
    const chips = tbl.querySelectorAll(".q-pass, .q-fail").length;
    check(group, `one latency-target chip per row, no budget chips (${want})`, chips === want, chips + " chips");
    const over = es.filter((e) => {
      const k = judgedKey(e.qout);
      return k && e.qout[k].p99.m > queryThr(D, e.qt, e.tier);
    }).length;
    check(group, `breach chips match the per-endpoint target hand count (${over})`,
      tbl.querySelectorAll(".q-fail").length === over, tbl.querySelectorAll(".q-fail").length + " ▲ chips");
    // The regression this fixture exists to catch: a run with no verdicts must
    // still show each endpoint's own target, not one fallback number.
    const printed = new Set([...tbl.querySelectorAll(".q-thr")].map(txt));
    check(group, "unfloored rows print more than one distinct target",
      printed.size > 1, [...printed].join(" | "));
    // The three open-loop figures still draw, chart and table view alike.
    for (const id of ["figq2", "figq3", "figq4"]) {
      check(group, `${id} draws a chart and a table view`,
        !!doc.querySelector(`#${id}-body svg`) && txt(doc.querySelector(`#${id}-tv`)).length > 20, "empty");
    }
  },

  /* The regression for queryGrid probing one unit: the FIRST profile has hot
     query legs only. The section must survive, keyed off the union of every
     unit's legs, with the missing rows simply absent. */
  "fixture-rps-partial": (doc, group, D) => {
    const [first, second] = D.dataset.unit_order;
    check(group, "fixture really is hot-only on the first profile",
      !D.queries.cold[first] && !!D.queries.cold[second] && !!D.queries.hot[first],
      "cold tier carries " + Object.keys(D.queries.cold).join(","));
    check(group, "queries section survives the first profile's missing cold legs",
      !!doc.getElementById("queries"), "section gone");
    // Row labels are "<endpoint> · <profile> · <tier>"; read the name cell alone
    // so the numbers in the next columns do not run into the tier word.
    const labels = [...doc.querySelectorAll("#query-verdict-table tr")].slice(1)
      .map((r) => txt(r.querySelector("td")));
    const profilesOn = (tier) => [...new Set(labels.filter((s) => s.endsWith(" · " + tier))
      .map((s) => s.split(" · ")[1]))];
    const cold = profilesOn("cold"), hot = profilesOn("hot");
    check(group, "both profiles have hot rows, only the second has cold rows",
      hot.length === 2 && cold.length === 1 && cold[0] === hot[1],
      `cold=[${cold.join(",")}] hot=[${hot.join(",")}]`);
    const want = qEntries(D).filter((e) => judgedKey(e.qout)).length;
    check(group, `verdict table drops the missing legs and nothing else (${want} rows)`,
      labels.length === want, labels.length + " rows");
    // The picker opens on the hot-only profile: its ladder must draw hot lanes
    // and list no cold ones.
    const ladder = txt(doc.querySelector("#figq2-tv"));
    check(group, "opening profile's ladder lists hot lanes only",
      /hot/.test(ladder) && !/cold/.test(ladder), ladder.slice(0, 200));
    check(group, "opening profile still draws all three charts",
      ["figq2", "figq3", "figq4"].every((id) => !!doc.querySelector(`#${id}-body svg`)), "a chart is missing");
  },
};
const fixtureFiles = fs.existsSync(FIXTURES)
  ? fs.readdirSync(FIXTURES).filter((f) => f.endsWith(".json")).sort() : [];
check("fixtures", "generated fixture runs present (run gen-fixtures.py)", fixtureFiles.length > 0, fixtureFiles.length + " files");
for (const file of fixtureFiles) {
  const raw = fs.readFileSync(path.join(FIXTURES, file), "utf8");
  const D = JSON.parse(raw);
  const group = `fixture:${D.run_id}`;
  console.log(`\n=== ${D.run_id} (${D.dataset.kind}, generated fixture) ===`);
  const entry = { id: D.run_id, name: D.run_name, date: D.run_date, kind: D.dataset.kind, path: `runs/${D.run_id}.json` };
  const overlay = {
    [`runs/${D.run_id}.json`]: raw,
    "runs/index.json": JSON.stringify({ ...manifest, runs: [...manifest.runs, entry] }),
  };
  const { window, errors } = await loadViewer(`?run=${D.run_id}`, { fetch: makeFetch(overlay) });
  const doc = window.document;
  const report = doc.getElementById("report");
  check(group, "zero JS/console errors", errors.length === 0, errors.join(" | ") || "");
  check(group, "masthead rendered", !!report.querySelector(".masthead"), "missing");
  // Guards the manifest overlay: without the entry the viewer would render the
  // first real run under the fixture's deep link and everything below would pass.
  const h1 = txt(doc.querySelector("#report h1"));
  check(group, "the fixture run is what rendered", h1.includes(D.run_name.slice(0, 20)), h1);
  check(group, "no error box", !report.querySelector(".error-box"), txt(report.querySelector(".error-box")));
  const sections = report.querySelectorAll("section").length;
  check(group, "the other sections still render", sections >= 5, sections + " sections");
  // The fixture is only worth loading if it really carries the new shape.
  check(group, "fixture carries r-cells + the campaign ladder",
    !!(D.campaign.query_load && qEntries(D).some((e) => rateKeys(e.qout).length)), "not the RPS shape");
  check(group, "fixture detected as the open-loop shape", queryShape(D) === "open", queryShape(D));
  checkSyntheticQueries(doc, group, D);
  const extra = FIXTURE_CHECKS[D.run_id];
  check(group, "fixture has its own assertions", !!extra, "no FIXTURE_CHECKS entry for " + D.run_id);
  if (extra) extra(doc, group, D);
  window.close();

  // The same fixture through the stakeholder summary. The committed runs carry
  // older machine-metadata dumps, so the fixture is the only run whose dump is
  // what the current harness writes — this is where a config-echo line leaking
  // internal vocabulary into the public page is caught.
  const sm = await loadViewer(`?run=${D.run_id}`, { page: "summary.html", script: "summary.js", fetch: makeFetch(overlay) });
  const sdoc = sm.window.document;
  const sreport = sdoc.getElementById("report");
  check(group, "summary: zero JS/console errors", sm.errors.length === 0, sm.errors.join(" | ") || "");
  check(group, "summary: machine metadata block filled", txt(sdoc.getElementById("machine-metadata")).length > 40, "empty");
  checkSummarySections(sdoc, group + " summary", D);
  sm.window.close();
}

/* ---------------- run index (bare index.html, no ?run=) ---------------- */
/* The landing page lists every manifest run: facets view by default, table
   view behind the toggle (?view=table). Listing metadata (phase/machine/
   hostname/commit/branch) is optional per entry, so assert per-entry. */
{
  const group = "run-index";
  console.log(`\n=== index.html (run-index listing, facets default) ===`);
  const { window, errors } = await loadViewer("");
  const doc = window.document;
  check(group, "zero JS/console errors", errors.length === 0, errors.join(" | ") || "");
  check(group, "no ?run injected on the landing page", window.location.search === "", window.location.search);
  check(group, "facets view is the default", !!doc.querySelector(".idx-layout"), "missing .idx-layout");
  const rows = doc.querySelectorAll(".idx-row").length;
  check(group, "one row per manifest run", rows === manifest.runs.length, rows + " rows");
  const toggles = [...doc.querySelectorAll(".idx-toggle button")].map(txt);
  check(group, "view toggle offers Facets + Table", toggles.join(",") === "Facets,Table", toggles.join(","));
  check(group, "Facets toggle marked active", doc.querySelector(".idx-toggle button.on")?.getAttribute("data-view") === "facets", "not facets");
  const facetHeads = [...doc.querySelectorAll(".idx-facet-h")].map(txt);
  check(group, "phase + machine facet groups present", facetHeads.includes("Phase") && facetHeads.includes("Machine"), facetHeads.join(","));
  const enriched = manifest.runs.filter((r) => r.commit);
  const ghLinks = [...doc.querySelectorAll(".idx-row a.idx-gh")].map((a) => a.href);
  check(group, "commit links target the stellar-rpc repo", enriched.length === 0
    || ghLinks.some((h) => h.includes("github.com/stellar/stellar-rpc/commit/")), ghLinks.slice(0, 2).join(","));
  const nameHrefs = [...doc.querySelectorAll("a.idx-name")].map((a) => a.getAttribute("href"));
  check(group, "run links land on the summary", nameHrefs.length > 0 && nameHrefs.every((h) => h.startsWith("summary.html?run=")), nameHrefs.slice(0, 2).join(","));
  // Verdict line: one per entry that carries phase + ingest_p99 (optional
  // metadata — entries without it must render without one).
  const withVerdict = manifest.runs.filter((r) => r.phase != null && Array.isArray(r.ingest_p99) && r.ingest_p99.length);
  const vlines = [...doc.querySelectorAll(".idx-row .idx-verdict")];
  check(group, "verdict line on phase-aware rows only", vlines.length === withVerdict.length, `${vlines.length} lines, ${withVerdict.length} eligible`);
  check(group, "verdict reads count · goal · worst", withVerdict.length === 0
    || vlines.every((el) => /[✓▲✕] \d+\/\d+ pass · goal .+ · worst: .+ p99 .+ \(.+\)/.test(txt(el))), txt(vlines[0]));
  const striped = doc.querySelectorAll(".idx-row.idx-v-pass, .idx-row.idx-v-miss, .idx-row.idx-v-fail").length;
  check(group, "verdict stripe class on the same rows", striped === withVerdict.length, striped + " striped");
  // Listing toolbar: global pages only — the run navigation stays hidden.
  check(group, "run selector hidden on the listing", doc.getElementById("run-select").hidden, "visible");
  check(group, "all-runs + summary links hidden on the listing",
    doc.getElementById("all-runs-link").hidden && doc.getElementById("summary-link").hidden, "visible");
  check(group, "latency + tx-submission links visible on the listing",
    !doc.getElementById("latency-link").hidden && !doc.getElementById("txsub-link").hidden, "hidden");
  window.close();
}
{
  const group = "run-index table";
  console.log(`\n=== index.html?view=table (run-index listing, table view) ===`);
  const { window, errors } = await loadViewer("?view=table");
  const doc = window.document;
  check(group, "zero JS/console errors", errors.length === 0, errors.join(" | ") || "");
  const tbl = doc.querySelector(".idx-table");
  check(group, "table view rendered", !!tbl, "missing .idx-table");
  const trs = doc.querySelectorAll(".idx-table tbody tr").length;
  check(group, "one table row per manifest run", trs === manifest.runs.length, trs + " rows");
  const dates = [...doc.querySelectorAll(".idx-table tbody tr td:last-child")].map(txt);
  const sortedDesc = dates.every((d, i) => i === 0 || dates[i - 1] >= d);
  check(group, "default sort is date descending", sortedDesc, dates.join(","));
  const headers = [...doc.querySelectorAll(".idx-table thead th")].map(txt);
  check(group, "all listing columns present",
    ["Run", "Phase", "Kind", "Machine", "Host", "Commit", "Branch"].every((h) => headers.some((x) => x.startsWith(h))), headers.join(","));
  window.close();
}

/* ---------------- unit-label display in the default report ---------------- */
/* The OZ-token assertion needs a run whose unit_meta carries labels (from
   --unit-facts); campaign runs may not have them. Guards the unit-facts label
   path in the synthetic renderer. */
const synthRun = manifest.runs.find((r) => r.kind === "synthetic"
  && Object.values(runJSON(r.id).dataset.unit_meta || {}).some((m) => m && m.label));
if (synthRun) {
  const group = "synthetic labels";
  console.log(`\n=== ${synthRun.id} (unit labels) ===`);
  const { window, errors } = await loadViewer(`?run=${synthRun.id}`);
  const doc = window.document;
  check(group, "zero JS/console errors", errors.length === 0, errors.join(" | ") || "");
  check(group, "OZ token label shown", txt(doc.getElementById("report")).includes("OZ token"), "missing");
  window.close();
}

/* Guards the numeric-id label prefix (T6) in the pubnet renderer. */
const pubnetRun = manifest.runs.find((r) => r.kind === "pubnet");
if (pubnetRun) {
  const group = "pubnet labels";
  console.log(`\n=== ${pubnetRun.id} (unit labels) ===`);
  const { window, errors } = await loadViewer(`?run=${pubnetRun.id}`);
  const doc = window.document;
  check(group, "zero JS/console errors", errors.length === 0, errors.join(" | ") || "");
  check(group, "Chunk 3000 label present", txt(doc.getElementById("report")).includes("Chunk 3000"), "missing");
  window.close();
}

/* ---------------- phase targets (campaign run carrying phase_targets) ---------------- */
const phaseRun = manifest.runs.find((r) => {
  const c = runJSON(r.id).campaign || {};
  return Array.isArray(c.phase_targets) && c.phase_targets.length > 0;
});
if (phaseRun) {
  const D = runJSON(phaseRun.id);
  const matched = D.campaign.phase;

  {
    const group = "phase default";
    console.log(`\n=== ${phaseRun.id} (phase table, default selection) ===`);
    const { window, errors } = await loadViewer(`?run=${phaseRun.id}`);
    const doc = window.document;
    check(group, "zero JS/console errors", errors.length === 0, errors.join(" | ") || "");
    const tbl = doc.querySelector("#phase-block table.phase-table");
    check(group, "phase table rendered", !!tbl, "missing");
    const tblTxt = txt(tbl);
    check(group, "all three phases in table", /Phase 1/.test(tblTxt) && /Phase 2/.test(tblTxt) && /Phase 3/.test(tblTxt), tblTxt.slice(0, 100));
    check(group, "phase 2 ingest-slice target derived from e2e budget (405 ms)", /405 ms/.test(tblTxt), tblTxt.slice(0, 100));
    const selTh = doc.querySelector("#phase-block th.ph-sel");
    check(group, `matched phase (${matched}) highlighted`, !!selTh && txt(selTh).includes(`Phase ${matched}`), selTh ? txt(selTh) : "missing");
    check(group, "matched phase badged 'this run'", /this run/.test(txt(doc.getElementById("phase-block"))), "missing");
    check(group, "no caveat at default (matched) selection", !doc.querySelector(".phase-caveat"), "caveat present");
    const svgTxt = txt(doc.querySelector("#fig42-body"));
    check(group, "budget line at Phase 1 block time (2 s)", /2 s — Phase 1 block time/.test(svgTxt), svgTxt.slice(0, 200));
    check(group, "ingest target line at 905 ms", /905 ms — Phase 1 ingest target \(p99\)/.test(svgTxt), svgTxt.slice(0, 200));
    const readout = txt(doc.getElementById("ingest-target-readout"));
    check(group, "pass/miss readout vs Phase 1 target", /vs Phase 1 target 905 ms/.test(readout) && /(PASS|MISS)/.test(readout), readout.slice(0, 140));
    // Tier-1: peak memory + pacing now surface in the DEFAULT report, not just ?view=hot.
    check(group, "peak-memory figure rendered", !!doc.querySelector("#fig21-body svg"), "missing");
    const memTv = txt(doc.querySelector("#fig21-tv"));
    check(group, "peak RSS shows % of box RAM", /% of box RAM/.test(memTv) && /GiB/.test(memTv), memTv.slice(0, 120));
    check(group, "sac-6000 cold peak ≈ 70% of RAM", /70 %/.test(memTv), memTv.slice(0, 200));
    check(group, "pacing figure in default report", !!doc.querySelector("#fig44-body svg"), "missing");
    check(group, "pacing readout names close interval", /close interval/.test(txt(doc.getElementById("pace-readout-full"))), "missing");
    window.close();
  }

  {
    const group = "phase switch";
    console.log(`\n=== ${phaseRun.id} (?phase=3) ===`);
    const { window, errors } = await loadViewer(`?run=${phaseRun.id}&phase=3`);
    const doc = window.document;
    check(group, "zero JS/console errors", errors.length === 0, errors.join(" | ") || "");
    const selTh = doc.querySelector("#phase-block th.ph-sel");
    check(group, "Phase 3 column highlighted", !!selTh && txt(selTh).includes("Phase 3"), selTh ? txt(selTh) : "missing");
    const caveat = txt(doc.querySelector(".phase-caveat"));
    check(group, "caveat names Phase 3 and the actual pace", /Viewing against Phase 3 targets/.test(caveat) && /2 s close interval/.test(caveat), caveat.slice(0, 160));
    const svgTxt = txt(doc.querySelector("#fig42-body"));
    check(group, "budget line re-based to 600 ms", /600 ms — Phase 3 block time/.test(svgTxt), svgTxt.slice(0, 200));
    check(group, "ingest target line re-based to 105 ms", /105 ms — Phase 3 ingest target \(p99\)/.test(svgTxt), svgTxt.slice(0, 200));
    const readout = txt(doc.getElementById("ingest-target-readout"));
    check(group, "readout vs Phase 3 target 105 ms", /vs Phase 3 target 105 ms/.test(readout), readout.slice(0, 140));
    // The pacing figure's budget is always the run's ACTUAL close interval —
    // never re-based when another phase is selected.
    const paceReadout = txt(doc.getElementById("pace-readout-full"));
    check(group, "pace budget = actual 2 s close interval (not re-based by phase)", /2\.00 s close interval/.test(paceReadout), paceReadout.slice(0, 160));
    window.close();
  }
}

/* ---------------- stakeholder summary (summary.html) ---------------- */
/* The public-facing summary renders the same run JSONs, cut to the ingestion
   story: goal banner (targets.json is the source; Phase 2's ingestion target is
   derived), methodology (dataset table + pacing schematic),
   the headline p99 chart with two separate reference lines, the six-phase
   per-ledger figure, and machine metadata as the bottom-most section. */
for (const run of manifest.runs) {
  const group = `summary:${run.id}`;
  console.log(`\n=== summary.html ?run=${run.id} (${run.kind}) ===`);
  const { window, errors } = await loadViewer(`?run=${run.id}`, { page: "summary.html", script: "summary.js" });
  const doc = window.document;
  const report = doc.getElementById("report");
  check(group, "zero JS/console errors", errors.length === 0, errors.join(" | ") || "");
  check(group, "masthead present", !!report.querySelector(".masthead"), "missing");
  check(group, "deep link kept in URL", window.location.search === `?run=${run.id}`, window.location.search);
  const full = doc.getElementById("full-report");
  check(group, "full-report link targets internal viewer", !!full && full.getAttribute("href") === `index.html?run=${run.id}`, full ? full.getAttribute("href") : "missing");
  const allRuns = doc.getElementById("all-runs-link");
  check(group, "all-runs link back to the run index", !!allRuns && allRuns.getAttribute("href") === "index.html", allRuns ? allRuns.getAttribute("href") : "missing");
  // Audience vocabulary + section order, including the two conditional
  // sections (go / no-go, query latency).
  const text = txt(report);
  checkSummarySections(doc, group, runJSON(run.id));
  // The goal banner leads the goal section; the conditional budget section
  // above it carries a verdict banner of its own.
  const goalSection = report.querySelector("#goal");
  const goalBanner = goalSection && goalSection.querySelector(".banner");
  const goalFigure = goalSection && goalSection.querySelector("#fig31");
  const bannerTxt = txt(goalBanner);
  check(group, "goal banner has a verdict", /(MEETS GOAL|MISS)/.test(bannerTxt), bannerTxt.slice(0, 120));
  check(group, "goal banner precedes the ingestion figure",
    !!(goalBanner && goalFigure && (goalBanner.compareDocumentPosition(goalFigure) & window.Node.DOCUMENT_POSITION_FOLLOWING)),
    "banner missing or misplaced");
  check(group, "at-a-glance section and phase goals table removed",
    !doc.getElementById("glance") && !doc.getElementById("phase-block"), "legacy summary content present");
  const phNo = (runJSON(run.id).campaign || {}).phase;
  // Fig 1.1: the E2E budget bar — the goal composition vs the same trip with
  // this run's measured slices, against the phase's declared E2E budget.
  if (doc.getElementById("budget")) {
    const f11 = doc.querySelector("#fig11-body svg");
    check(group, "budget figure rendered", !!f11, "missing");
    check(group, "budget ceiling line drawn", !!(f11 && f11.querySelector("line.refline-budget")), "missing");
    const f11t = txt(doc.querySelector("#fig11-tv"));
    check(group, "budget table: all six slices + derived end-to-end",
      /network — submission request/.test(f11t) && /sendTransaction/.test(f11t) && /Stellar Core consensus/.test(f11t)
      && /ingestion/.test(f11t) && /network — getTransaction round trip/.test(f11t) && /getTransaction/.test(f11t)
      && /end-to-end \(derived\)/.test(f11t),
      f11t.slice(0, 160));
    check(group, "budget verdict rendered (under/over/on budget)", /(under|over|on budget)/.test(f11t), f11t.slice(0, 160));
  }
  // Dataset table: every cell renders real data — no empty or "—" cells.
  const dsCells = [...doc.querySelectorAll("#dataset-table td")].map(td => txt(td));
  const badCell = c => !c || c === "—" || /NaN|Infinity/.test(c);
  check(group, "dataset table has no empty or — cells", dsCells.length > 0 && !dsCells.some(badCell), dsCells.filter(badCell).length + " bad of " + dsCells.length);
  check(group, "dataset table names the workloads", /SAC transfers/.test(txt(doc.getElementById("dataset-table"))), "missing");
  // Test-data provenance: sizes + source come from dataset-sizes.json; the
  // run's own bundle URI (source_uri, legacy source_gcs) is the raw-results
  // dir, linked from §05.
  const dsTblTxt = txt(doc.getElementById("dataset-table"));
  check(group, "avg ledger size column filled (MiB)", /Avg ledger size/i.test(dsTblTxt) && /MiB/.test(dsTblTxt), dsTblTxt.slice(0, 120));
  check(group, "test-data source links the synthetic dataset", /synthetic-ledgers/.test(txt(doc.getElementById("source-data"))), "missing");
  check(group, "raw-results link in §05", /(gs|s3):\/\/\S*results\//.test(txt(doc.getElementById("raw-results"))), "missing");
  check(group, "pacing diagram rendered", !!doc.querySelector("#fig21-body svg"), "missing");
  // Headline figure: bars plus two visually separate reference lines.
  const f31 = doc.querySelector("#fig31-body svg");
  check(group, "headline figure rendered", !!f31, "missing");
  check(group, "ingestion-target refline (dashed) drawn", !!(f31 && f31.querySelector("line.refline")), "missing");
  check(group, "block-time refline (solid) drawn", !!(f31 && f31.querySelector("line.refline-block")), "missing");
  // Fig 4.1: share-of-ingestion-time composition (additive per-phase totals).
  const f41 = txt(doc.querySelector("#fig41-tv"));
  check(group, "share table has all six phases with % shares", /extract/.test(f41) && /commit \(fsync\)/.test(f41) && /apply/.test(f41) && /%/.test(f41), f41.slice(0, 160));
  check(group, "share figure rendered", !!doc.querySelector("#fig41-body svg"), "missing");
  // Fig 4.2: per-phase percentile detail behind a phase picker (linear axis).
  const pickBtns = [...doc.querySelectorAll("#fig42-picker button")];
  check(group, "phase picker: 7 options, 'end to end' selected", pickBtns.length === 7 && txt(doc.querySelector("#fig42-picker button.sel")) === "end to end", pickBtns.map(b => txt(b)).join(","));
  check(group, "detail figure rendered on a linear scale", /linear scale/.test(txt(doc.querySelector("#fig42-body"))), "missing");
  const f42 = txt(doc.querySelector("#fig42-tv"));
  check(group, "all phases in fig 4.2 table", /end to end/.test(f42) && /extract/.test(f42) && /commit \(fsync\)/.test(f42) && /apply/.test(f42), f42.slice(0, 160));
  const meta = txt(doc.getElementById("machine-metadata"));
  check(group, "machine metadata block filled", meta.length > 100, meta.length + " chars");
  // Per-run sanity values.
  // The goal banner's count and verdict follow from the run data (see
  // expectedGoal); the run id only selects the fixed target and per-run figures.
  const g = expectedGoal(runJSON(run.id));
  if (g) checkGoalBanner(group, bannerTxt, g, `phase ${phNo} banner`);
  if (run.id.startsWith("phase2-")) {
    check(group, "Phase 2 ingestion target derived (405 ms)", /405 ms/.test(bannerTxt), bannerTxt.slice(0, 200));
    // SAC's p99 is a per-box number, not a per-phase one — the two 2026-07 Phase 2
    // runs miss the 405 ms goal, at 604 ms on the m6id.2xlarge and 456 ms on the
    // faster c6id.8xlarge. Key each figure to its own run id.
    if (run.id === "phase2-m6id2xl-c48a55c6-20260723T035541Z") {
      check(group, "SAC p99 ≈ 604 ms surfaced", /60[34](\.\d)? ms/.test(text), text.slice(0, 120));
    } else if (run.id === "phase2-c6id8xl-c48a55c6-20260724T000724Z") {
      check(group, "SAC p99 ≈ 456 ms surfaced", /45[56](\.\d)? ms/.test(text), text.slice(0, 120));
    }
  }
  window.close();
}

/* ---------------- tx-submission interim page (tx-submission.html) ---------------- */
/* Renders the harvest summaries under docs/txsub/ (verbatim external contract,
   not run JSONs): headline handler table, mean split bars, testnet table. */
{
  const group = "tx-submission";
  console.log(`\n=== tx-submission.html ===`);
  const { window, errors } = await loadViewer("", { page: "tx-submission.html", script: "txsub.js" });
  const doc = window.document;
  check(group, "zero JS/console errors", errors.length === 0, errors.join(" | ") || "");
  const headRows = doc.querySelectorAll("#headline-table tbody tr").length;
  check(group, "three headline profile rows", headRows === 3, headRows + " rows");
  const splitBars = doc.querySelectorAll(".ts-splitbar").length;
  check(group, "three split bars", splitBars === 3, splitBars + " bars");
  // Sanity value: the standalone soroswap handler p99 (4564515 ns) renders as 4.56 ms.
  const soroswapRow = [...doc.querySelectorAll("#headline-table tbody tr")].map(txt).find((t) => /Soroswap/.test(t));
  check(group, "soroswap handler p99 renders as 4.56 ms", !!soroswapRow && /4\.56 ms/.test(soroswapRow), soroswapRow || "row missing");
  window.close();
}

console.log(`\nSMOKE SUMMARY: ${pass} passed, ${fail} failed (${manifest.runs.length} runs)`);
if (fail) {
  console.log("Failures:");
  failures.forEach((f) => console.log("  - " + f));
  process.exit(1);
}
