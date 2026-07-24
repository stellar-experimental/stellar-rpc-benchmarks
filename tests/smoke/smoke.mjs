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
const ORIGIN = "http://viewer.local";

const MIME = { ".json": "application/json", ".js": "text/javascript", ".css": "text/css", ".html": "text/html" };

function fetchShim(input) {
  const u = new URL(input, ORIGIN + "/");
  const file = path.join(DOCS, decodeURIComponent(u.pathname));
  return new Promise((resolve) => {
    fs.readFile(file, "utf8", (err, body) => {
      if (err) {
        resolve({ ok: false, status: 404, headers: { get: () => null }, text: async () => "", json: async () => { throw new Error("404 " + u.pathname); } });
        return;
      }
      resolve({
        ok: true, status: 200,
        headers: { get: (h) => (h.toLowerCase() === "content-type" ? MIME[path.extname(file)] || "text/plain" : null) },
        text: async () => body,
        json: async () => JSON.parse(body),
      });
    });
  });
}

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
  window.fetch = fetchShim;
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

function checkKind(kind, doc, group, reps) {
  const base = EXPECT[kind];
  if (!base) { check(group, `known kind (${kind})`, false, "no expectations defined"); return; }
  const exp = { ...base };
  // Single-rep runs hide the run-to-run variance section (one section, one
  // figure, one SVG fewer).
  if (kind === "synthetic" && reps === 1) { exp.sections -= 1; exp.minFigures -= 1; exp.minSvgs -= 1; }
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
      check(group, "banner shows both goal + keep-up verdicts",
        /(MEETS GOAL|MISS)/.test(banner) && /KEEPS UP/.test(banner), banner.slice(0, 140));
      if (group.startsWith("phase2-")) {
        // SAC's ~604 ms ingest p99 misses the 400 ms Phase 2 goal: the headline
        // must read 2 / 3 with a MISS, not an all-clear 3 / 3.
        check(group, "phase 2 headline is 2 / 3 MISS (not all-clear)",
          /2 \/ 3/.test(banner) && /MISS/.test(banner) && !/MEETS GOAL/.test(banner), banner.slice(0, 140));
      }
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
  checkKind(run.kind, doc, group, runJSON(run.id).campaign.reps);
  checkSanity(run.kind, doc, group, runJSON(run.id));
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
    check(group, "phase 2 ingest-slice target derived from e2e budget (400 ms)", /400 ms/.test(tblTxt), tblTxt.slice(0, 100));
    const selTh = doc.querySelector("#phase-block th.ph-sel");
    check(group, `matched phase (${matched}) highlighted`, !!selTh && txt(selTh).includes(`Phase ${matched}`), selTh ? txt(selTh) : "missing");
    check(group, "matched phase badged 'this run'", /this run/.test(txt(doc.getElementById("phase-block"))), "missing");
    check(group, "no caveat at default (matched) selection", !doc.querySelector(".phase-caveat"), "caveat present");
    const svgTxt = txt(doc.querySelector("#fig42-body"));
    check(group, "budget line at Phase 1 block time (2 s)", /2 s — Phase 1 block time/.test(svgTxt), svgTxt.slice(0, 200));
    check(group, "ingest target line at 900 ms", /900 ms — Phase 1 ingest target \(p99\)/.test(svgTxt), svgTxt.slice(0, 200));
    const readout = txt(doc.getElementById("ingest-target-readout"));
    check(group, "pass/miss readout vs Phase 1 target", /vs Phase 1 target 900 ms/.test(readout) && /(PASS|MISS)/.test(readout), readout.slice(0, 140));
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
    check(group, "ingest target line re-based to 100 ms", /100 ms — Phase 3 ingest target \(p99\)/.test(svgTxt), svgTxt.slice(0, 200));
    const readout = txt(doc.getElementById("ingest-target-readout"));
    check(group, "readout vs Phase 3 target 100 ms", /vs Phase 3 target 100 ms/.test(readout), readout.slice(0, 140));
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
    check(group, "budget table: all four slices + derived end-to-end",
      /Stellar Core consensus/.test(f11t) && /tx submission/.test(f11t) && /ingestion/.test(f11t) && /query/.test(f11t) && /end-to-end \(derived\)/.test(f11t),
      f11t.slice(0, 160));
    check(group, "budget verdict rendered (under/over/on budget)", /(under|over|on budget)/.test(f11t), f11t.slice(0, 160));
  }
  // Dataset table: every cell renders real data — no empty or "—" cells.
  const dsCells = [...doc.querySelectorAll("#dataset-table td")].map(td => txt(td));
  const badCell = c => !c || c === "—" || /NaN|Infinity/.test(c);
  check(group, "dataset table has no empty or — cells", dsCells.length > 0 && !dsCells.some(badCell), dsCells.filter(badCell).length + " bad of " + dsCells.length);
  check(group, "dataset table names the workloads", /SAC transfers/.test(txt(doc.getElementById("dataset-table"))), "missing");
  // Test-data provenance: sizes + source come from dataset-sizes.json; the
  // run's own source_gcs is the raw-results dir, linked from §05.
  const dsTblTxt = txt(doc.getElementById("dataset-table"));
  check(group, "avg ledger size column filled (MiB)", /Avg ledger size/i.test(dsTblTxt) && /MiB/.test(dsTblTxt), dsTblTxt.slice(0, 120));
  check(group, "test-data source links the synthetic dataset", /synthetic-ledgers/.test(txt(doc.getElementById("source-data"))), "missing");
  check(group, "raw-results link in §05", /results\//.test(txt(doc.getElementById("raw-results"))), "missing");
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
  if (run.id.startsWith("phase2-")) {
    check(group, "Phase 2 ingestion target derived (400 ms)", /400 ms/.test(phTbl), phTbl.slice(0, 200));
    check(group, "phase 2 banner: 2 / 3 with a MISS (SAC over 400 ms)", /2 \/ 3/.test(bannerTxt) && /MISS/.test(bannerTxt), bannerTxt.slice(0, 140));
    check(group, "SAC p99 ≈ 604 ms surfaced", /60[34](\.\d)? ms/.test(text), text.slice(0, 120));
  } else if (run.id.startsWith("phase1-")) {
    check(group, "phase 1 banner: 3 / 3 MEETS GOAL", /3 \/ 3/.test(bannerTxt) && /MEETS GOAL/.test(bannerTxt), bannerTxt.slice(0, 140));
  }
  window.close();
}

console.log(`\nSMOKE SUMMARY: ${pass} passed, ${fail} failed (${manifest.runs.length} runs)`);
if (fail) {
  console.log("Failures:");
  failures.forEach((f) => console.log("  - " + f));
  process.exit(1);
}
