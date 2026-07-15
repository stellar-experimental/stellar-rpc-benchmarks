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

async function loadViewer(query) {
  const errors = [];
  const vc = new VirtualConsole();
  vc.on("jsdomError", (e) => errors.push("jsdomError: " + (e.detail?.message || e.message || e)));
  vc.on("error", (...a) => errors.push("console.error: " + a.join(" ")));

  const html = fs.readFileSync(path.join(DOCS, "index.html"), "utf8");
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

  // index.html references app.js via <script src>, which jsdom does not fetch
  // without `resources: "usable"` (an HTTP loader we deliberately avoid) — inject it.
  const app = fs.readFileSync(path.join(DOCS, "app.js"), "utf8");
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

function checkKind(kind, doc, group) {
  const exp = EXPECT[kind];
  if (!exp) { check(group, `known kind (${kind})`, false, "no expectations defined"); return; }
  const report = doc.getElementById("report");
  const sections = report.querySelectorAll("section").length;
  check(group, `${exp.sections} sections rendered`, sections === exp.sections, sections);
  for (const id of exp.requiredIds) check(group, `section #${id} present`, !!doc.getElementById(id), "missing");
  const figs = report.querySelectorAll("figure.fig").length;
  check(group, `>= ${exp.minFigures} figures`, figs >= exp.minFigures, figs);
  const svgs = report.querySelectorAll("svg").length;
  check(group, `>= ${exp.minSvgs} SVG charts`, svgs >= exp.minSvgs, svgs);
  const targetRows = report.querySelectorAll("#target-table tr").length;
  check(group, "target/keep-up table populated", targetRows >= 2, targetRows + " rows");
  const meta = txt(doc.getElementById("machine-metadata"));
  check(group, "machine metadata block filled", meta.length > 100, meta.length + " chars");
}

function checkSanity(kind, doc, group) {
  const report = doc.getElementById("report");
  const banner = txt(report.querySelector(".banner"));
  if (kind === "pubnet") {
    check(group, "banner 24 / 24 PASS", /24 \/ 24/.test(banner) && /PASS/.test(banner), banner.slice(0, 60));
    const f31 = txt(doc.querySelector("#fig31-tv"));
    check(group, "chunk 6345 cold wall ≈ 61.7 s", /61\.\d s/.test(f31), f31.slice(0, 120));
    const ticks = (txt(doc.getElementById("target-table")).match(/✓/g) || []).length;
    check(group, "24 passing target cells", ticks === 24, ticks + " ticks");
  } else if (kind === "synthetic") {
    check(group, "banner 3 / 3 KEEPS UP", /3 \/ 3/.test(banner) && /KEEPS UP/.test(banner), banner.slice(0, 60));
    const tgt = txt(doc.getElementById("target-table"));
    check(group, "sac p99 920 ms flagged over interval", /920 ms ▲/.test(tgt), tgt.slice(0, 160));
    const f42 = txt(doc.querySelector("#fig42-tv"));
    check(group, "sac end-to-end p50 ≈ 144 ms", /14[34](\.\d)? ms/.test(f42), f42.slice(0, 120));
  }
}

/* ---------------- main ---------------- */
const manifest = JSON.parse(fs.readFileSync(path.join(DOCS, "runs", "index.json"), "utf8"));
if (!Array.isArray(manifest.runs) || manifest.runs.length === 0) {
  console.error("manifest has no runs");
  process.exit(1);
}

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
  checkKind(run.kind, doc, group);
  checkSanity(run.kind, doc, group);
  window.close();
}

console.log(`\nSMOKE SUMMARY: ${pass} passed, ${fail} failed (${manifest.runs.length} runs)`);
if (fail) {
  console.log("Failures:");
  failures.forEach((f) => console.log("  - " + f));
  process.exit(1);
}
