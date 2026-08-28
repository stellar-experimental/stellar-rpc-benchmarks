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
const qEntry = (D) => {
  const tier = Object.keys(D.queries || {})[0];
  const unit = D.dataset.unit_order[0];
  const e = ((D.queries || {})[tier] || {})[unit] || {};
  const qt = Object.keys(e).filter((k) => k !== "setup")[0];
  return qt ? e[qt] : {};
};
const rateKeys = (qout) => Object.keys(qout).filter((k) => RATE_KEY.test(k)).sort((a, b) => +a.slice(1) - +b.slice(1));
function queryShape(D) {
  if (!D.queries) return null;
  const e = qEntry(D);
  if (Object.keys(e).some((k) => CONC_KEY.test(k))) return "closed";
  return rateKeys(e).length ? "open" : null;
}

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
  // the table figures each shape adds.
  const figs = sec.querySelectorAll("figure.fig").length;
  const svgs = sec.querySelectorAll("svg").length;
  const wantFigs = shape === "open" ? 5 : 4;
  check(group, `queries section emits exactly ${wantFigs} figures (${shape}-loop)`, figs === wantFigs, figs + " figures");
  check(group, "queries section emits exactly 3 charts", svgs === 3, svgs + " svgs");
  const picker = sec.querySelectorAll("#profile-filter .chunk-btn").length;
  check(group, "one profile button per unit", picker === D.dataset.unit_order.length, picker + " buttons");
  if (shape === "open") { checkOpenLoopQueries(doc, sec, group, D); return; }

  const tiers = Object.keys(D.queries);
  const unit = D.dataset.unit_order[0];
  const types = Object.keys(D.queries[tiers[0]][unit]).filter((k) => k !== "setup");
  const concs = Object.keys(D.queries[tiers[0]][unit][types[0]]).filter((k) => /^c\d+$/.test(k));

  const p99tv = txt(doc.querySelector("#figq2-tv"));
  check(group, "every query type in the p99 table", types.every((t) => p99tv.includes(t)), p99tv.slice(0, 160));
  check(group, "both tiers in the p99 table", tiers.every((t) => p99tv.includes(t)), p99tv.slice(0, 160));
  check(group, "whole concurrency sweep in the p99 table", concs.every((c) => p99tv.includes(c + " p99")), p99tv.slice(0, 200));
  // Setup rows: `open` on both tiers, `evict` on cold only.
  const setup = txt(doc.getElementById("query-setup-table"));
  check(group, "setup table names open", /open/.test(setup), setup.slice(0, 160));
  const coldHasEvict = tiers.includes("cold") && D.queries.cold[unit].setup && D.queries.cold[unit].setup.evict;
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
  const thr = ((D.checks_all || [D.checks || {}]).find((c) => c && c.applies_to === "queries") || {}).threshold_ns || 500e6;
  let cellBreaches = 0, unitBreaches = 0;
  for (const qt of types) for (const tier of tiers) for (const cc of concs) {
    const over = D.dataset.unit_order.filter((u) => D.queries[tier][u][qt][cc].p99.m > thr).length;
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
  const types = Object.keys(D.queries[tiers[0]][units[0]]).filter((k) => k !== "setup");

  // Human endpoint names everywhere; the CSV's machine names nowhere.
  check(group, "endpoint display names rendered", types.every((t) => !ENDPOINT[t] || secTxt.includes(ENDPOINT[t])),
    types.map((t) => ENDPOINT[t]).join(","));
  const rawTokens = secTxt.match(/\btxhash\b|\btxpage\b|\br\d+(?:\.\d+)?\b/g) || [];
  check(group, "no raw CSV tokens in the rendered section", rawTokens.length === 0, rawTokens.slice(0, 6).join(","));

  // Verdict table: one row per endpoint × profile × tier, and one chip per
  // budget the run actually judged — headline, plus getTransaction's in-RPC
  // budget and getEvents' page budget as separate verdicts.
  const tbl = doc.getElementById("query-verdict-table");
  check(group, "verdict table rendered", !!tbl, "missing");
  if (!tbl) return;
  let checks = 0, fails = 0, rows = 0;
  const judged = [];
  for (const qt of types) for (const u of units) for (const tier of tiers) {
    const qout = ((D.queries[tier] || {})[u] || {})[qt];
    if (!qout) continue;
    const v = qout.verdict_1x;
    const keys = rateKeys(qout);
    if (!keys.length) continue;
    rows++;
    const key = v && v.rate && qout[v.rate] ? v.rate : keys[Math.floor((keys.length - 1) / 2)];
    judged.push({ qt, u, tier, cell: qout[key], v });
    for (const ok of [v ? v.pass : null, v && v.in_rpc ? v.in_rpc.pass : null,
      v && v.page_budget ? v.page_budget.pass : null]) {
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
  // The three budgets are separate columns, never folded into one verdict.
  const heads = [...tbl.querySelectorAll("th")].map(txt);
  check(group, "in-RPC and mean-page budgets are their own columns",
    heads.some((h) => /In-RPC p99/.test(h)) && heads.some((h) => /Mean page/.test(h)), heads.join(" | "));

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
  // Every rung of every endpoint × tier is listed, labelled by multiplier.
  const rungRows = judged.filter((j) => j.u === first).length;
  check(group, "ladder table lists 1× for every endpoint × tier of the profile",
    (ladder.match(/1×/g) || []).length >= rungRows, ladder.slice(0, 160));

  // Latency detail at 1×: the scheduled distribution plus the in-RPC column.
  const detail = txt(doc.querySelector("#figq4-tv"));
  check(group, "1× latency table carries p50/p90/p99/max + in-RPC p99",
    /p50/.test(detail) && /p99/.test(detail) && /In-RPC p99/.test(detail), detail.slice(0, 160));
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

/* ---------------- generated fixture runs (tests/smoke/fixtures) ---------------- */
/* Shapes the converter can emit that no committed run carries yet — today the
   open-loop (paced-RPS) queries block. `make smoke` regenerates them with
   gen-fixtures.py; they are never committed and never reach docs/runs/, which
   is the published site. Served through a fetch overlay: the run file itself,
   plus a manifest with its entry appended so the viewer resolves ?run=<id>
   rather than silently falling back to the first real run. */
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
  const someQt = Object.values(Object.values(D.queries.hot)[0]).find((v) => v && v.verdict_1x);
  check(group, "fixture carries r-cells + a 1x verdict", !!(D.campaign.query_load && someQt), "not the RPS shape");
  check(group, "fixture detected as the open-loop shape", queryShape(D) === "open", queryShape(D));
  checkSyntheticQueries(doc, group, D);
  window.close();
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
   story: goal banner + phase-goal table (targets.json is the source; Phase 2's
   ingestion target is derived), methodology (dataset table + pacing schematic),
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
  // Audience vocabulary: the page never uses the internal tier/org words.
  const text = txt(report);
  const banned = text.match(/\b(h[o]t|pod|platform|team)\b/i);
  check(group, "no internal vocabulary in rendered page", !banned, banned ? banned[0] : "");
  // Section order: machine metadata is the bottom-most section.
  const secs = [...report.querySelectorAll("section")].map(s => s.id);
  check(group, "section order glance→method→goal→phases→machine", secs.join(",") === "glance,method,goal,phases,machine", secs.join(","));
  const bannerTxt = txt(report.querySelector(".banner"));
  check(group, "goal banner has a verdict", /(MEETS GOAL|MISS)/.test(bannerTxt), bannerTxt.slice(0, 120));
  // Goals table: cut to the phase the run was paced for; the two benchmark
  // non-inputs (Orgs, Retention) are not shown.
  const phNo = (runJSON(run.id).campaign || {}).phase;
  const phTbl = txt(doc.querySelector("#phase-block table"));
  if (phNo != null) {
    const others = [1, 2, 3].filter(p => p !== phNo);
    check(group, `phase table shows only Phase ${phNo}, badged 'this run'`,
      new RegExp(`Phase ${phNo}`).test(phTbl) && /this run/.test(phTbl) && others.every(p => !new RegExp(`Phase ${p}\\b`).test(phTbl)),
      phTbl.slice(0, 100));
  } else {
    check(group, "phase table lists all three phases", /Phase 1/.test(phTbl) && /Phase 2/.test(phTbl) && /Phase 3/.test(phTbl), phTbl.slice(0, 100));
  }
  check(group, "no Orgs or Retention rows", !/Orgs|Retention/.test(phTbl), phTbl.slice(0, 100));
  // Fig 1.1: the E2E budget bar — the goal composition vs the same trip with
  // this run's measured ingestion, against the phase's declared E2E budget.
  if (phNo != null) {
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
    check(group, "Phase 2 ingestion target derived (405 ms)", /405 ms/.test(phTbl), phTbl.slice(0, 200));
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
