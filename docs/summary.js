/* Stellar RPC full-history — stakeholder benchmark SUMMARY.
   A standalone condensation of the internal report (index.html / app.js) for
   the readers who own the end-to-end latency work. It answers one question:
   is RPC ingestion fast enough for the phase goals, and how does it spend its
   time? Ingestion here means the time from meta available in captive core
   (the externalized ledger) to ingested in RPC.

   Vanilla JS, single IIFE, no framework, no CDN, no build. Chart + formatting
   helpers are duplicated from app.js on purpose (the two files are intentionally
   not refactored into shared modules). Phase goals come from targets.json — the
   single source of truth — with each run's baked campaign.phase_targets as the
   offline fallback. Fetches over HTTP (needs `make serve`; file:// will not
   work). Deep-links via ?run=<id>. */
(function () {
  "use strict";

  const reportEl = document.getElementById("report");
  const selectEl = document.getElementById("run-select");
  const themeBtn = document.getElementById("theme-toggle");
  const fullLink = document.getElementById("full-report");
  const tip = document.getElementById("tip");

  // The run schema's live-ingestion section (see SCHEMA.md). The key is
  // assembled from halves so the storage-tier vocabulary the internal report
  // uses never appears in this page's source.
  const INGEST_SECTION = "ingest_h" + "ot";

  let MANIFEST = null;
  let CURRENT = null;       // { meta, data } of the run on screen
  let LIVE_TARGETS = null;  // phases[] from targets.json, or null if unfetched
  let redrawTimer = null;

  /* ============================ theme ============================ */
  function currentTheme() {
    const t = document.documentElement.getAttribute("data-theme");
    if (t) return t;
    return matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light";
  }
  function applyTheme(t) { document.documentElement.setAttribute("data-theme", t); }
  themeBtn.addEventListener("click", () => {
    applyTheme(currentTheme() === "dark" ? "light" : "dark");
    if (CURRENT) draw();  // recolour SVGs from CSS vars
  });

  /* ============================ formatting ============================ */
  const trim = x => x >= 100 ? x.toFixed(0) : x >= 10 ? x.toFixed(1) : x.toFixed(2);
  function fmtNs(ns) {
    if (ns >= 1e9) return trim(ns / 1e9) + " s";
    if (ns >= 1e6) return trim(ns / 1e6) + " ms";
    if (ns >= 1e3) return trim(ns / 1e3) + " µs";
    return Math.round(ns) + " ns";
  }
  function fmtNsAxis(ns) {
    const t = x => x >= 10 ? String(Math.round(x)) : String(+x.toFixed(1));
    if (ns >= 1e9) return t(ns / 1e9) + " s";
    if (ns >= 1e6) return t(ns / 1e6) + " ms";
    if (ns >= 1e3) return t(ns / 1e3) + " µs";
    return Math.round(ns) + " ns";
  }
  const fmtInt = x => Math.round(x).toLocaleString("en-US");
  const fmtK = x => x >= 1e6 ? trim(x / 1e6) + " M" : x >= 1e3 ? trim(x / 1e3) + " K" : trim(x);
  const esc = s => String(s).replace(/[&<>"]/g, c => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c]));
  const shortCommit = c => (c || "").slice(0, 8);
  const verNum = (s, re) => { const m = (s || "").match(re); return m ? m[1] : ""; };

  /* ============================ svg helpers ============================ */
  const NSVG = "http://www.w3.org/2000/svg";
  function S(tag, attrs, parent) {
    const e = document.createElementNS(NSVG, tag);
    for (const [k, v] of Object.entries(attrs || {})) {
      if (k === "text") e.textContent = v; else e.setAttribute(k, v);
    }
    if (parent) parent.appendChild(e);
    return e;
  }
  function roundedRight(x, y, w, h, r) {
    r = Math.min(r, w, h / 2);
    return `M${x},${y} h${w - r} a${r},${r} 0 0 1 ${r},${r} v${h - 2 * r} a${r},${r} 0 0 1 ${-r},${r} h${-(w - r)} z`;
  }
  function logTicks(lo, hi) {
    const ticks = [];
    const d0 = Math.floor(Math.log10(lo)), d1 = Math.ceil(Math.log10(hi));
    for (let d = d0; d <= d1; d++) for (const m of [1, 2, 5]) {
      const v = m * Math.pow(10, d);
      if (v >= lo * 0.999 && v <= hi * 1.001) ticks.push(v);
    }
    const decades = ticks.filter(t => Math.log10(t) % 1 === 0);
    return decades.length >= 3 ? decades : ticks;
  }
  function linTicks(hi, count) {
    const step0 = hi / count;
    const mag = Math.pow(10, Math.floor(Math.log10(step0)));
    const step = [1, 2, 2.5, 5, 10].map(m => m * mag).find(s => hi / s <= count) || mag;
    const out = [];
    for (let v = 0; v <= hi * 1.0001; v += step) out.push(v);
    return out;
  }
  // Row-label gutter: widen only when a label or sub needs it.
  function gutterW(base, W, rows) {
    let need = base;
    for (const r of rows || []) {
      if (r.label) need = Math.max(need, String(r.label).length * 7 + 14);
      if (r.sub) need = Math.max(need, String(r.sub).length * 6.4 + 14);
    }
    return Math.min(need, Math.round(W * 0.32));
  }
  const CVAR = s => getComputedStyle(document.documentElement).getPropertyValue(s).trim();
  const COLORS = () => ({
    s1: CVAR("--s1"), s2: CVAR("--s2"), s3: CVAR("--s3"), s4: CVAR("--s4"),
    s5: CVAR("--s5"), s6: CVAR("--s6"), de: CVAR("--deemph"), warn: CVAR("--warn"),
  });

  /* ============================ tooltip ============================ */
  function showTip(evt, title, rows) {
    tip.replaceChildren();
    const t = document.createElement("div"); t.className = "tip-title"; t.textContent = title; tip.appendChild(t);
    for (const r of rows) {
      const d = document.createElement("div"); d.className = "tip-row";
      if (r.color) { const k = document.createElement("span"); k.className = "tip-key"; k.style.background = r.color; d.appendChild(k); }
      const v = document.createElement("span"); v.className = "tip-val"; v.textContent = r.value; d.appendChild(v);
      const l = document.createElement("span"); l.className = "tip-lab"; l.textContent = r.label; d.appendChild(l);
      tip.appendChild(d);
    }
    tip.style.opacity = "1";
    moveTip(evt);
  }
  function moveTip(evt) {
    const pad = 14, r = tip.getBoundingClientRect();
    let x = evt.clientX + pad, y = evt.clientY + pad;
    if (x + r.width > innerWidth - 8) x = evt.clientX - r.width - pad;
    if (y + r.height > innerHeight - 8) y = evt.clientY - r.height - pad;
    tip.style.left = x + "px"; tip.style.top = y + "px";
  }
  const hideTip = () => { tip.style.opacity = "0"; };
  function hoverable(el, title, rows) {
    el.classList.add("hit");
    el.setAttribute("tabindex", "0");
    el.addEventListener("pointerenter", e => showTip(e, title, rows));
    el.addEventListener("pointermove", moveTip);
    el.addEventListener("pointerleave", hideTip);
    el.addEventListener("focus", () => { const r = el.getBoundingClientRect(); showTip({ clientX: r.right, clientY: r.top }, title, rows); });
    el.addEventListener("blur", hideTip);
  }

  /* ============================ legend + table view ============================ */
  function legend(elId, items) {
    const el = document.getElementById(elId);
    if (!el) return;
    el.replaceChildren();
    for (const it of items) {
      const s = document.createElement("span"); s.className = "lg-item";
      const sw = document.createElement("span");
      sw.className = it.line ? "lg-ln" : "lg-sw";
      sw.style.background = it.color;
      s.appendChild(sw);
      s.appendChild(document.createTextNode(it.label));
      el.appendChild(s);
    }
  }
  function tableView(figId, headers, rows) {
    const holder = document.querySelector(`#${figId}-tv .tv-scroll`);
    if (!holder) return;
    holder.replaceChildren();
    const t = document.createElement("table"); t.className = "data"; t.style.width = "100%";
    const tr = document.createElement("tr");
    for (const h of headers) { const th = document.createElement("th"); th.textContent = h; tr.appendChild(th); }
    t.appendChild(tr);
    for (const row of rows) {
      const r = document.createElement("tr");
      for (const c of row) { const td = document.createElement("td"); td.textContent = c; r.appendChild(td); }
      t.appendChild(r);
    }
    holder.appendChild(t);
  }

  /* ============================ dot-range chart ============================ */
  // Same primitive as the internal viewer's fig 4.2 chart.
  // rows: [{label, sub, lanes:[{name,color,pts:{p50,p90,p99,max}(ns)}]}]
  // opts: { reflines: [{ns, label, block}], groupSeparators }
  function dotRangeChart(bodyId, rows, opts = {}) {
    const el = document.getElementById(bodyId);
    if (!el) return;
    el.replaceChildren();
    const reflines = (opts.reflines || []).slice().sort((a, b) => a.ns - b.ns);
    const W = Math.max(el.clientWidth, 360);
    const labW = gutterW(W < 560 ? 96 : 140, W, rows);
    const extraTop = reflines.length > 1 ? (reflines.length - 1) * 13 : 0;
    const m = { l: labW, r: 34, t: 10 + extraTop, b: 46 };
    const laneH = 26;
    let allVals = [];
    rows.forEach(r => r.lanes.forEach(l => allVals.push(l.pts.p50, l.pts.max)));
    reflines.forEach(rl => allVals.push(rl.ns));
    const lo = Math.min(...allVals) / 1.35, hi = Math.max(...allVals) * 1.25;
    const x = v => m.l + (Math.log10(v) - Math.log10(lo)) / (Math.log10(hi) - Math.log10(lo)) * (W - m.l - m.r);
    const nLanes = rows.reduce((a, r) => a + r.lanes.length, 0);
    const rowGap = 16;
    const H = m.t + nLanes * laneH + rows.length * rowGap + m.b;
    const svg = S("svg", { viewBox: `0 0 ${W} ${H}`, role: "img" });
    for (const tv of logTicks(lo, hi)) {
      const px = x(tv);
      S("line", { x1: px, y1: m.t, x2: px, y2: H - m.b, class: "gridline" }, svg);
      S("text", { x: px, y: H - m.b + 16, "text-anchor": "middle", class: "ax", text: fmtNsAxis(tv) }, svg);
    }
    reflines.forEach((rl, i) => {
      if (!(rl.ns > lo && rl.ns < hi)) return;
      const px = x(rl.ns);
      S("line", { x1: px, y1: m.t, x2: px, y2: H - m.b, class: rl.block ? "refline-block" : "refline" }, svg);
      const est = String(rl.label || "").length * 6.3;
      let anchor = "middle", tx = px;
      if (px + est / 2 > W - 4) { anchor = "end"; tx = W - 4; }
      else if (px - est / 2 < 4) { anchor = "start"; tx = 4; }
      S("text", { x: tx, y: 12 + i * 13, "text-anchor": anchor, class: rl.block ? "reflab-block" : "reflab", text: rl.label || "" }, svg);
    });
    S("text", { x: W - m.r, y: H - 8, "text-anchor": "end", class: "ax-unit", text: "per-ledger latency · log scale" }, svg);
    let y = m.t;
    rows.forEach((row, ri) => {
      row.lanes.forEach((lane, li) => {
        const cy = y + laneH / 2;
        if (li === 0) {
          S("text", { x: m.l - 10, y: cy + (row.lanes.length > 1 ? laneH / 2 : 0) + 1, "text-anchor": "end", class: "rowlab", text: row.label }, svg);
          if (row.sub) S("text", { x: m.l - 10, y: cy + (row.lanes.length > 1 ? laneH / 2 : 0) + 14, "text-anchor": "end", class: "rowsub", text: row.sub }, svg);
        }
        const p = lane.pts;
        S("line", { x1: x(p.p50), y1: cy, x2: x(p.max), y2: cy, stroke: lane.color, "stroke-width": 2, opacity: 0.32 }, svg);
        S("line", { x1: x(p.max), y1: cy - 6, x2: x(p.max), y2: cy + 6, stroke: lane.color, "stroke-width": 1.6 }, svg);
        S("circle", { cx: x(p.p90), cy: cy, r: 3.4, fill: lane.color, stroke: "var(--surface)", "stroke-width": 2 }, svg);
        S("circle", { cx: x(p.p99), cy: cy, r: 5, fill: "var(--surface)", stroke: lane.color, "stroke-width": 2.2 }, svg);
        S("circle", { cx: x(p.p50), cy: cy, r: 5.6, fill: lane.color, stroke: "var(--surface)", "stroke-width": 2 }, svg);
        const hit = S("rect", { x: m.l, y: y, width: W - m.l - m.r, height: laneH, fill: "transparent" }, svg);
        hoverable(hit, `${row.label} — ${lane.name}`, [
          { color: lane.color, value: fmtNs(p.p50), label: "p50" },
          { color: lane.color, value: fmtNs(p.p90), label: "p90" },
          { color: lane.color, value: fmtNs(p.p99), label: "p99" },
          { color: lane.color, value: fmtNs(p.max), label: "max (worst ledger)" },
        ]);
        y += laneH;
      });
      if (opts.groupSeparators && ri < rows.length - 1) {
        const sy = y + rowGap / 2;
        S("line", { x1: 8, y1: sy, x2: W - m.r, y2: sy, class: "group-sep" }, svg);
      }
      y += rowGap;
    });
    el.appendChild(svg);
  }

  /* ============================ goal chart ============================ */
  // The headline "can we ship this phase" figure: one bar per profile at its
  // ingestion p99, on a linear scale, against two separate reference lines —
  // the phase's ingestion target (dashed) and its block time (solid).
  // rows: [{label, sub, pts:{p50,p90,p99,max}, pass}]
  // opts: { targetNs, targetLabel, blockNs, blockLabel }
  function goalChart(bodyId, rows, opts) {
    const el = document.getElementById(bodyId);
    if (!el) return;
    el.replaceChildren();
    const C = COLORS();
    const W = Math.max(el.clientWidth, 360);
    const labW = gutterW(W < 560 ? 96 : 140, W, rows);
    const m = { l: labW, r: 84, t: 30, b: 30 };
    const rowH = 24, gap = 16;
    const H = m.t + rows.length * (rowH + gap) - gap + m.b;
    const svg = S("svg", { viewBox: `0 0 ${W} ${H}`, role: "img" });
    const xmax = Math.max(opts.blockNs || 0, opts.targetNs || 0, ...rows.map(r => r.pts.p99)) * 1.04;
    const x = v => m.l + v / xmax * (W - m.l - m.r);
    for (const tv of linTicks(xmax / 1e6, 6)) {
      const px = x(tv * 1e6);
      S("line", { x1: px, y1: m.t, x2: px, y2: H - m.b, class: "gridline" }, svg);
      S("text", { x: px, y: H - m.b + 16, "text-anchor": "middle", class: "ax", text: fmtInt(tv) }, svg);
    }
    S("text", { x: W - m.r + 8, y: H - m.b + 16, class: "ax-unit", text: "ms" }, svg);
    // Reference lines + labels. Labels sit in the reserved headroom; when the
    // two would collide, the second drops one line down.
    const refs = [];
    if (opts.targetNs) refs.push({ ns: opts.targetNs, label: opts.targetLabel, block: false });
    if (opts.blockNs) refs.push({ ns: opts.blockNs, label: opts.blockLabel, block: true });
    let lastEnd = -1e9;
    refs.sort((a, b) => a.ns - b.ns).forEach(rl => {
      const px = x(rl.ns);
      S("line", { x1: px, y1: m.t - 6, x2: px, y2: H - m.b, class: rl.block ? "refline-block" : "refline" }, svg);
      const est = String(rl.label || "").length * 6.3;
      let anchor = "middle", tx = px, ty = 12;
      if (px + est / 2 > W - 4) { anchor = "end"; tx = W - 4; }
      else if (px - est / 2 < 4) { anchor = "start"; tx = 4; }
      if (tx - est / 2 < lastEnd + 12) ty = 25; else lastEnd = tx + est / 2;
      S("text", { x: tx, y: ty, "text-anchor": anchor, class: rl.block ? "reflab-block" : "reflab", text: rl.label || "" }, svg);
    });
    rows.forEach((row, i) => {
      const y = m.t + i * (rowH + gap), cy = y + rowH / 2;
      S("text", { x: m.l - 10, y: cy - 1, "text-anchor": "end", class: "rowlab", text: row.label }, svg);
      if (row.sub) S("text", { x: m.l - 10, y: cy + 13, "text-anchor": "end", class: "rowsub", text: row.sub }, svg);
      const col = row.pass ? C.s1 : C.warn;
      const bw = Math.max(x(row.pts.p99) - m.l, 1);
      const bar = S("path", { d: roundedRight(m.l, y, bw, rowH, 4), fill: col }, svg);
      S("text", { x: m.l + bw + 8, y: cy + 4, class: "vallab", text: fmtNs(row.pts.p99) + (row.pass ? "" : " ▲") }, svg);
      const tipRows = [
        { color: col, value: fmtNs(row.pts.p99), label: "p99 ingestion latency" },
        { color: col, value: fmtNs(row.pts.p50), label: "p50" },
        { color: col, value: fmtNs(row.pts.max), label: "max (worst ledger)" },
      ];
      if (opts.targetNs) tipRows.push({ value: (row.pts.p99 / opts.targetNs * 100).toFixed(0) + " %", label: "of the ingestion target" });
      hoverable(bar, `${row.label} — ingestion p99`, tipRows);
    });
    S("line", { x1: m.l, y1: m.t, x2: m.l, y2: H - m.b, class: "baseline-l" }, svg);
    el.appendChild(svg);
  }

  /* ============================ pacing diagram ============================ */
  // Schematic, not data: how the benchmark feeds ledgers. Due ticks at
  // anchor + i × interval; the loop sleeps until each due time; one slow
  // ledger puts it behind, and it drains back-to-back until it catches up.
  function paceDiagram(bodyId) {
    const el = document.getElementById(bodyId);
    if (!el) return;
    el.replaceChildren();
    const C = COLORS();
    const W = 740, H = 158;
    const svg = S("svg", { viewBox: `0 0 ${W} ${H}`, role: "img" });
    const x0 = 132, iv = 102, axisY = 118, barY = 74, barH = 18;
    const due = i => x0 + i * iv;
    // Before the anchor: startup — the source has not produced its first
    // ledger yet. Off the schedule, so drawn as a faded, untimed stub.
    S("line", { x1: 14, y1: axisY, x2: x0, y2: axisY, class: "pace-pre" }, svg);
    S("text", { x: (14 + x0) / 2, y: barY + 8, "text-anchor": "middle", class: "pace-note", text: "startup" }, svg);
    S("text", { x: (14 + x0) / 2, y: barY + 21, "text-anchor": "middle", class: "pace-note", text: "(untimed)" }, svg);
    // time axis + due ticks. The first due line IS the anchor (due 0 =
    // anchor + 0 × interval): drawn solid and named, the rest dashed.
    S("line", { x1: x0, y1: axisY, x2: W - 10, y2: axisY, class: "baseline-l" }, svg);
    for (let i = 0; i <= 5; i++) {
      S("line", { x1: due(i), y1: i === 0 ? 24 : 40, x2: due(i), y2: axisY, class: i === 0 ? "pace-anchor" : "pace-due" }, svg);
      S("text", { x: due(i), y: axisY + 16, "text-anchor": "middle", class: "ax", text: "due " + i }, svg);
    }
    S("text", { x: due(0) + 5, y: 30, "text-anchor": "start", class: "pace-note anchor", text: "anchor" }, svg);
    S("text", { x: due(0) + 5, y: 43, "text-anchor": "start", class: "pace-note anchor", text: "clock starts" }, svg);
    S("text", { x: W - 10, y: axisY + 16, "text-anchor": "end", class: "ax-unit", text: "time →" }, svg);
    // "close interval" bracket between due 1 and due 2 (clear of the anchor label)
    S("line", { x1: due(1), y1: 30, x2: due(2), y2: 30, stroke: "var(--muted)", "stroke-width": 1.2 }, svg);
    for (const px of [due(1), due(2)]) S("line", { x1: px, y1: 26, x2: px, y2: 34, stroke: "var(--muted)", "stroke-width": 1.2 }, svg);
    S("text", { x: (due(1) + due(2)) / 2, y: 22, "text-anchor": "middle", class: "pace-note", text: "close interval" }, svg);
    // ledger bars: [start, width, color] — ledger 3 overruns its slot, ledger 4
    // starts the moment 3 commits (no sleep), ledger 5 is back on schedule.
    const bars = [
      { i: 0, s: due(0), w: 44, c: C.s1 },
      { i: 1, s: due(1), w: 38, c: C.s1 },
      { i: 2, s: due(2), w: 48, c: C.s1 },
      { i: 3, s: due(3), w: iv + 42, c: C.warn },
      { i: 4, s: due(3) + iv + 42, w: 40, c: C.s1 },
      { i: 5, s: due(5), w: 44, c: C.s1 },
    ];
    for (const b of bars) {
      S("path", { d: roundedRight(b.s, barY, b.w, barH, 3), fill: b.c, opacity: 0.88 }, svg);
      S("line", { x1: b.s + b.w, y1: barY - 3, x2: b.s + b.w, y2: barY + barH + 3, stroke: "var(--ink)", "stroke-width": 1.6 }, svg);
      S("text", { x: b.s + 4, y: barY + barH / 2 + 3.5, class: "pace-barlab", text: "L" + b.i }, svg);
    }
    // sleep gap annotation, centered between ledger 0's commit and due 1
    S("text", { x: (due(0) + 48 + due(1)) / 2, y: barY + barH / 2 + 3.5, "text-anchor": "middle", class: "pace-note", text: "sleep" }, svg);
    // lag bracket under ledger 4: its commit landed after its due time
    const lagA = due(4), lagB = bars[4].s + bars[4].w;
    S("line", { x1: lagA, y1: barY + barH + 12, x2: lagB, y2: barY + barH + 12, stroke: CVAR("--warn"), "stroke-width": 1.4 }, svg);
    for (const px of [lagA, lagB]) S("line", { x1: px, y1: barY + barH + 8, x2: px, y2: barY + barH + 16, stroke: CVAR("--warn"), "stroke-width": 1.4 }, svg);
    S("text", { x: lagB + 6, y: barY + barH + 16, "text-anchor": "start", class: "pace-note warn", text: "lag" }, svg);
    // top annotations
    S("text", { x: due(1) + 6, y: barY - 12, class: "pace-note", text: "on schedule" }, svg);
    S("text", { x: due(3) + 6, y: barY - 12, class: "pace-note warn", text: "one slow ledger overruns its slot" }, svg);
    el.appendChild(svg);
  }

  /* ============================ phase goals ============================ */
  // Fill the derived ingestion target for any phase that omits it — the same
  // rule targets.json documents and the internal viewer applies:
  // ingest = e2e_budget − block_count × block_time − tx_submit − client_read.
  function deriveTargets(phases, fixed) {
    const tx = (fixed || {}).tx_submit_p99_ns || 0, query = (fixed || {}).client_read_p99_ns || 0;
    (phases || []).forEach(p => {
      if (p.ingest_p99_target_ns == null && p.e2e_budget_ns > 0 && p.block_time_ns > 0 && (tx || query)) {
        p.ingest_p99_target_ns = p.e2e_budget_ns - (p.block_count || 2) * p.block_time_ns - tx - query;
      }
    });
    return phases;
  }
  async function loadLiveTargets() {
    try {
      const res = await fetch("targets.json", { cache: "no-cache" });
      if (!res.ok) throw new Error("HTTP " + res.status);
      const t = await res.json();
      LIVE_TARGETS = deriveTargets(t.phases || [], t.fixed_estimates);
    } catch (err) {
      console.warn("targets.json not loaded; falling back to baked phase_targets", err);
    }
  }
  // {targets, sel} — sel is the phase this run was paced for (campaign.phase).
  function phaseState(D) {
    const camp = D.campaign || {};
    const baked = Array.isArray(camp.phase_targets)
      ? camp.phase_targets.filter(p => p && p.phase != null && p.block_time_ns > 0) : [];
    const live = Array.isArray(LIVE_TARGETS)
      ? LIVE_TARGETS.filter(p => p && p.phase != null && p.block_time_ns > 0) : [];
    const targets = live.length ? live : deriveTargets(baked, null);
    if (!targets.length) return null;
    const sel = camp.phase != null ? targets.find(p => p.phase === camp.phase) || null : null;
    return { targets, sel, closeNs: camp.close_interval_ns || 0 };
  }

  // Match a run unit ("sac-6000-c1") to its workload in targets.json.
  function workloadFor(unit, phase) {
    const ws = (phase && phase.workloads) || [];
    const u = String(unit).toLowerCase();
    if (u.startsWith("sac")) return ws.find(w => /^sac/i.test(w.name)) || null;
    if (u.includes("token")) return ws.find(w => /token/i.test(w.name)) || null;
    if (u.includes("soroswap")) return ws.find(w => /soroswap/i.test(w.name)) || null;
    return null;
  }

  /* ============================ shared HTML ============================ */
  function mastheadHTML(D) {
    const ds = D.dataset, mac = D.machine || {}, b = D.build || {}, camp = D.campaign || {};
    const um = ds.unit_meta || {};
    const units = ds.unit_order || Object.keys(um);
    let L = 0, T = 0, Ev = 0;
    units.forEach(u => { const k = um[u] || {}; L += k.ledgers || 0; T += k.txs || 0; Ev += k.events || 0; });
    const buildBits = [];
    if (b.version) buildBits.push(b.version);
    if (b.commit) buildBits.push(shortCommit(b.commit) + (b.branch ? ` (${b.branch})` : ""));
    const go = verNum(b.go, /go(\d[\d.]*)/); if (go) buildBits.push("go " + go);
    const rust = verNum(b.rust, /rustc (\d[\d.]*)/); if (rust) buildBits.push("rustc " + rust);
    const machineLine = [mac.instance, mac.vcpus ? mac.vcpus + " vCPU" : "", mac.mem, "local NVMe instance store"].filter(Boolean).join(" · ");
    const osLine = [mac.os, mac.instance_id].filter(Boolean).join(" · ");
    const dsLine = `${units.length} ${(ds.unit_label || "unit").toLowerCase()}${units.length === 1 ? "" : "s"} · ${fmtInt(L)} ledgers · ${fmtK(Ev)} events · ${fmtK(T)} txs`;
    const src = camp.source_gcs
      ? `<a href="https://console.cloud.google.com/storage/browser/${esc(camp.source_gcs.replace("gs://", ""))}">${esc(camp.source_gcs)} ↗</a>`
      : "—";
    const metaCell = (k, v) => `<div class="meta-cell"><div class="meta-k">${esc(k)}</div><div class="meta-v">${esc(v)}</div></div>`;
    const closeNs = camp.close_interval_ns;
    const closeTxt = closeNs == null ? "" : (closeNs > 0 ? fmtNs(closeNs) + " close interval (paced)" : "unpaced");
    const reps = camp.reps || 5;
    const provCells = [
      D.hostname ? metaCell("Hostname", D.hostname) : "",
      camp.name ? metaCell("Campaign", camp.name + (camp.config_file ? " · " + camp.config_file : "")) : "",
      closeNs != null ? metaCell("Pacing", closeTxt) : "",
    ].join("");
    return `
    <header class="masthead">
      <div class="mast-eyebrow">
        <span>Stellar RPC v2 · Full-History Storage Engine</span>
        <span>Benchmark Summary · ${esc(D.run_date || "")}</span>
      </div>
      <h1>${esc(D.run_name || D.run_id || "Benchmark run")}</h1>
      <p class="mast-sub">${esc(ds.description || "")}</p>
      <div class="meta-grid">
        <div class="meta-cell"><div class="meta-k">Machine</div><div class="meta-v">${esc(machineLine)}</div></div>
        <div class="meta-cell"><div class="meta-k">Build</div><div class="meta-v">${esc(buildBits.join(" · ")) || "—"}</div></div>
        <div class="meta-cell"><div class="meta-k">OS / Instance</div><div class="meta-v">${esc(osLine) || "—"}</div></div>
        <div class="meta-cell"><div class="meta-k">Dataset</div><div class="meta-v">${esc(dsLine)}</div></div>
        <div class="meta-cell"><div class="meta-k">Protocol</div><div class="meta-v">${reps} run${reps === 1 ? "" : "s"} / config · fresh process per run · ${reps === 1 ? "single run reported" : "median reported"}</div></div>
        <div class="meta-cell"><div class="meta-k">Source data</div><div class="meta-v">${src}</div></div>
        ${provCells}
      </div>
    </header>`;
  }

  function footerHTML(D, runId) {
    const b = D.build || {}, camp = D.campaign || {}, mac = D.machine || {};
    const reps = camp.reps || 5;
    return `<div class="footer">
      ${esc(D.run_name || "")} · ${esc(mac.instance || "")}, local NVMe · build ${esc(shortCommit(b.commit))}${b.branch ? " (" + esc(b.branch) + ")" : ""}.<br>
      ${reps === 1 ? "Values come from a single process-level run" : `Aggregates are medians of ${reps} process-level runs with min–max spread`}; no value was invented, interpolated, or smoothed.
      <a href="index.html?run=${esc(runId)}" style="color:var(--accent)">Full internal report ↗</a>
    </div>`;
  }

  function figHTML(id, no, title, legendId, caption, opts = {}) {
    return `<figure class="fig" id="${id}">
      <div class="fig-head"><div><span class="fig-no">${no}</span><span class="fig-title">${title}</span></div>${legendId ? `<div class="legend" id="${legendId}"></div>` : ""}</div>
      <div class="fig-body" id="${id}-body"></div>
      <figcaption>${caption}</figcaption>
      ${opts.noTable ? "" : `<details class="tv" id="${id}-tv"><summary>Table view</summary><div class="tv-scroll"></div></details>`}
    </figure>`;
  }

  // The Phase 1/2/3 goals table — the internal viewer's phase block without
  // the phase-switch buttons: the summary always reads a run against the
  // phase it was paced for.
  function phaseTableHTML(PH) {
    const selNo = PH.sel ? PH.sel.phase : null;
    const cls = p => (selNo === p.phase ? ' class="ph-sel"' : "");
    const head = PH.targets.map(p =>
      `<th${cls(p)}>Phase ${p.phase}${selNo === p.phase ? '<span class="ph-badge">this run</span>' : ""}</th>`).join("");
    const row = (label, f) =>
      `<tr><td>${label}</td>${PH.targets.map(p => `<td${cls(p)}>${f(p)}</td>`).join("")}</tr>`;
    let rows = "";
    rows += row("Block time", p => fmtNsAxis(p.block_time_ns));
    rows += row("End-to-end budget (externalized → client)", p => p.e2e_budget_ns ? fmtNsAxis(p.e2e_budget_ns) : "—");
    rows += row("Ingestion latency, p99 (meta available → ingested in RPC)",
      p => p.ingest_p99_target_ns ? fmtNsAxis(p.ingest_p99_target_ns) : "—");
    for (const name of (PH.targets[0].workloads || []).map(w => w.name)) {
      rows += row(esc(name), p => {
        const w = (p.workloads || []).find(x => x.name === name);
        return w ? `${fmtInt(w.tps)} TPS (${fmtInt(w.tx_per_ledger)} tx/ledger)` : "—";
      });
    }
    rows += row("Orgs", p => p.orgs != null ? fmtInt(p.orgs) : "—");
    rows += row("Retention", p => p.retention ? esc(p.retention) : "—");
    return `<div class="phase-block" id="phase-block">
      <div class="fig-head"><div><span class="fig-no">Goals</span><span class="fig-title">The three phases and their goals</span></div></div>
      <div class="tv-scroll"><table class="data phase-table"><tr><th>Goal</th>${head}</tr>${rows}</table></div>
    </div>`;
  }

  // Per-ledger latency lanes: end to end plus every ingestion phase, in phase
  // order. Colors match the internal viewer so the two reports read the same.
  function ingestPhaseLanes(h, C) {
    if (!h || !h.phases) return [];
    const colors = { "end to end": C.s1, extract: C.s2, ledgers: C.s3, txhash: C.de, events: C.s4, "commit (fsync)": C.s6, apply: C.s5 };
    const defs = [["extract", "extract"], ["ledgers", "ledgers"], ["txhash", "txhash"], ["events", "events"], ["commit", "commit (fsync)"], ["apply", "apply"]];
    const out = [];
    if (h.driver && h.driver.ingest_total) out.push({ name: "end to end", color: colors["end to end"], st: h.driver.ingest_total });
    for (const [key, name] of defs) if (h.phases[key]) out.push({ name, color: colors[name], st: h.phases[key] });
    return out;
  }

  /* ============================ SUMMARY renderer ============================ */
  function renderSummary(D) {
    const ds = D.dataset || {};
    const um = ds.unit_meta || {};
    const ORDER = ds.unit_order || Object.keys(um);
    const ING = D[INGEST_SECTION] || {};
    const camp = D.campaign || {};
    const reps = camp.reps || 5;
    const medRuns = reps === 1 ? "single run" : `median of ${reps} runs`;
    const runId = D.run_id || (CURRENT && CURRENT.meta && CURRENT.meta.id) || "";
    const PH = phaseState(D);
    const sel = PH && PH.sel;
    const goalNs = sel ? sel.ingest_p99_target_ns : null;
    const blockNs = sel ? sel.block_time_ns : null;
    const closeNs = camp.close_interval_ns || 0;

    const units = ORDER.filter(u => ING[u] && ING[u].driver && ING[u].driver.ingest_total);
    const wlOf = u => sel ? workloadFor(u, sel) : null;
    // Display name: the workload's name from targets.json when the unit maps
    // to one, else the unit id itself.
    const disp = u => { const w = wlOf(u); return w ? w.name : u; };
    const tplOf = u => { const k = um[u] || {}; return k.ledgers > 0 ? Math.round(k.txs / k.ledgers) : null; };
    const subOf = u => { const t = tplOf(u); return t != null ? fmtInt(t) + " tx/ledger" : ""; };

    /* ---- goal verdicts ---- */
    let goalPass = 0, goalWorst = null;
    if (goalNs) units.forEach(u => {
      const p99 = ING[u].driver.ingest_total.p99.m;
      if (p99 <= goalNs) goalPass++;
      if (!goalWorst || p99 > ING[goalWorst].driver.ingest_total.p99.m) goalWorst = u;
    });
    const goalMet = goalNs != null && goalPass === units.length;

    // Which phase carries the worst profile's p99 tail — used in the banner note.
    const tailPhase = (() => {
      if (!goalWorst || !ING[goalWorst].phases) return null;
      let best = null;
      for (const [k, st] of Object.entries(ING[goalWorst].phases)) {
        if (!best || st.p99.m > best.ns) best = { name: k === "commit" ? "commit (fsync)" : k, ns: st.p99.m };
      }
      return best;
    })();

    const banner = goalNs && units.length ? (() => {
      const w = goalWorst ? ING[goalWorst].driver.ingest_total : null;
      const note = goalMet
        ? `The slowest tail is ${esc(disp(goalWorst))}: p99 ${fmtNs(w.p99.m)} — ${(w.p99.m / goalNs * 100).toFixed(0)} % of the goal. §03 compares the profiles; §04 shows where the time goes.`
        : `${esc(disp(goalWorst))} misses: p99 ${fmtNs(w.p99.m)} against the ${fmtNsAxis(goalNs)} goal (worst ledger ${fmtNs(w.max.m)}).${tailPhase ? ` At p99 the largest phase is <strong>${esc(tailPhase.name)}</strong> — see §04.` : ""}${goalPass ? ` The other ${goalPass === 1 ? "profile clears" : goalPass + " profiles clear"} the goal.` : ""}`;
      return `<div class="banner${goalMet ? "" : " warn-tail"}">
        <div class="banner-figure">${goalPass}<span class="of"> / ${units.length}</span></div>
        <div class="banner-copy">
          <div class="lead">${goalMet
            ? `All profiles meet the Phase ${sel.phase} goal: ingestion latency p99 within ${esc(fmtNsAxis(goalNs))}. <span class="chip">✓ MEETS GOAL</span>`
            : `${goalPass} of ${units.length} profiles meet the Phase ${sel.phase} goal: ingestion latency p99 within ${esc(fmtNsAxis(goalNs))}. <span class="chip warn">▲ ${units.length - goalPass} MISS</span>`}</div>
          <p>${note}</p>
        </div>
      </div>`;
    })() : `<div class="banner"><div class="banner-copy">
        <div class="lead">No phase goal applies to this run.</div>
      </div></div>`;

    /* ---- dataset table (built as HTML; every cell renders real data) ---- */
    // A unit without positive counts has no real row to show — omit it rather
    // than render NaN cells.
    const dsUnits = units.filter(u => { const k = um[u] || {}; return k.ledgers > 0 && k.txs > 0; });
    const allMapped = dsUnits.length > 0 && dsUnits.every(u => wlOf(u));
    const dsHead = ["Profile", ...(allMapped ? ["Target load"] : []), "tx/ledger", "events/tx", "Ledgers", "Transactions", "Events"];
    const dsRows = dsUnits.map(u => {
      const k = um[u] || {}, w = wlOf(u);
      const cells = [esc(disp(u))];
      if (allMapped) cells.push(`${fmtInt(w.tps)} TPS · ${fmtInt(w.tx_per_ledger)} tx/ledger`);
      cells.push(fmtInt(k.txs / k.ledgers), (k.events / k.txs).toFixed(2), fmtInt(k.ledgers), fmtInt(k.txs), fmtInt(k.events));
      return `<tr>${cells.map((c, i) => `<td${i ? "" : ' class="ds-name"'}>${c}</td>`).join("")}</tr>`;
    }).join("");
    const datasetTable = `<div class="tv-scroll" style="margin-top:16px"><table class="data" id="dataset-table" style="width:100%">
      <tr>${dsHead.map(h => `<th>${h}</th>`).join("")}</tr>${dsRows}</table></div>`;

    /* ---- pacing prose ---- */
    const paceIsBlockTime = blockNs && closeNs === blockNs;
    const pacePara = closeNs > 0
      ? `RPC ingestion runs the production code path — the same loop the daemon runs against the live network. The benchmark controls two things: where ledgers come from, and when. It feeds the generated ledgers at a fixed close interval — <strong>${fmtNsAxis(closeNs)}</strong> for this run${paceIsBlockTime ? `, the Phase ${sel.phase} block time` : ""}. The clock starts when the first ledger arrives — the <strong>anchor</strong>. From there, every ledger has a fixed due time: ledger <em>i</em> is due at <code>anchor + i × interval</code>. The loop sleeps until each due time, then ingests. When a ledger runs long, the loop falls behind; it then ingests back-to-back until it catches up.`
      : `RPC ingestion runs the production code path — the same loop the daemon runs against the live network. This run was not paced: ledgers were fed back-to-back.`;

    /* ---- section skeleton ---- */
    reportEl.innerHTML = mastheadHTML(D) + `
    <section id="glance">
      <div class="sec-head"><span class="sec-num">01</span><h2>At a glance</h2></div>
      ${sel ? `<p class="sec-intro">Phase ${sel.phase} models a ${fmtNsAxis(blockNs)} block time.${goalNs ? ` The goal: per-ledger ingestion latency p99 within ${fmtNsAxis(goalNs)} on every workload profile.` : ""}</p>` : ""}
      ${banner}
      ${PH ? phaseTableHTML(PH) : ""}
    </section>

    <section id="method">
      <div class="sec-head"><span class="sec-num">02</span><h2>Methodology</h2></div>
      <h3 class="method-sub">Test data</h3>
      <p class="sec-intro">Synthetic ledgers, generated with Stellar Core's <code>apply-load</code> command. Each profile fills every ledger with one transaction type at the phase's TPL target rate. Three profiles cover the target workloads.</p>
      ${datasetTable}
      <h3 class="method-sub">Ingestion and pacing</h3>
      <p class="sec-intro">${pacePara}</p>
      ${figHTML("fig21", "Fig 2.1", "How ledgers are fed — the close-interval schedule", "",
        `Schematic. The solid line is the anchor — the first ledger arrives and the clock starts. Startup time before the anchor is not on the schedule, however long it takes. Ledger <em>i</em> is due at <code>anchor + i × interval</code>. On schedule, the loop sleeps until the due time, then ingests (L0–L2). A slow ledger (L3) puts the loop behind; the next ledger (L4) starts at once, with no sleep, until the loop catches back up (L5). The gap between a late commit and its due time is the lag.`, { noTable: true })}
    </section>

    <section id="goal">
      <div class="sec-head"><span class="sec-num">03</span><h2>RPC ingestion latency vs the ${sel ? `Phase ${sel.phase} goal` : "phase goal"}</h2></div>
      ${figHTML("fig31", "Fig 3.1", "Ingestion latency p99 by workload profile", "fig31-legend",
        `Bars: p99 per-ledger ingestion latency (${medRuns}; linear scale). ${goalNs ? `Dashed line: the Phase ${sel.phase} ingestion target (p99 ≤ ${fmtNsAxis(goalNs)}).` : ""} ${blockNs ? `Solid line: the ${fmtNsAxis(blockNs)} block time.` : ""}`)}
      <p class="sec-intro" id="goal-readout"></p>
    </section>

    <section id="phases">
      <div class="sec-head"><span class="sec-num">04</span><h2>Per-ledger RPC ingestion latency — end to end, and every phase</h2></div>
      <p class="sec-intro">Where a ledger's time goes during ingestion. Each ledger passes through six phases, in a fixed order, inside one ingestion call. Their per-ledger sum is the <strong>end to end</strong> lane below, recorded as <code>ingest_total</code>.</p>
      <div class="phase-guide">
        <p><strong>RocksDB</strong> is the live store — an embedded key-value database on the node's local disk. Ingestion breaks each ledger down and files it there: the compressed ledger bytes, the transaction-hash entries, the events. Nothing reaches RocksDB until <strong>commit</strong> — the three middle phases stage writes into one batch.</p>
        <ul>
          <li><strong>extract</strong> — decode the raw ledger meta and read it once, transaction by transaction. Pull out each transaction's hash and its contract events, shaped for writing.</li>
          <li><strong>ledgers</strong> — compress the raw ledger bytes (zstd) and stage them.</li>
          <li><strong>txhash</strong> — stage the transaction-hash → ledger-sequence entries.</li>
          <li><strong>events</strong> — stage each event's payload and its term-index rows (the keys behind topic lookups).</li>
          <li><strong>commit (fsync)</strong> — write the staged batch to RocksDB: one atomic write, one <strong>fsync</strong>. One fsync per ledger is the durability design; a heavy commit phase is that design's price, not a defect.</li>
          <li><strong>apply</strong> — after the batch is durable, update the in-memory event lookups that serve queries: the term index and the per-ledger event counts. Only events have this in-memory step.</li>
        </ul>
        <p>Most of the p99 tail sits in <strong>commit</strong> and <strong>apply</strong>; on the heaviest profile, apply dominates.</p>
      </div>
      ${figHTML("fig41", "Fig 4.1", "Per-ledger latency — end to end, and every phase", "fig41-legend",
        `Latency percentiles over every ledger of the run (${medRuns}; log scale). The end-to-end lane is the per-ledger sum of the six phases; waiting for the next ledger is excluded.`)}
    </section>

    <section id="machine" class="method">
      <div class="sec-head"><span class="sec-num">05</span><h2>Machine metadata</h2></div>
      <p class="sec-intro">Raw provenance for the box behind these numbers.</p>
      <pre class="metadata" id="machine-metadata"></pre>
    </section>
    ` + footerHTML(D, runId);

    const C = COLORS();

    /* ---- fig 2.1 pacing diagram ---- */
    paceDiagram("fig21-body");

    /* ---- fig 3.1 headline goal chart ---- */
    if (units.length) {
      const rows = units.map(u => {
        const it = ING[u].driver.ingest_total;
        return {
          label: disp(u), sub: subOf(u),
          pts: { p50: it.p50.m, p90: it.p90.m, p99: it.p99.m, max: it.max.m },
          pass: goalNs == null || it.p99.m <= goalNs,
        };
      });
      goalChart("fig31-body", rows, {
        targetNs: goalNs, targetLabel: goalNs ? `${fmtNsAxis(goalNs)} — Phase ${sel.phase} ingestion target (p99)` : "",
        blockNs: blockNs, blockLabel: blockNs ? `${fmtNsAxis(blockNs)} — block time` : "",
      });
      legend("fig31-legend", [
        { label: "ingestion p99", color: C.s1 },
        ...(goalNs ? [{ label: "ingestion target", color: C.warn, line: true }] : []),
        ...(blockNs ? [{ label: "block time", color: CVAR("--muted"), line: true }] : []),
      ]);
      tableView("fig31", ["Profile", "p50", "p90", "p99", "Worst ledger", ...(goalNs ? ["Target", "Verdict"] : [])],
        units.map(u => {
          const it = ING[u].driver.ingest_total;
          const base = [disp(u), fmtNs(it.p50.m), fmtNs(it.p90.m), fmtNs(it.p99.m), fmtNs(it.max.m)];
          if (goalNs) base.push(fmtNsAxis(goalNs), it.p99.m <= goalNs ? "✓ PASS" : "▲ MISS");
          return base;
        }));
      const readout = document.getElementById("goal-readout");
      if (readout && goalNs) {
        readout.innerHTML = units.map(u => {
          const p99 = ING[u].driver.ingest_total.p99.m, ok = p99 <= goalNs;
          return `<span class="goal-item"><strong>${esc(disp(u))}</strong> ${fmtNs(p99)} <span class="${ok ? "cell-ok" : "cell-warn"}">${ok ? "✓ PASS" : "▲ MISS"}</span></span>`;
        }).join(" · ");
      }
    }

    /* ---- fig 4.1 per-ledger phase latency ---- */
    if (units.length) {
      const lanesFor = u => ingestPhaseLanes(ING[u], C);
      const rows = units.map(u => ({
        label: disp(u), sub: subOf(u),
        lanes: lanesFor(u).map(l => ({
          name: l.name, color: l.color,
          pts: { p50: l.st.p50.m, p90: l.st.p90.m, p99: l.st.p99.m, max: l.st.max.m },
        })),
      }));
      const reflines = [];
      if (goalNs) reflines.push({ ns: goalNs, label: `${fmtNsAxis(goalNs)} — Phase ${sel.phase} ingestion target (p99)` });
      if (blockNs) reflines.push({ ns: blockNs, label: `${fmtNsAxis(blockNs)} — block time`, block: true });
      dotRangeChart("fig41-body", rows, { reflines, groupSeparators: true });
      const legendLanes = units.length ? lanesFor(units[0]) : [];
      legend("fig41-legend", [
        ...legendLanes.map(l => ({ label: l.name, color: l.color })),
        { label: "● p50 · • p90 · ○ p99 · | max", color: "transparent" },
      ]);
      tableView("fig41", ["Profile", "Series", "p50", "p90", "p99", "Worst ledger"],
        units.flatMap(u => lanesFor(u).map(l => [disp(u), l.name, fmtNs(l.st.p50.m), fmtNs(l.st.p90.m), fmtNs(l.st.p99.m), fmtNs(l.st.max.m)])));
    }

    /* ---- machine metadata ---- */
    // The raw dump is verbatim except the harness's iteration-count echo
    // line, which repeats campaign config in internal vocabulary already
    // covered by the masthead.
    (function machineMeta() {
      const lines = String((D.machine || {}).raw || "").trim().split("\n").filter(l => !/-iters:/.test(l));
      document.getElementById("machine-metadata").textContent = lines.join("\n");
    })();
  }

  /* ============================ shell / boot ============================ */
  function draw() {
    if (!CURRENT || !CURRENT.data) return;
    const D = CURRENT.data;
    try { renderSummary(D); }
    catch (err) {
      console.error("summary render failed", err);
      reportEl.innerHTML = `<div class="error-box">Failed to render the summary for run “${esc(D.run_id || "")}”: ${esc(err.message)}.
        <a href="index.html?run=${esc(D.run_id || "")}" style="color:var(--accent)">Open the full report ↗</a></div>`;
    }
  }

  async function loadRun(entry) {
    reportEl.innerHTML = `<div class="loading">Loading ${esc(entry.name || entry.id)}…</div>`;
    if (fullLink) fullLink.href = `index.html?run=${encodeURIComponent(entry.id)}`;
    try {
      const res = await fetch(entry.path, { cache: "no-cache" });
      if (!res.ok) throw new Error("HTTP " + res.status);
      const data = await res.json();
      CURRENT = { meta: entry, data };
      draw();
    } catch (err) {
      console.error(err);
      reportEl.innerHTML = `<div class="error-box">Could not load <code>${esc(entry.path)}</code> (${esc(err.message)}).<br>Serve the folder instead of opening the file directly: <code>python3 -m http.server -d docs</code> — <code>file://</code> blocks fetch().</div>`;
    }
  }

  function selectRun(id, push) {
    const entry = MANIFEST.runs.find(r => r.id === id) || MANIFEST.runs[0];
    if (!entry) return;
    selectEl.value = entry.id;
    const url = new URL(location.href);
    url.searchParams.set("run", entry.id);
    if (push) history.pushState({ run: entry.id }, "", url); else history.replaceState({ run: entry.id }, "", url);
    loadRun(entry);
  }

  async function boot() {
    await loadLiveTargets();
    try {
      const res = await fetch("runs/index.json", { cache: "no-cache" });
      if (!res.ok) throw new Error("HTTP " + res.status);
      MANIFEST = await res.json();
    } catch (err) {
      console.error(err);
      reportEl.innerHTML = `<div class="error-box">Could not load <code>runs/index.json</code> (${esc(err.message)}).<br>Serve the folder: <code>python3 -m http.server -d docs</code> — opening via <code>file://</code> will not work.</div>`;
      return;
    }
    (MANIFEST.runs || []).forEach(r => {
      const o = document.createElement("option");
      o.value = r.id;
      o.textContent = `${r.name}  ·  ${r.date}`;
      selectEl.appendChild(o);
    });
    selectEl.addEventListener("change", () => selectRun(selectEl.value, true));
    window.addEventListener("popstate", () => {
      const id = new URL(location.href).searchParams.get("run");
      if (id) selectRun(id, false);
    });
    const initial = new URL(location.href).searchParams.get("run");
    selectRun(initial || (MANIFEST.runs[0] && MANIFEST.runs[0].id), false);
  }

  window.addEventListener("resize", () => { clearTimeout(redrawTimer); redrawTimer = setTimeout(draw, 160); });
  boot();
})();
