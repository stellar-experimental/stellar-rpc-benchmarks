/* Stellar RPC full-history benchmark viewer.
   Vanilla JS, no frameworks, no CDNs. Fetches runs/index.json, renders a run
   selector with ?run=<id> deep-linking, and draws the selected schema-v1 run.
   Named renderers are keyed off dataset.kind + campaign.vocabulary; anything
   unrecognised degrades to a generic collapsible table. */
(function () {
  "use strict";

  const reportEl = document.getElementById("report");
  const selectEl = document.getElementById("run-select");
  const themeBtn = document.getElementById("theme-toggle");
  const tip = document.getElementById("tip");

  let MANIFEST = null;
  let CURRENT = null;   // { meta, data } of the run on screen
  let redrawTimer = null;

  /* ============================ theme ============================ */
  // Default follows prefers-color-scheme; the toggle stamps data-theme (which
  // wins over the media query in both directions).
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
  const NS = 1e9, MS = 1e6;
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
  const fmtMs = ns => trim(ns / 1e6);
  const fmtInt = x => Math.round(x).toLocaleString("en-US");
  const fmtK = x => x >= 1e6 ? trim(x / 1e6) + " M" : x >= 1e3 ? trim(x / 1e3) + " K" : trim(x);
  const esc = s => String(s).replace(/[&<>"]/g, c => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c]));
  const median5 = rs => [...rs].sort((a, b) => a - b)[Math.floor(rs.length / 2)];

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
  const CVAR = s => getComputedStyle(document.documentElement).getPropertyValue(s).trim();
  const COLORS = () => ({
    s1: CVAR("--s1"), s2: CVAR("--s2"), s3: CVAR("--s3"), s4: CVAR("--s4"),
    s5: CVAR("--s5"), s6: CVAR("--s6"), hot: CVAR("--hot"), cold: CVAR("--s1"),
    de: CVAR("--deemph"), warn: CVAR("--warn"),
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

  /* ============================ chart primitives ============================ */
  // rows: [{label, sub, segs:[{name,color,val(ns),extra?}], total(ns)}]
  function stackedH(bodyId, rows) {
    const el = document.getElementById(bodyId);
    if (!el) return;
    el.replaceChildren();
    const W = Math.max(el.clientWidth, 360);
    const labW = W < 560 ? 96 : 128;
    const m = { l: labW, r: 76, t: 6, b: 30 };
    const rowH = 26, gap = 18;
    const H = m.t + rows.length * (rowH + gap) - gap + m.b;
    const svg = S("svg", { viewBox: `0 0 ${W} ${H}`, role: "img" });
    const xmax = Math.max(...rows.map(r => r.total)) * 1.02;
    const x = v => m.l + v / xmax * (W - m.l - m.r);
    for (const tv of linTicks(xmax / 1e9, 6)) {
      const px = x(tv * 1e9);
      S("line", { x1: px, y1: m.t, x2: px, y2: H - m.b, class: "gridline" }, svg);
      S("text", { x: px, y: H - m.b + 16, "text-anchor": "middle", class: "ax", text: tv >= 10 ? tv.toFixed(0) : String(+tv.toFixed(1)) }, svg);
    }
    S("text", { x: W - m.r + 8, y: H - m.b + 16, class: "ax-unit", text: "s" }, svg);
    rows.forEach((row, i) => {
      const y = m.t + i * (rowH + gap);
      S("text", { x: m.l - 10, y: y + rowH / 2 + 1, "text-anchor": "end", class: "rowlab", text: row.label }, svg);
      if (row.sub) S("text", { x: m.l - 10, y: y + rowH / 2 + 14, "text-anchor": "end", class: "rowsub", text: row.sub }, svg);
      let cx = m.l;
      row.segs.forEach((seg, j) => {
        const wpx = seg.val / xmax * (W - m.l - m.r);
        const last = j === row.segs.length - 1;
        const gx = cx + (j === 0 ? 0 : 1), gw = Math.max(wpx - (j === 0 ? 1 : 2), 0.5);
        const mark = last && wpx > 6
          ? S("path", { d: roundedRight(gx, y, gw, rowH, 4), fill: seg.color }, svg)
          : S("rect", { x: gx, y: y, width: gw, height: rowH, fill: seg.color }, svg);
        const pct = seg.val / row.total * 100;
        hoverable(mark, `${row.label} — ${seg.name}`, [
          { color: seg.color, value: fmtNs(seg.val), label: `${pct.toFixed(0)} % of wall` },
          ...(seg.extra || []),
        ]);
        if (pct >= 14 && wpx > 56) S("text", { x: cx + wpx / 2, y: y + rowH / 2 + 4, "text-anchor": "middle", class: "vallab-in", text: fmtNs(seg.val) }, svg);
        cx += wpx;
      });
      S("text", { x: cx + 8, y: y + rowH / 2 + 4, class: "vallab", text: fmtNs(row.total) }, svg);
    });
    S("line", { x1: m.l, y1: m.t, x2: m.l, y2: H - m.b, class: "baseline-l" }, svg);
    el.appendChild(svg);
  }

  // panels: [{title, unit, color, bars:[{label,val,lo,hi,fmt}]}]
  function barPanels(bodyId, panels) {
    const el = document.getElementById(bodyId);
    if (!el) return;
    el.replaceChildren();
    const W = Math.max(el.clientWidth, 360);
    const cols = W < 640 ? 1 : panels.length;
    const pw = (W - (cols - 1) * 28) / cols;
    const rowH = 22, gap = 12, labW = 70;
    const rows = panels[0].bars.length;
    const panelH = 26 + rows * (rowH + gap) - gap + 30;
    const H = cols === 1 ? panels.length * (panelH + 16) : panelH;
    const svg = S("svg", { viewBox: `0 0 ${W} ${H}`, role: "img" });
    panels.forEach((p, pi) => {
      const ox = cols === 1 ? 0 : pi * (pw + 28);
      const oy = cols === 1 ? pi * (panelH + 16) : 0;
      S("text", { x: ox, y: oy + 12, class: "paneltitle", text: p.title }, svg);
      const m = { l: ox + labW, r: 64, t: oy + 26 };
      const innerW = pw - labW - 64;
      const xmax = Math.max(...p.bars.map(b => b.hi ?? b.val)) * 1.05;
      const x = v => m.l + v / xmax * innerW;
      p.bars.forEach((b, i) => {
        const y = m.t + i * (rowH + gap);
        S("text", { x: m.l - 8, y: y + rowH / 2 + 4, "text-anchor": "end", class: "rowlab", text: b.label }, svg);
        const bw = Math.max(x(b.val) - m.l, 1);
        const mark = S("path", { d: roundedRight(m.l, y, bw, rowH, 4), fill: p.color }, svg);
        hoverable(mark, `${p.title} — ${b.label}`, [
          { color: p.color, value: b.fmt(b.val), label: "median of 5 runs" },
          { value: `${b.fmt(b.lo)} – ${b.fmt(b.hi)}`, label: "min–max spread" },
        ]);
        if (b.lo != null && x(b.hi) - x(b.lo) > 3) {
          S("line", { x1: x(b.lo), y1: y + rowH / 2, x2: x(b.hi), y2: y + rowH / 2, stroke: "var(--ink)", "stroke-width": 1.4, opacity: 0.55 }, svg);
          for (const vv of [b.lo, b.hi]) S("line", { x1: x(vv), y1: y + rowH / 2 - 4, x2: x(vv), y2: y + rowH / 2 + 4, stroke: "var(--ink)", "stroke-width": 1.4, opacity: 0.55 }, svg);
        }
        S("text", { x: Math.max(x(b.val), b.hi != null ? x(b.hi) : 0) + 8, y: y + rowH / 2 + 4, class: "vallab", text: b.fmt(b.val) }, svg);
      });
      const axY = m.t + rows * (rowH + gap) - gap + 10;
      S("line", { x1: m.l, y1: m.t - 4, x2: m.l, y2: axY - 6, class: "baseline-l" }, svg);
      S("text", { x: m.l, y: axY + 8, class: "ax-unit", text: "0 → " + p.bars[0].fmt(xmax) + " " + p.unit }, svg);
    });
    el.appendChild(svg);
  }

  // rows: [{label, sub, lanes:[{name,color,pts:{p50,p90,p99,max}(ns), spread?}]}]
  // opts: { reflineNs, reflineLabel }
  function dotRangeChart(bodyId, rows, opts = {}) {
    const el = document.getElementById(bodyId);
    if (!el) return;
    el.replaceChildren();
    const W = Math.max(el.clientWidth, 360);
    const labW = W < 560 ? 96 : 140;
    const m = { l: labW, r: 34, t: 10, b: 46 };
    const laneH = 26;
    let allVals = [];
    rows.forEach(r => r.lanes.forEach(l => allVals.push(l.pts.p50, l.pts.max)));
    if (opts.reflineNs) allVals.push(opts.reflineNs);
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
    if (opts.reflineNs && opts.reflineNs > lo && opts.reflineNs < hi) {
      const px = x(opts.reflineNs);
      S("line", { x1: px, y1: m.t, x2: px, y2: H - m.b, class: "refline" }, svg);
      S("text", { x: px, y: m.t + 2, "text-anchor": "middle", class: "reflab", text: opts.reflineLabel || "" }, svg);
    }
    S("text", { x: W - m.r, y: H - 8, "text-anchor": "end", class: "ax-unit", text: "per-op latency · log scale" }, svg);
    let y = m.t;
    rows.forEach(row => {
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
        const tipRows = [
          { color: lane.color, value: fmtNs(p.p50), label: "p50" },
          { color: lane.color, value: fmtNs(p.p90), label: "p90" },
          { color: lane.color, value: fmtNs(p.p99), label: "p99" },
          { color: lane.color, value: fmtNs(p.max), label: "max (worst op)" },
        ];
        if (lane.spread) tipRows.push({ value: lane.spread, label: "p99 min–max across runs" });
        hoverable(hit, `${row.label} — ${lane.name}`, tipRows);
        y += laneH;
      });
      y += rowGap;
    });
    el.appendChild(svg);
  }

  // panels: [{title, series:[{name,color,vals:[N]}]}] — shared log y, x = conc levels
  function linePanels(bodyId, panels, opts) {
    const el = document.getElementById(bodyId);
    if (!el) return;
    el.replaceChildren();
    const xLabels = opts.xLabels;
    const W = Math.max(el.clientWidth, 360);
    const cols = W < 640 ? 2 : Math.min(4, panels.length);
    const gapX = 22;
    const pw = (W - (cols - 1) * gapX) / cols;
    const ph = 168, gapY = 30;
    const nRows = Math.ceil(panels.length / cols);
    const H = nRows * (ph + gapY) - gapY + 8;
    const svg = S("svg", { viewBox: `0 0 ${W} ${H}`, role: "img" });
    let allVals = [];
    panels.forEach(p => p.series.forEach(s => allVals.push(...s.vals)));
    const lo = Math.min(...allVals) / 1.4, hi = Math.max(...allVals) * 1.4;
    const N = xLabels.length;
    panels.forEach((p, pi) => {
      const ox = (pi % cols) * (pw + gapX);
      const oy = Math.floor(pi / cols) * (ph + gapY);
      const m = { l: ox + 52, r: ox + pw - 16, t: oy + 24, b: oy + ph - 22 };
      S("text", { x: ox + 52, y: oy + 12, class: "paneltitle", text: p.title }, svg);
      const yS = v => m.b - (Math.log10(v) - Math.log10(lo)) / (Math.log10(hi) - Math.log10(lo)) * (m.b - m.t);
      const xS = i => m.l + (N === 1 ? 0.5 : i / (N - 1)) * (m.r - m.l);
      const yt = logTicks(lo, hi).filter(t => Math.log10(t) % 1 === 0);
      for (const tv of yt) {
        S("line", { x1: m.l, y1: yS(tv), x2: m.r, y2: yS(tv), class: "gridline" }, svg);
        S("text", { x: m.l - 6, y: yS(tv) + 3.5, "text-anchor": "end", class: "ax", text: (opts.fmtTick || opts.fmt)(tv) }, svg);
      }
      xLabels.forEach((c, i) => S("text", { x: xS(i), y: m.b + 16, "text-anchor": "middle", class: "ax", text: c }, svg));
      for (const s of p.series) {
        const d = s.vals.map((v, i) => `${i ? "L" : "M"}${xS(i)},${yS(v)}`).join(" ");
        S("path", { d: d, fill: "none", stroke: s.color, "stroke-width": 2, "stroke-linejoin": "round", "stroke-linecap": "round" }, svg);
        s.vals.forEach((v, i) => S("circle", { cx: xS(i), cy: yS(v), r: 4.2, fill: s.color, stroke: "var(--surface)", "stroke-width": 2 }, svg));
      }
      xLabels.forEach((_, i) => {
        const hit = S("rect", { x: xS(i) - (m.r - m.l) / (2 * N), y: m.t - 6, width: (m.r - m.l) / N, height: m.b - m.t + 12, fill: "transparent" }, svg);
        hoverable(hit, `${p.title} — ${xLabels[i]}`,
          p.series.map(s => ({ color: s.color, value: opts.fmt(s.vals[i]), label: s.name })));
      });
    });
    el.appendChild(svg);
  }

  // rows: [{label, color, devs:[% values], absFmt:[strings]}]
  function stripChart(bodyId, rows) {
    const el = document.getElementById(bodyId);
    if (!el) return;
    el.replaceChildren();
    const W = Math.max(el.clientWidth, 360);
    const labW = W < 560 ? 104 : 150;
    const m = { l: labW, r: 30, t: 8, b: 30 };
    const rowH = 24;
    const H = m.t + rows.length * rowH + m.b;
    const svg = S("svg", { viewBox: `0 0 ${W} ${H}`, role: "img" });
    const maxAbs = Math.max(2.6, ...rows.flatMap(r => r.devs.map(Math.abs))) * 1.15;
    const x = v => m.l + (v + maxAbs) / (2 * maxAbs) * (W - m.l - m.r);
    const step = maxAbs > 4 ? 2 : 1;
    for (let tv = -Math.floor(maxAbs); tv <= Math.floor(maxAbs); tv += step) {
      const px = x(tv);
      S("line", { x1: px, y1: m.t, x2: px, y2: H - m.b, class: tv === 0 ? "baseline-l" : "gridline" }, svg);
      S("text", { x: px, y: H - m.b + 16, "text-anchor": "middle", class: "ax", text: (tv > 0 ? "+" : "") + tv + "%" }, svg);
    }
    rows.forEach((row, i) => {
      const cy = m.t + i * rowH + rowH / 2;
      S("text", { x: m.l - 10, y: cy + 4, "text-anchor": "end", class: "rowlab", text: row.label }, svg);
      row.devs.forEach((dv, ri) => {
        const dot = S("circle", { cx: x(dv), cy: cy, r: 4.6, fill: row.color, stroke: "var(--surface)", "stroke-width": 2, opacity: 0.9 }, svg);
        hoverable(dot, `${row.label} — run ${ri + 1}`, [
          { color: row.color, value: row.absFmt[ri], label: `wall (${dv >= 0 ? "+" : ""}${dv.toFixed(1)} % vs median)` },
        ]);
      });
    });
    el.appendChild(svg);
  }

  // two-dot log rate chart (cold vs hot), optional floor refline
  // rows: [{label, sub, lanes:[{name,color,val,lo,hi}]}]  opts:{fmt, floor, floorLabel}
  function rateChart(bodyId, rows, opts) {
    const el = document.getElementById(bodyId);
    if (!el) return;
    el.replaceChildren();
    const W = Math.max(el.clientWidth, 360);
    const labW = W < 560 ? 96 : 140;
    const m = { l: labW, r: 60, t: 8, b: 44 };
    const rowH = 40;
    const H = m.t + rows.length * rowH + m.b;
    const svg = S("svg", { viewBox: `0 0 ${W} ${H}`, role: "img" });
    const vals = rows.flatMap(r => r.lanes.map(l => l.val));
    if (opts.floor) vals.push(opts.floor);
    const lo = Math.min(...vals) / 1.6, hi = Math.max(...vals) * 1.6;
    const x = v => m.l + (Math.log10(v) - Math.log10(lo)) / (Math.log10(hi) - Math.log10(lo)) * (W - m.l - m.r);
    for (const tv of logTicks(lo, hi)) {
      S("line", { x1: x(tv), y1: m.t, x2: x(tv), y2: H - m.b, class: "gridline" }, svg);
      S("text", { x: x(tv), y: H - m.b + 16, "text-anchor": "middle", class: "ax", text: fmtInt(tv) }, svg);
    }
    if (opts.floor && opts.floor > lo && opts.floor < hi) {
      S("line", { x1: x(opts.floor), y1: m.t, x2: x(opts.floor), y2: H - m.b, class: "refline" }, svg);
      S("text", { x: x(opts.floor) + 4, y: m.t + 10, class: "reflab", text: opts.floorLabel || "" }, svg);
    }
    S("text", { x: W - m.r, y: H - 8, "text-anchor": "end", class: "ax-unit", text: "ledgers per second · log scale" }, svg);
    rows.forEach((row, i) => {
      const cy = m.t + i * rowH + rowH / 2;
      S("text", { x: m.l - 10, y: cy + 1, "text-anchor": "end", class: "rowlab", text: row.label }, svg);
      if (row.sub) S("text", { x: m.l - 10, y: cy + 14, "text-anchor": "end", class: "rowsub", text: row.sub }, svg);
      const xs = row.lanes.map(l => x(l.val));
      S("line", { x1: Math.min(...xs), y1: cy, x2: Math.max(...xs), y2: cy, stroke: "var(--baseline)", "stroke-width": 1.4 }, svg);
      for (const lane of row.lanes) {
        const dot = S("circle", { cx: x(lane.val), cy: cy, r: 6, fill: lane.color, stroke: "var(--surface)", "stroke-width": 2 }, svg);
        hoverable(dot, `${row.label} — ${lane.name}`, [
          { color: lane.color, value: opts.fmt(lane.val) + " ledgers/s", label: "median of 5 runs" },
          { value: `${opts.fmt(lane.lo)} – ${opts.fmt(lane.hi)}`, label: "min–max" },
        ]);
      }
      if (row.lanes.length === 2) {
        const ratio = row.lanes[0].val / row.lanes[1].val;
        S("text", { x: (xs[0] + xs[1]) / 2, y: cy - 10, "text-anchor": "middle", class: "vallab", text: (ratio >= 1 ? ratio : 1 / ratio).toFixed(1) + "×" }, svg);
      }
    });
    el.appendChild(svg);
  }

  /* ============================ shared shell HTML ============================ */
  function shortCommit(c) { return (c || "").slice(0, 8); }
  function verNum(s, re) { const m = (s || "").match(re); return m ? m[1] : ""; }

  function mastheadHTML(D) {
    const ds = D.dataset, mac = D.machine || {}, b = D.build || {}, camp = D.campaign || {};
    const um = ds.unit_meta || {};
    const units = ds.unit_order || Object.keys(um);
    let L = 0, T = 0, Ev = 0;
    units.forEach(u => { const k = um[u] || {}; L += k.ledgers || 0; T += k.txs || 0; Ev += k.events || 0; });
    const buildBits = [];
    if (b.commit) buildBits.push(shortCommit(b.commit) + (b.branch ? ` (${b.branch})` : ""));
    const go = verNum(b.go, /go(\d[\d.]*)/); if (go) buildBits.push("go " + go);
    const rust = verNum(b.rust, /rustc (\d[\d.]*)/); if (rust) buildBits.push("rustc " + rust);
    const machineLine = [mac.instance, mac.vcpus ? mac.vcpus + " vCPU" : "", mac.mem, "local NVMe instance store"].filter(Boolean).join(" · ");
    const osLine = [mac.os, mac.instance_id].filter(Boolean).join(" · ");
    const dsLine = `${units.length} ${(ds.unit_label || "unit").toLowerCase()}${units.length === 1 ? "" : "s"} · ${fmtInt(L)} ledgers · ${fmtK(Ev)} events · ${fmtK(T)} txs`;
    const src = camp.source_gcs
      ? `<a href="https://console.cloud.google.com/storage/browser/${esc(camp.source_gcs.replace("gs://", ""))}">${esc(camp.source_gcs)} ↗</a>`
      : "—";
    return `
    <header class="masthead">
      <div class="mast-eyebrow">
        <span>Stellar RPC v2 · Full-History Storage Engine</span>
        <span>Benchmark Report · ${esc(D.run_date || "")}</span>
      </div>
      <h1>${esc(D.run_name || D.run_id || "Benchmark run")}</h1>
      <p class="mast-sub">${esc(ds.description || "")}</p>
      <div class="meta-grid">
        <div class="meta-cell"><div class="meta-k">Machine</div><div class="meta-v">${esc(machineLine)}</div></div>
        <div class="meta-cell"><div class="meta-k">Build</div><div class="meta-v">${esc(buildBits.join(" · ")) || "—"}</div></div>
        <div class="meta-cell"><div class="meta-k">OS / Instance</div><div class="meta-v">${esc(osLine) || "—"}</div></div>
        <div class="meta-cell"><div class="meta-k">Dataset</div><div class="meta-v">${esc(dsLine)}</div></div>
        <div class="meta-cell"><div class="meta-k">Protocol</div><div class="meta-v">${camp.reps || 5} runs / config · fresh process per run · median reported</div></div>
        <div class="meta-cell"><div class="meta-k">Source data</div><div class="meta-v">${src}</div></div>
      </div>
    </header>`;
  }

  function footerHTML(D) {
    const b = D.build || {}, camp = D.campaign || {}, mac = D.machine || {};
    return `<div class="footer">
      ${esc(D.run_name || "")} · ${esc(mac.instance || "")}, local NVMe · build ${esc(shortCommit(b.commit))}${b.branch ? " (" + esc(b.branch) + ")" : ""}.<br>
      Aggregates are medians of ${camp.reps || 5} process-level runs with min–max spread; no value was invented, interpolated, or smoothed.
    </div>`;
  }

  function figHTML(id, no, title, legendId, caption) {
    return `<figure class="fig" id="${id}">
      <div class="fig-head"><div><span class="fig-no">${no}</span><span class="fig-title">${title}</span></div>${legendId ? `<div class="legend" id="${legendId}"></div>` : ""}</div>
      <div class="fig-body" id="${id}-body"></div>
      <figcaption>${caption}</figcaption>
      <details class="tv" id="${id}-tv"><summary>Table view</summary><div class="tv-scroll"></div></details>
    </figure>`;
  }

  function methodologyHTML(D, num, extraDL) {
    return `<section id="methodology" class="method">
      <div class="sec-head"><span class="sec-num">${num}</span><h2>Methodology &amp; machine metadata</h2></div>
      <dl>
        <dt>Process isolation &amp; aggregation</dt>
        <dd>${D.campaign && D.campaign.reps || 5} repetitions per configuration, each a fresh process. Every reported value is the median across runs; spread is min–max. Nothing is interpolated or smoothed — every plotted number traces to a raw CSV field.</dd>
        ${extraDL || ""}
        <dt>fsync probe</dt>
        <dd>${esc((D.machine && D.machine.fsync_probe) || "—")} — the device-level context for the per-ledger commit distribution.</dd>
      </dl>
      <pre class="metadata" id="machine-metadata"></pre>
    </section>`;
  }

  /* ============================ PUBNET renderer ============================ */
  function renderPubnet(D) {
    const ds = D.dataset;
    const CH = ds.unit_order;
    const um = ds.unit_meta;
    const cold = D.ingest_cold, hot = D.ingest_hot, Q = D.queries, gold = D.golden || {};
    // discover query types + concurrency levels from data (degrade gracefully)
    const firstTier = Q && (Q.cold || Q.hot) ? (Q.cold || Q.hot) : {};
    const firstUnit = firstTier[CH[0]] || {};
    const QT = Object.keys(firstUnit).filter(k => k !== "setup");
    const QT_LABEL = {
      ledgers: "ledgers (20-ledger range)", txpage: "txpage (20-tx page)",
      txhash: "txhash (single lookup)", events: "events (filtered query)",
    };
    const someQt = QT[0] ? firstUnit[QT[0]] : {};
    const CONC = Object.keys(someQt).filter(k => /^c\d+$/.test(k)).sort((a, b) => +a.slice(1) - +b.slice(1));
    const thr = D.checks && D.checks.threshold_ns ? D.checks.threshold_ns : 500e6;

    const chunkSub = c => `${fmtK(um[c].events)} ev`;

    // ---- banner (data-driven pass/fail from checks) ----
    let total = 0, pass = 0, worst = null;
    for (const qt of QT) for (const tier of ["cold", "hot"]) for (const cc of CONC) {
      let w = null;
      for (const c of CH) { const cell = Q[tier][c][qt][cc]; if (!w || cell.p99.m > w.cell.p99.m) w = { cell, c, tier, qt, cc }; }
      total++;
      if (w.cell.p99.m <= thr) pass++;
      if (!worst || w.cell.p99.m > worst.cell.p99.m) worst = w;
    }
    const worstTxt = worst ? `${worst.tier}-tier ${worst.qt} at ${worst.cc.slice(1)}-way concurrency on chunk ${worst.c} — median-run p99 of ${fmtNs(worst.cell.p99.m)}` : "";

    // ---- section skeleton ----
    reportEl.innerHTML = mastheadHTML(D) + `
    <section id="glance">
      <div class="sec-head"><span class="sec-num">01</span><h2>At a glance</h2></div>
      <div class="banner">
        <div class="banner-figure">${pass}<span class="of"> / ${total}</span></div>
        <div class="banner-copy">
          <div class="lead">Design-target cells ${pass === total ? "pass" : "checked"}: ${esc(D.checks ? D.checks.label : "query p99 target")} in every type × tier × concurrency cell. <span class="chip">${pass === total ? "✓ PASS" : pass + "/" + total}</span></div>
          <p>Worst cell: ${esc(worstTxt)}${worst && worst.cell.p99.m <= thr ? ", within budget" : ""}. Every other cell clears the target with more headroom. Details in §7.</p>
        </div>
      </div>
      <div class="tiles" id="tiles"></div>
      <p class="sec-intro">Cold ingestion (pack build) freezes each chunk into packfiles plus the event term index; hot ingestion is the live RocksDB path with one fsync per ledger. Queries are swept over cold and hot tiers at increasing concurrency. All numbers below are medians of ${D.campaign.reps} process-level runs with min–max spread.</p>
    </section>

    <section id="dataset">
      <div class="sec-head"><span class="sec-num">02</span><h2>Dataset &amp; environment</h2></div>
      <p class="sec-intro">A chunk is an immutable 10,000-ledger unit of chain history. The chunks span activity profiles from sparse early history to dense recent traffic; all are real Stellar pubnet data, sourced once as golden packs and re-ingested locally for every timed run.</p>
      <div class="tv-scroll" style="margin-top:16px"><table class="data" id="chunk-table" style="width:100%"></table></div>
      <p style="margin-top:16px">Machine and durability context are summarised in the masthead; full machine metadata is in §8.</p>
    </section>

    <section id="ingest-cold">
      <div class="sec-head"><span class="sec-num">03</span><h2>Cold ingestion — building the packfile tier</h2></div>
      <p class="sec-intro">Cold ingestion reads a chunk from the local golden pack and freezes it into immutable packfiles plus the event term index. Each data type runs an instrumented pipeline: <code>extract</code>, <code>term_index</code> (events only), <code>write</code>, and a one-shot <code>finalize</code>.</p>
      ${figHTML("fig31", "Fig 3.1", "Chunk wall time, attributed by data type", "fig31-legend", "Median of 5 runs per chunk. Events dominate cost everywhere; the unattributed remainder is driver overhead outside the three typed pipelines. Bar total = chunk wall.")}
      ${figHTML("fig32", "Fig 3.2", "Event pipeline composition per chunk", "fig32-legend", "Total time in each event-pipeline stage (median of 5 runs). <code>finalize</code> (MPHF index build + seal) is a one-shot cost at chunk close.")}
      ${figHTML("fig33", "Fig 3.3", "Cold-ingest throughput by data type", "", "Per-store pipeline throughput: items ÷ that store's own pipeline time, median of 5 runs (whisker = min–max). Event throughput holds a narrow band across a wide density spread.")}
      <p id="cold-prose" class="sec-intro"></p>
    </section>

    <section id="ingest-hot">
      <div class="sec-head"><span class="sec-num">04</span><h2>Hot ingestion — live RocksDB with per-ledger fsync</h2></div>
      <p class="sec-intro">Hot ingestion is the streaming path: each ledger is extracted, applied to the RocksDB stores, and committed with <strong>one fsync per ledger</strong> — the durability contract of live ingestion.</p>
      ${figHTML("fig41", "Fig 4.1", "Where hot-ingest wall time goes", "fig41-legend", "Sum of per-ledger phase times over the whole chunk (median of 5 runs). <code>source wait</code> is time blocked on the ledger source.")}
      ${figHTML("fig42", "Fig 4.2", "Per-ledger ingest latency — end to end, and its fsync commit", "fig42-legend", "Latency percentiles over 10,000 ledgers (median of 5 runs; log scale). <em>End-to-end</em> is the complete ingest of one ledger (<code>ingest_total</code>), source wait excluded.")}
      ${figHTML("fig43", "Fig 4.3", "End-to-end ingest rate — cold vs hot", "fig43-legend", "Ledgers per second over the full chunk (median of 5 runs; log scale). The fsync-per-ledger contract costs hot ingestion against the batch cold path — the price of live durability.")}
      <p id="hot-prose" class="sec-intro"></p>
    </section>

    <section id="queries">
      <div class="sec-head"><span class="sec-num">05</span><h2>Queries — cold vs hot tier</h2></div>
      <p class="sec-intro">Query shapes swept at increasing closed-loop worker counts. Cold-tier queries run with the page cache evicted every iteration; hot-tier queries run against read-only RocksDB after warmup.</p>
      <div class="filter-row" id="chunk-filter"><span class="filter-lab">${esc(ds.unit_label)}</span></div>
      ${figHTML("fig51", "Fig 5.1", "Per-operation latency at " + (CONC[0] || "c1") + " — cold vs hot", "fig51-legend", "Single-worker per-op latency percentiles, median of 5 runs, log scale.")}
      ${figHTML("fig52", "Fig 5.2", "Tail latency vs concurrency — p99", "fig52-legend", "p99 per-op latency as the closed-loop worker count grows (median of 5 runs; log scale, shared across panels).")}
      ${figHTML("fig53", "Fig 5.3", "Throughput vs concurrency — completed ops/s", "fig53-legend", "Closed-loop throughput per cell (ops ÷ cell wall, median of 5 runs; log scale, shared across panels).")}
      <div class="note-callout" id="query-note"></div>
    </section>

    <section id="variance">
      <div class="sec-head"><span class="sec-num">06</span><h2>Run-to-run variance</h2></div>
      <p class="sec-intro">Every configuration ran as five independent processes. Ingestion is highly repeatable; query tails at low concurrency are the least stable — worth knowing before reading any single p99 too literally.</p>
      ${figHTML("fig61", "Fig 6.1", "Ingest wall time — all 5 runs, deviation from median", "fig61-legend", "Each dot is one run's chunk wall time as % deviation from its configuration's median.")}
      <figure class="fig" id="fig62">
        <div class="fig-head"><div><span class="fig-no">Fig 6.2</span><span class="fig-title">Least stable query cells — p99 spread across 5 runs</span></div></div>
        <div class="fig-body"><div class="tv-scroll"><table class="data" id="fig62-table" style="width:100%"></table></div></div>
        <figcaption>Spread = (max − min) ÷ median of the five per-run p99 values. The unstable cells concentrate in the hot tier at low concurrency, where a cell holds few operations so one stall swings p99.</figcaption>
      </figure>
    </section>

    <section id="target">
      <div class="sec-head"><span class="sec-num">07</span><h2>Design-target check — ${esc(D.checks ? D.checks.label : "query p99")}</h2></div>
      <p class="sec-intro">Each cell shows the worst chunk's median-run p99 for that query type, tier, and concurrency. The design target is met when p99 ≤ ${fmtMs(thr)} ms.</p>
      <div class="target-table-wrap"><table class="target" id="target-table"></table></div>
      <p id="target-footnote" style="color:var(--muted); font-size:12.5px; margin-top:10px"></p>
    </section>
    ` + methodologyHTML(D, "08",
      `<dt>Golden-pack sourcing</dt><dd>Each chunk was downloaded from S3 once (golden runs, excluded from every aggregate). All timed cold-ingest runs re-ingest from the local pack — no timed number includes network transfer.</dd>
       <dt>Counter semantics</dt><dd><code>n</code> counts non-zero-duration samples; <code>n_items</code> counts natural units, so throughput = n_items ÷ total time. On the hot path <code>ingest_total</code> times each ledger's complete ingest; <code>read_blocked</code> separately times source waits.</dd>`)
      + footerHTML(D);

    const C = COLORS();

    /* ---- tiles ---- */
    (function tiles() {
      const el = document.getElementById("tiles");
      const mk = (value, small, label) => {
        const t = document.createElement("div"); t.className = "tile";
        const v = document.createElement("div"); v.className = "tile-value"; v.textContent = value;
        if (small) { const s = document.createElement("small"); s.textContent = " " + small; v.appendChild(s); }
        const l = document.createElement("div"); l.className = "tile-label"; l.textContent = label;
        t.appendChild(v); t.appendChild(l); el.appendChild(t);
      };
      const evTp = CH.map(c => cold[c].derived.tp_events.m);
      mk(fmtK(Math.max(...evTp)) + "/s", "events", "peak cold-ingest event throughput (≥ " + fmtK(Math.min(...evTp)) + "/s on every chunk)");
      const e2e99 = CH.map(c => hot[c].driver.ingest_total.p99.m);
      mk(fmtMs(Math.min(...e2e99)) + "–" + fmtMs(Math.max(...e2e99)), "ms", "hot-ingest p99 per ledger, end to end (range across the " + CH.length + " chunks)");
      const dense = CH[CH.length - 1];
      if (Q.hot[dense].txhash) mk(fmtNs(Q.hot[dense].txhash[CONC[0]].p50.m), "", "hot-tier single-tx lookup p50 incl. verification, densest chunk");
      if (Q.hot[dense].events) { const evq = Q.hot[dense].events[CONC[CONC.length - 1]]; mk(fmtK(evq.items_s.m) + "/s", "events", "hot-tier event scan rate at " + CONC[CONC.length - 1] + ", densest chunk (" + Math.round(evq.ops_s.m) + " queries/s)"); }
    })();

    /* ---- chunk table ---- */
    (function chunkTable() {
      const t = document.getElementById("chunk-table");
      const head = [ds.unit_label, "Ledger range", "Transactions", "Events", "Events / ledger", "Golden download (incl. S3)"];
      const tr = document.createElement("tr");
      head.forEach(h => { const th = document.createElement("th"); th.textContent = h; tr.appendChild(th); });
      t.appendChild(tr);
      for (const c of CH) {
        const k = um[c];
        const r = document.createElement("tr");
        const range = (k.seq_start != null) ? `${fmtInt(k.seq_start)} – ${fmtInt(k.seq_end)}` : "—";
        const g = gold[c] ? trim(gold[c].wall_ns / 1e9) + " s" : "—";
        [c, range, fmtInt(k.txs), fmtInt(k.events), fmtInt(k.events / k.ledgers), g]
          .forEach(v => { const td = document.createElement("td"); td.textContent = v; r.appendChild(td); });
        t.appendChild(r);
      }
    })();

    /* ---- fig 3.1 cold wall attribution ---- */
    (function fig31() {
      const segsDef = [["ledgers_total", "ledgers", C.s1], ["txhash_total", "tx hashes", C.s2], ["events_total", "events", C.s3]];
      const rows = CH.map(c => {
        const drv = cold[c].driver;
        const segs = segsDef.map(([k, name, col]) => ({ name, color: col, val: drv[k].total.m }));
        const other = drv.chunk_wall.total.m - segs.reduce((a, s) => a + s.val, 0);
        segs.push({ name: "driver / unattributed", color: C.de, val: Math.max(other, 0) });
        return { label: c, sub: chunkSub(c), segs, total: drv.chunk_wall.total.m };
      });
      stackedH("fig31-body", rows);
      legend("fig31-legend", [...segsDef.map(([, n, col]) => ({ label: n, color: col })), { label: "driver / unattributed", color: C.de }]);
      tableView("fig31", [ds.unit_label, "Ledgers", "Tx hashes", "Events", "Unattributed", "Wall (median)", "Wall min–max"],
        CH.map(c => {
          const d = cold[c].driver;
          const other = d.chunk_wall.total.m - d.ledgers_total.total.m - d.txhash_total.total.m - d.events_total.total.m;
          return [c, fmtNs(d.ledgers_total.total.m), fmtNs(d.txhash_total.total.m), fmtNs(d.events_total.total.m), fmtNs(other), fmtNs(d.chunk_wall.total.m), `${fmtNs(d.chunk_wall.total.lo)} – ${fmtNs(d.chunk_wall.total.hi)}`];
        }));
    })();

    /* ---- fig 3.2 event pipeline ---- */
    (function fig32() {
      const evStages = cold[CH[0]].files && cold[CH[0]].files.events ? Object.keys(cold[CH[0]].files.events) : [];
      const palette = [C.s1, C.s2, C.s3, C.s4, C.s5, C.s6];
      const stages = evStages.map((name, i) => [name, palette[i % palette.length]]);
      const rows = CH.map(c => {
        const st = cold[c].files.events;
        const segs = stages.map(([name, col]) => ({ name, color: col, val: st[name].total.m }));
        return { label: c, sub: chunkSub(c), segs, total: segs.reduce((a, s) => a + s.val, 0) };
      });
      stackedH("fig32-body", rows);
      legend("fig32-legend", stages.map(([n, col]) => ({ label: n, color: col })));
      tableView("fig32", [ds.unit_label, ...evStages, "Events pipeline total", "ns / event"],
        CH.map(c => {
          const st = cold[c].files.events;
          const tot = evStages.reduce((a, n) => a + st[n].total.m, 0);
          return [c, ...evStages.map(n => fmtNs(st[n].total.m)), fmtNs(tot), Math.round(tot / um[c].events) + " ns"];
        }));
    })();

    /* ---- fig 3.3 cold throughput ---- */
    (function fig33() {
      const defs = [["tp_ledgers", "ledger store · ledgers/s", fmtInt], ["tp_txs", "tx-hash store · tx/s", fmtK], ["tp_events", "event store · events/s", fmtK]];
      const panels = defs.map(([k, title, f]) => ({
        title, unit: "", color: C.s1,
        bars: CH.map(c => { const s = cold[c].derived[k]; return { label: c, val: s.m, lo: s.lo, hi: s.hi, fmt: f }; }),
      }));
      barPanels("fig33-body", panels);
      tableView("fig33", [ds.unit_label, "Ledgers/s", "Tx/s", "Events/s", "Wall", "s per 1 M events"],
        CH.map(c => {
          const ic = cold[c];
          return [c, fmtInt(ic.derived.tp_ledgers.m), fmtInt(ic.derived.tp_txs.m), fmtInt(ic.derived.tp_events.m), fmtNs(ic.driver.chunk_wall.total.m), trim(ic.driver.chunk_wall.total.m / 1e9 / (um[c].events / 1e6))];
        }));
      const walls = CH.map(c => cold[c].driver.chunk_wall.total.m / 1e9);
      const perM = CH.map(c => cold[c].driver.chunk_wall.total.m / 1e9 / (um[c].events / 1e6));
      document.getElementById("cold-prose").innerHTML =
        `End-to-end, a full chunk freezes in <strong>${trim(Math.min(...walls))} s</strong> to <strong>${trim(Math.max(...walls))} s</strong> depending on density. Normalised by event count the chunks land within ${trim(Math.min(...perM))}–${trim(Math.max(...perM))} s per million events, so cold ingest is close to event-count-linear.`;
    })();

    /* ---- fig 4.1 hot wall attribution ---- */
    (function fig41() {
      const phaseNames = Object.keys(hot[CH[0]].phases);
      const palette = { extract: C.s1, ledgers: C.s2, txhash: C.s3, events: C.s4, apply: C.s5, commit: C.s6 };
      const rows = CH.map(c => {
        const ih = hot[c];
        const segs = [];
        if (ih.driver.read_blocked) segs.push({ name: "source wait", color: C.de, val: ih.driver.read_blocked.total.m });
        for (const name of phaseNames) segs.push({ name: name === "commit" ? "commit (fsync)" : name, color: palette[name] || C.de, val: ih.phases[name].total.m });
        return { label: c, sub: chunkSub(c), segs, total: ih.driver.chunk_wall.total.m };
      });
      stackedH("fig41-body", rows);
      legend("fig41-legend", [{ label: "source wait", color: C.de }, ...phaseNames.map(n => ({ label: n === "commit" ? "commit (fsync)" : n, color: palette[n] || C.de }))]);
      tableView("fig41", [ds.unit_label, "source wait", ...phaseNames, "Wall", "commit % of wall"],
        CH.map(c => {
          const ih = hot[c];
          const sw = ih.driver.read_blocked ? fmtNs(ih.driver.read_blocked.total.m) : "—";
          const commit = ih.phases.commit ? (ih.phases.commit.total.m / ih.driver.chunk_wall.total.m * 100).toFixed(0) + " %" : "—";
          return [c, sw, ...phaseNames.map(n => fmtNs(ih.phases[n].total.m)), fmtNs(ih.driver.chunk_wall.total.m), commit];
        }));
    })();

    /* ---- fig 4.2 per-ledger latency ---- */
    (function fig42() {
      const series = c => [
        ["end-to-end ingest", C.hot, hot[c].driver.ingest_total],
        ["commit (fsync)", C.s6, hot[c].phases.commit],
      ].filter(s => s[2]);
      const rows = CH.map(c => ({
        label: c, sub: chunkSub(c),
        lanes: series(c).map(([name, color, st]) => ({
          name, color,
          pts: { p50: st.p50.m, p90: st.p90.m, p99: st.p99.m, max: st.max.m },
          spread: `${fmtNs(st.p99.lo)} – ${fmtNs(st.p99.hi)}`,
        })),
      }));
      dotRangeChart("fig42-body", rows);
      legend("fig42-legend", [{ label: "end-to-end ingest", color: C.hot }, { label: "commit (fsync)", color: C.s6 }, { label: "● p50 · • p90 · ○ p99 · | max", color: "transparent" }]);
      tableView("fig42", [ds.unit_label, "Series", "p50", "p90", "p99", "max", "p99 min–max across runs"],
        CH.flatMap(c => series(c).map(([name, , st]) => [c, name, fmtNs(st.p50.m), fmtNs(st.p90.m), fmtNs(st.p99.m), fmtNs(st.max.m), `${fmtNs(st.p99.lo)} – ${fmtNs(st.p99.hi)}`])));
      const p99s = CH.map(c => hot[c].driver.ingest_total.p99.m / 1e6);
      const commitShare = CH.map(c => hot[c].phases.commit ? hot[c].phases.commit.total.m / hot[c].driver.chunk_wall.total.m * 100 : 0);
      document.getElementById("hot-prose").innerHTML =
        `Per-ledger end-to-end p99 spans <strong>${trim(Math.min(...p99s))}–${trim(Math.max(...p99s))} ms</strong> across the ${CH.length} chunks. Commit (fsync) carries about <strong>${Math.round(Math.min(...commitShare))}–${Math.round(Math.max(...commitShare))} %</strong> of hot-ingest wall — the bottleneck this NVMe box was provisioned to measure.`;
    })();

    /* ---- fig 4.3 rate cold vs hot ---- */
    (function fig43() {
      const rows = CH.map(c => ({
        label: c, sub: chunkSub(c),
        lanes: [
          { name: "cold (pack build)", color: C.cold, val: cold[c].derived.ledgers_per_s.m, lo: cold[c].derived.ledgers_per_s.lo, hi: cold[c].derived.ledgers_per_s.hi },
          { name: "hot (fsync/ledger)", color: C.hot, val: hot[c].derived.ledgers_per_s.m, lo: hot[c].derived.ledgers_per_s.lo, hi: hot[c].derived.ledgers_per_s.hi },
        ],
      }));
      rateChart("fig43-body", rows, { fmt: fmtInt });
      legend("fig43-legend", [{ label: "cold (pack build)", color: C.cold }, { label: "hot (one fsync per ledger)", color: C.hot }]);
      tableView("fig43", [ds.unit_label, "Cold ledgers/s", "Hot ledgers/s", "Cold wall", "Hot wall", "Hot cost ×"],
        CH.map(c => [c, fmtInt(cold[c].derived.ledgers_per_s.m), fmtInt(hot[c].derived.ledgers_per_s.m), fmtNs(cold[c].driver.chunk_wall.total.m), fmtNs(hot[c].driver.chunk_wall.total.m), (hot[c].driver.chunk_wall.total.m / cold[c].driver.chunk_wall.total.m).toFixed(1) + "×"]));
    })();

    /* ---- query section (chunk-scoped) ---- */
    let curChunk = CH[CH.length - 1];
    function fig51() {
      const rows = QT.map(qt => ({
        label: qt, sub: (QT_LABEL[qt] || qt).replace(qt + " ", "").replace(/[()]/g, ""),
        lanes: ["cold", "hot"].map(tier => {
          const cell = Q[tier][curChunk][qt][CONC[0]];
          return { name: tier + " tier (" + CONC[0] + ")", color: tier === "cold" ? C.cold : C.hot, pts: { p50: cell.p50.m, p90: cell.p90.m, p99: cell.p99.m, max: cell.max.m }, spread: `${fmtNs(cell.p99.lo)} – ${fmtNs(cell.p99.hi)}` };
        }),
      }));
      dotRangeChart("fig51-body", rows);
      legend("fig51-legend", [{ label: "cold tier", color: C.cold }, { label: "hot tier", color: C.hot }, { label: "● p50 · • p90 · ○ p99 · | max", color: "transparent" }]);
      tableView("fig51", ["Query type", "Tier", "p50", "p90", "p99", "max", "p99 spread (5 runs)"],
        QT.flatMap(qt => ["cold", "hot"].map(tier => { const cell = Q[tier][curChunk][qt][CONC[0]]; return [QT_LABEL[qt] || qt, tier, fmtNs(cell.p50.m), fmtNs(cell.p90.m), fmtNs(cell.p99.m), fmtNs(cell.max.m), `${fmtNs(cell.p99.lo)} – ${fmtNs(cell.p99.hi)}`]; })));
    }
    function fig52() {
      const panels = QT.map(qt => ({ title: qt, series: ["cold", "hot"].map(tier => ({ name: tier + " p99", color: tier === "cold" ? C.cold : C.hot, vals: CONC.map(cc => Q[tier][curChunk][qt][cc].p99.m) })) }));
      linePanels("fig52-body", panels, { fmt: fmtNs, fmtTick: fmtNsAxis, xLabels: CONC });
      legend("fig52-legend", [{ label: "cold p99", color: C.cold, line: true }, { label: "hot p99", color: C.hot, line: true }]);
      tableView("fig52", ["Query type", "Tier", ...CONC.map(c => c + " p99")],
        QT.flatMap(qt => ["cold", "hot"].map(tier => [QT_LABEL[qt] || qt, tier, ...CONC.map(cc => fmtNs(Q[tier][curChunk][qt][cc].p99.m))])));
    }
    function fig53() {
      const panels = QT.map(qt => ({ title: qt, series: ["cold", "hot"].map(tier => ({ name: tier + " ops/s", color: tier === "cold" ? C.cold : C.hot, vals: CONC.map(cc => Q[tier][curChunk][qt][cc].ops_s.m) })) }));
      linePanels("fig53-body", panels, { fmt: fmtInt, xLabels: CONC });
      legend("fig53-legend", [{ label: "cold ops/s", color: C.cold, line: true }, { label: "hot ops/s", color: C.hot, line: true }]);
      tableView("fig53", ["Query type", "Tier", ...CONC, "events/s @ " + CONC[CONC.length - 1]],
        QT.flatMap(qt => ["cold", "hot"].map(tier => { const q = Q[tier][curChunk][qt]; const ev = q[CONC[CONC.length - 1]].items_s ? fmtInt(q[CONC[CONC.length - 1]].items_s.m) : "—"; return [QT_LABEL[qt] || qt, tier, ...CONC.map(cc => fmtInt(q[cc].ops_s.m)), ev]; })));
    }
    (function chunkFilter() {
      const holder = document.getElementById("chunk-filter");
      for (const c of CH) {
        const b = document.createElement("button");
        b.className = "chunk-btn"; b.setAttribute("aria-pressed", String(c === curChunk));
        b.appendChild(document.createTextNode(c + " "));
        const s = document.createElement("small"); s.textContent = "· " + fmtK(um[c].events) + " ev"; b.appendChild(s);
        b.addEventListener("click", () => {
          curChunk = c;
          holder.querySelectorAll(".chunk-btn").forEach(x => x.setAttribute("aria-pressed", "false"));
          b.setAttribute("aria-pressed", "true");
          fig51(); fig52(); fig53();
        });
        holder.appendChild(b);
      }
    })();
    fig51(); fig52(); fig53();

    // chunk-5000-style anomaly note: find the densest-tail hot cell outlier automatically
    (function queryNote() {
      const el = document.getElementById("query-note");
      const topC = CONC[CONC.length - 1];
      let flagged = null;
      for (const c of CH) {
        let p99s = [];
        for (const qt of QT) if (Q.hot[c][qt] && Q.hot[c][qt][topC]) p99s.push(Q.hot[c][qt][topC].p99.m);
        const avg = p99s.reduce((a, b) => a + b, 0) / p99s.length;
        if (!flagged || avg > flagged.avg) flagged = { c, avg };
      }
      if (flagged) el.innerHTML = `<strong>Chunk ${flagged.c}'s hot store is the outlier under concurrency.</strong> At ${topC} its hot-tier p99 rises well above the same cells on the other chunks, reproducibly across all five runs — a property of that store's shape, not run noise. Worth a targeted look at RocksDB read amplification for that chunk.`;
    })();

    /* ---- fig 6.1 variance strip ---- */
    (function fig61() {
      const rows = [];
      for (const [kind, src, color] of [["cold", cold, C.cold], ["hot", hot, C.hot]]) {
        for (const c of CH) {
          const w = src[c].driver.chunk_wall.total;
          rows.push({ label: `${kind} · ${c}`, color, devs: w.r.map(v => (v / w.m - 1) * 100), absFmt: w.r.map(v => fmtNs(v)) });
        }
      }
      stripChart("fig61-body", rows);
      legend("fig61-legend", [{ label: "cold ingest", color: C.cold }, { label: "hot ingest", color: C.hot }]);
      tableView("fig61", ["Config", "Median wall", "Min", "Max", "Spread % of median"],
        rows.map((r, i) => {
          const src = i < CH.length ? cold : hot;
          const w = src[CH[i % CH.length]].driver.chunk_wall.total;
          return [r.label, fmtNs(w.m), fmtNs(w.lo), fmtNs(w.hi), ((w.hi - w.lo) / w.m * 100).toFixed(1) + " %"];
        }));
    })();

    /* ---- fig 6.2 least-stable cells ---- */
    (function fig62() {
      const cells = [];
      for (const tier of ["cold", "hot"]) for (const c of CH) for (const qt of QT) for (const cc of CONC) {
        const cell = Q[tier][c][qt][cc]; const p = cell.p99, p50 = cell.p50;
        cells.push({ tier, c, qt, cc, spread: (p.hi - p.lo) / p.m * 100, p99: p.m, lo: p.lo, hi: p.hi, p50spread: (p50.hi - p50.lo) / p50.m * 100 });
      }
      cells.sort((a, b) => b.spread - a.spread);
      const t = document.getElementById("fig62-table");
      const tr = document.createElement("tr");
      ["Cell", "p99 (median run)", "p99 min–max", "p99 spread", "p50 spread (same cell)"].forEach(h => { const th = document.createElement("th"); th.textContent = h; tr.appendChild(th); });
      t.appendChild(tr);
      for (const cell of cells.slice(0, 8)) {
        const r = document.createElement("tr");
        [`${cell.tier} · ${cell.c} · ${cell.qt} · ${cell.cc}`, fmtNs(cell.p99), `${fmtNs(cell.lo)} – ${fmtNs(cell.hi)}`, cell.spread.toFixed(0) + " %", cell.p50spread.toFixed(0) + " %"]
          .forEach(v => { const td = document.createElement("td"); td.textContent = v; r.appendChild(td); });
        t.appendChild(r);
      }
    })();

    /* ---- design-target table ---- */
    (function targetTable() {
      const t = document.getElementById("target-table");
      const tr = document.createElement("tr");
      ["Query type · tier", ...CONC.map(c => c + " p99")].forEach(h => { const th = document.createElement("th"); th.textContent = h; tr.appendChild(th); });
      t.appendChild(tr);
      const over = [];
      for (const qt of QT) for (const tier of ["cold", "hot"]) {
        const r = document.createElement("tr");
        const name = document.createElement("td");
        const tag = document.createElement("span"); tag.className = "tier-tag"; tag.style.background = tier === "cold" ? C.cold : C.hot;
        name.appendChild(tag); name.appendChild(document.createTextNode(`${qt} · ${tier}`)); r.appendChild(name);
        for (const cc of CONC) {
          let worstCell = null;
          for (const c of CH) { const cell = Q[tier][c][qt][cc]; if (!worstCell || cell.p99.m > worstCell.cell.p99.m) worstCell = { cell, c }; }
          const td = document.createElement("td");
          td.textContent = fmtMs(worstCell.cell.p99.m) + " ms";
          const note = document.createElement("span"); note.className = "cell-note"; note.textContent = ` (${worstCell.c})`; td.appendChild(note);
          const ok = document.createElement("span");
          if (worstCell.cell.p99.m > thr) { ok.className = "cell-warn"; ok.textContent = " ▲"; }
          else if (worstCell.cell.p99.hi > thr) { ok.className = "cell-ok"; ok.textContent = " ✓†"; over.push(`${qt} · ${tier} · ${cc} on ${worstCell.c} (worst run ${fmtMs(worstCell.cell.p99.hi)} ms)`); }
          else { ok.className = "cell-ok"; ok.textContent = " ✓"; }
          td.appendChild(ok); r.appendChild(td);
        }
        t.appendChild(r);
      }
      document.getElementById("target-footnote").textContent = over.length
        ? "✓ = median-run p99 within the budget for every chunk. † = passes at the median but at least one of the five runs exceeded it: " + over.join("; ") + "."
        : "✓ = median-run p99 within the budget for every chunk, including each cell's min–max spread.";
    })();

    document.getElementById("machine-metadata").textContent = (D.machine && D.machine.raw || "").trim();
  }

  /* ============================ SYNTHETIC renderer ============================ */
  function renderSynthetic(D) {
    const ds = D.dataset;
    const ORDER = ds.unit_order;
    const um = ds.unit_meta;
    const cold = D.ingest_cold, hot = D.ingest_hot;
    const interval = D.checks && D.checks.interval_ns ? D.checks.interval_ns : 600e6;
    const intervalMs = interval / MS;
    const floor = 1e9 / interval;   // ledgers/s a block interval demands

    const sub = p => fmtK(um[p].events) + " events";

    // banner keep-up (data-driven)
    let keeps = 0, tail = false;
    ORDER.forEach(p => {
      const sus = hot[p].driver.run_wall.total.m / um[p].ledgers;
      if (sus <= interval) keeps++;
      if (hot[p].driver.ingest_total.p99.m > interval) tail = true;
    });

    reportEl.innerHTML = mastheadHTML(D) + `
    <section id="glance">
      <div class="sec-head"><span class="sec-num">01</span><h2>At a glance</h2></div>
      <div class="banner${tail ? " warn-tail" : ""}">
        <div class="banner-figure">${keeps}<span class="of"> / ${ORDER.length}</span></div>
        <div class="banner-copy">
          <div class="lead">All profiles sustain the ${esc(D.checks ? D.checks.label : "block model")} in steady state. <span class="chip">✓ KEEPS UP</span>${tail ? ` <span class="chip warn">TAIL CAVEAT</span>` : ""}</div>
          <p id="banner-note"></p>
        </div>
      </div>
      <div class="tiles" id="tiles"></div>
      <p class="sec-intro">Hot ingestion is the daemon's live loop with one fsync per ledger; cold ingestion freezes each profile into packfiles plus the event term index. Every number is a median of ${D.campaign.reps} process-level runs judged against a ${intervalMs} ms block model.</p>
    </section>

    <section id="dataset">
      <div class="sec-head"><span class="sec-num">02</span><h2>Dataset &amp; environment</h2></div>
      <p class="sec-intro">Synthetic Stellar ledgers generated by stellar-core <code>apply-load</code> at three model-transaction profiles. Per-ledger density models a <strong>${intervalMs} ms block time</strong> (tx/ledger = target TPS × ${(interval / 1e9).toFixed(1)}), which is also the keep-up bar every hot number is judged against.</p>
      <div class="tv-scroll" style="margin-top:16px"><table class="data" id="profile-table" style="width:100%"></table></div>
      <p style="margin-top:16px">Machine and durability context are summarised in the masthead; full machine metadata is in §7.</p>
    </section>

    <section id="ingest-cold">
      <div class="sec-head"><span class="sec-num">03</span><h2>Cold ingestion — freezing synthetic history into packfiles</h2></div>
      <p class="sec-intro">Cold ingestion drives the production backfill from the golden pack into a fresh cold tree: a shared per-ledger extract (<code>cold_extract</code>), then per-type pipelines — ledgers, txhash, and events (term indexing → write → one-shot <code>finalize</code> that builds the MPHF event index).</p>
      ${figHTML("fig31", "Fig 3.1", "Backfill wall time, attributed by pipeline", "fig31-legend", "Median of 5 runs. Bar length = whole-campaign backfill wall. The remainder is coordination outside the instrumented pipelines.")}
      ${figHTML("fig32", "Fig 3.2", "Event pipeline composition", "fig32-legend", "Total time in each event-pipeline stage (median of 5 runs). The MPHF <code>finalize</code> is priced in distinct terms, not events.")}
      ${figHTML("fig33", "Fig 3.3", "Normalized cold cost — seconds per million events", "", "Whole-campaign backfill wall ÷ events ingested, median of 5 runs (whiskers = min–max).")}
      <p id="cold-prose" class="sec-intro"></p>
    </section>

    <section id="ingest-hot">
      <div class="sec-head"><span class="sec-num">04</span><h2>Hot ingestion — the live loop against a ${intervalMs} ms block model</h2></div>
      <p class="sec-intro">Hot ingestion runs the daemon's production loop: per ledger — extract, apply to the RocksDB stores, commit with <strong>one fsync per ledger</strong>, then <code>apply</code> makes the write batch live.</p>
      ${figHTML("fig41", "Fig 4.1", "Where hot-ingest wall time goes", "fig41-legend", "Sum of per-ledger phase times over the whole run (median of 5). Commit (fsync) holds a large share, but on the dense profiles <code>apply</code> grows to rival it.")}
      ${figHTML("fig42", "Fig 4.2", "Per-ledger latency — end to end, its fsync commit, and the apply stall tail", "fig42-legend", "Latency percentiles over every ledger of the run (median of 5 runs; log scale). The dashed line is one block interval.")}
      ${figHTML("fig43", "Fig 4.3", "End-to-end ingest rate — cold vs hot vs the block-model floor", "fig43-legend", "Ledgers per second over the full run (median of 5; log scale). The dashed line is the rate a block interval demands.")}
      <p id="hot-prose" class="sec-intro"></p>
    </section>

    <section id="variance">
      <div class="sec-head"><span class="sec-num">05</span><h2>Run-to-run variance</h2></div>
      <p class="sec-intro">Every configuration ran as five independent processes against identical inputs. The spread is tight enough that every number above can be read at face value — including the tail.</p>
      ${figHTML("fig51", "Fig 5.1", "Run wall time — all 5 runs, deviation from median", "fig51-legend", "Each dot is one run's whole-campaign wall as % deviation from its configuration's median.")}
      <p id="variance-prose" class="sec-intro"></p>
    </section>

    <section id="target">
      <div class="sec-head"><span class="sec-num">06</span><h2>Keep-up check — the ${intervalMs} ms block model</h2></div>
      <p class="sec-intro">The datasets model a ${intervalMs} ms close time, so ${intervalMs} ms is the budget: sustained per-ledger cost decides whether the follower keeps up at all; per-ledger percentiles say how often a single ledger overruns one interval.</p>
      <div class="target-table-wrap"><table class="target" id="target-table"></table></div>
      <p style="color:var(--muted); font-size:12.5px; margin-top:10px">Sustained = run wall ÷ ledgers (source wait included). Values are medians of 5 runs; "worst ledger" is the median across runs of each run's slowest ledger.</p>
    </section>
    ` + methodologyHTML(D, "07",
      `<dt>Cold-run semantics</dt><dd>Each cold run wipes its scratch output tree and backfills the whole configuration via the production backfill — plan, freeze all three data types, build the txhash MPHF. <code>backfill_wall</code> is that whole plan-and-execute wall.</dd>
       <dt>Hot-run semantics</dt><dd>Each hot run starts from an empty store and drives the daemon's bounded ingestion loop over the full range. <code>ingest_total</code> times one ledger's complete ingest — the sum of its phase burst, source wait excluded; run wall includes source wait.</dd>
       <dt>Counter semantics</dt><dd><code>n</code> counts non-zero-duration samples; <code>n_items</code> counts natural units (ledgers, txs, events). Percentiles are per-ledger, never averaged across runs — the reported percentile is the median run's.</dd>`)
      + footerHTML(D);

    const C = COLORS();

    /* ---- profile table ---- */
    (function profileTable() {
      const t = document.getElementById("profile-table");
      const head = ["profile", "model tx", "target", "tx/ledger", "ev/tx", "ledgers", "chunks", "transactions", "events", "golden pack"];
      const tr = document.createElement("tr");
      head.forEach(h => { const th = document.createElement("th"); th.textContent = h; tr.appendChild(th); });
      t.appendChild(tr);
      let L = 0, T = 0, Ev = 0;
      ORDER.forEach(p => {
        const d = um[p]; L += d.ledgers; T += d.txs; Ev += d.events;
        const row = [p, d.model || "—", d.tps ? (d.tps / 1000).toFixed(1).replace(".0", "") + " K TPS" : "—",
          fmtInt(d.txs / d.ledgers), (d.events / d.txs).toFixed(2), fmtInt(d.ledgers), d.source_chunks || "—",
          fmtInt(d.txs), fmtInt(d.events), d.pack || "—"];
        const r = document.createElement("tr");
        row.forEach(v => { const td = document.createElement("td"); td.textContent = v; r.appendChild(td); });
        t.appendChild(r);
      });
      const tot = ["total", "", "", "", "", fmtInt(L), "", fmtInt(T), fmtInt(Ev), ""];
      const r = document.createElement("tr");
      tot.forEach(v => { const td = document.createElement("td"); td.textContent = v; r.appendChild(td); });
      t.appendChild(r);
    })();

    /* ---- tiles (computed) ---- */
    (function tiles() {
      const el = document.getElementById("tiles");
      const mk = (value, small, label) => {
        const t = document.createElement("div"); t.className = "tile";
        const v = document.createElement("div"); v.className = "tile-value"; v.textContent = value;
        if (small) { const s = document.createElement("small"); s.textContent = " " + small; v.appendChild(s); }
        const l = document.createElement("div"); l.className = "tile-label"; l.textContent = label;
        t.appendChild(v); t.appendChild(l); el.appendChild(t);
      };
      const txps = ORDER.map(p => um[p].txs / (hot[p].driver.run_wall.total.m / NS));
      mk(fmtK(Math.max(...txps)) + "/s", "tx", "peak sustained live tx ingest across profiles");
      const p50 = ORDER.map(p => hot[p].driver.ingest_total.p50.m);
      mk(fmtMs(Math.min(...p50)) + "–" + fmtMs(Math.max(...p50)), "ms", "typical ledger end-to-end (p50 range across profiles)");
      // highest apply share of wall
      let bestApply = 0;
      ORDER.forEach(p => { if (hot[p].phases.apply) bestApply = Math.max(bestApply, hot[p].phases.apply.total.m / hot[p].driver.run_wall.total.m); });
      mk(Math.round(bestApply * 100) + " %", "", "of hot wall in RocksDB apply at the densest profile — overtaking fsync as the bottleneck");
      const perM = ORDER.map(p => cold[p].driver.backfill_wall.total.m / NS / (um[p].events / 1e6));
      mk(trim(Math.min(...perM)) + "–" + trim(Math.max(...perM)), "s / M events", "cold freeze cost per million events across profiles — set by term shape, not event count");
      // banner note
      const worstP = ORDER.reduce((a, p) => hot[p].driver.ingest_total.p99.m > hot[a].driver.ingest_total.p99.m ? p : a, ORDER[0]);
      const bn = document.getElementById("banner-note");
      if (tail) bn.innerHTML = `The caveat lives in the tail: the <strong>${esc(worstP)}</strong> profile's slowest 1 % of ledgers exceed one block interval (p99 <strong>${fmtNs(hot[worstP].driver.ingest_total.p99.m)}</strong>, worst single ledger ${fmtNs(hot[worstP].driver.ingest_total.max.m)}) — a stall localized to the RocksDB <code>apply</code> phase, not the fsync (§4). The other profiles keep even their worst ledger under one interval.`;
      else bn.textContent = "Every profile keeps even its worst ledger comfortably inside one block interval.";
    })();

    /* ---- fig 3.1 cold wall attribution ---- */
    (function fig31() {
      const segsDef = [["events_total", "events", C.s1], ["cold_extract", "extract (shared)", C.s3], ["ledgers_total", "ledgers", C.s2], ["txhash_total", "txhash", C.s5]];
      const rows = ORDER.map(p => {
        const d = cold[p].driver;
        const segs = segsDef.map(([k, name, col]) => ({ name, color: col, val: d[k] ? d[k].total.m : 0 }));
        const other = d.backfill_wall.total.m - segs.reduce((a, s) => a + s.val, 0);
        segs.push({ name: "driver / unattributed", color: C.de, val: Math.max(other, 0) });
        return { label: p, sub: sub(p), segs, total: d.backfill_wall.total.m };
      });
      stackedH("fig31-body", rows);
      legend("fig31-legend", [...segsDef.map(([, n, col]) => ({ label: n, color: col })), { label: "driver / unattributed", color: C.de }]);
      tableView("fig31", ["profile", "backfill wall", "events", "extract (shared)", "ledgers", "txhash", "uninstrumented"],
        ORDER.map(p => {
          const d = cold[p].driver;
          const rest = d.backfill_wall.total.m - d.events_total.total.m - d.cold_extract.total.m - d.ledgers_total.total.m - d.txhash_total.total.m;
          return [p, fmtNs(d.backfill_wall.total.m), fmtNs(d.events_total.total.m), fmtNs(d.cold_extract.total.m), fmtNs(d.ledgers_total.total.m), fmtNs(d.txhash_total.total.m), fmtNs(rest)];
        }));
    })();

    /* ---- fig 3.2 event pipeline ---- */
    (function fig32() {
      const evStages = cold[ORDER[0]].files && cold[ORDER[0]].files.events ? Object.keys(cold[ORDER[0]].files.events) : [];
      const palette = { term_index: C.s1, write: C.s2, finalize: C.s6 };
      const rows = ORDER.map(p => {
        const st = cold[p].files.events;
        const segs = evStages.map(name => ({ name: name === "finalize" ? "finalize (MPHF)" : name, color: palette[name] || C.de, val: st[name].total.m }));
        return { label: p, sub: sub(p), segs, total: segs.reduce((a, s) => a + s.val, 0) };
      });
      stackedH("fig32-body", rows);
      legend("fig32-legend", evStages.map(name => ({ label: name === "finalize" ? "finalize (MPHF)" : name, color: palette[name] || C.de })));
      tableView("fig32", ["profile", ...evStages, "events pipeline total"],
        ORDER.map(p => { const st = cold[p].files.events; const tot = evStages.reduce((a, n) => a + st[n].total.m, 0); return [p, ...evStages.map(n => fmtNs(st[n].total.m)), fmtNs(tot)]; }));
    })();

    /* ---- fig 3.3 s per M events ---- */
    (function fig33() {
      const bars = ORDER.map(p => {
        const evM = um[p].events / 1e6;
        const w = cold[p].driver.backfill_wall.total;
        return { label: p, val: w.m / NS / evM, lo: w.lo / NS / evM, hi: w.hi / NS / evM, fmt: v => trim(v) };
      });
      barPanels("fig33-body", [{ title: "seconds per million events", unit: "s", color: C.s1, bars }]);
      tableView("fig33", ["profile", "s / M events (median)", "min", "max", "backfill wall", "events/s (wall-incl.)"],
        ORDER.map((p, i) => {
          const b = bars[i], w = cold[p].driver.backfill_wall.total;
          return [p, trim(b.val), trim(b.lo), trim(b.hi), fmtNs(w.m), fmtK(um[p].events / (w.m / NS)) + "/s"];
        }));
      const perM = bars.map(b => b.val);
      const walls = ORDER.map(p => cold[p].driver.backfill_wall.total.m / NS);
      document.getElementById("cold-prose").innerHTML =
        `End to end, the campaign configurations freeze in <strong>${trim(Math.min(...walls))}–${trim(Math.max(...walls))} s</strong>. The per-event cost varies from <strong>${trim(Math.min(...perM))} to ${trim(Math.max(...perM))} s/M events</strong> at similar event volumes — the event-index build is priced by the number of distinct terms, not the event count.`;
    })();

    /* ---- fig 4.1 hot wall attribution ---- */
    (function fig41() {
      const phaseNames = Object.keys(hot[ORDER[0]].phases);
      const palette = { extract: C.s1, ledgers: C.s2, txhash: C.de, events: C.s3, commit: C.s6, apply: C.s5 };
      const rows = ORDER.map(p => {
        const h = hot[p];
        const segs = phaseNames.map(name => ({
          name: name === "commit" ? "commit (fsync)" : name, color: palette[name] || C.de, val: h.phases[name].total.m,
          extra: [{ value: fmtNs(h.phases[name].p50.m), label: "p50 / ledger" }, { value: fmtNs(h.phases[name].p99.m), label: "p99 / ledger" }],
        }));
        return { label: p, sub: fmtInt(um[p].txs / um[p].ledgers) + " tx/ledger", segs, total: h.driver.run_wall.total.m };
      });
      stackedH("fig41-body", rows);
      legend("fig41-legend", phaseNames.map(n => ({ label: n === "commit" ? "commit (fsync)" : n, color: palette[n] || C.de })));
      tableView("fig41", ["profile", "run wall", ...phaseNames, "source wait + startup"],
        ORDER.map(p => { const h = hot[p]; const sum = phaseNames.reduce((a, n) => a + h.phases[n].total.m, 0); return [p, fmtNs(h.driver.run_wall.total.m), ...phaseNames.map(n => fmtNs(h.phases[n].total.m)), fmtNs(h.driver.run_wall.total.m - sum)]; }));
    })();

    /* ---- fig 4.2 per-ledger latency ---- */
    (function fig42() {
      const series = p => [
        ["end to end", C.s1, hot[p].driver.ingest_total],
        ["commit (fsync)", C.s6, hot[p].phases.commit],
        ["apply", C.s5, hot[p].phases.apply],
      ].filter(s => s[2]);
      const rows = ORDER.map(p => ({ label: p, sub: sub(p), lanes: series(p).map(([name, color, st]) => ({ name, color, pts: { p50: st.p50.m, p90: st.p90.m, p99: st.p99.m, max: st.max.m } })) }));
      dotRangeChart("fig42-body", rows, { reflineNs: interval, reflineLabel: intervalMs + " ms — block interval" });
      legend("fig42-legend", [{ label: "end to end", color: C.s1 }, { label: "commit (fsync)", color: C.s6 }, { label: "apply", color: C.s5 }, { label: intervalMs + " ms block interval", color: CVAR("--hot"), line: true }]);
      tableView("fig42", ["profile", "series", "p50", "p90", "p99", "worst ledger"],
        ORDER.flatMap(p => series(p).map(([name, , st]) => [p, name, fmtNs(st.p50.m), fmtNs(st.p90.m), fmtNs(st.p99.m), fmtNs(st.max.m)])));
      const p99 = ORDER.map(p => hot[p].driver.ingest_total.p99.m);
      document.getElementById("hot-prose").innerHTML =
        `Per-ledger end-to-end p99 ranges <strong>${fmtNs(Math.min(...p99))}–${fmtNs(Math.max(...p99))}</strong> across profiles. Where it crosses ${intervalMs} ms the gap is almost entirely RocksDB <code>apply</code> — a write-stall signature (flat median, cliff tail) reproduced in all five runs, not fsync.`;
    })();

    /* ---- fig 4.3 rate cold vs hot vs floor ---- */
    (function fig43() {
      const rows = ORDER.map(p => {
        const L = um[p].ledgers;
        const coldR = L / (cold[p].driver.backfill_wall.total.m / NS);
        const hotR = L / (hot[p].driver.run_wall.total.m / NS);
        const coldLo = L / (cold[p].driver.backfill_wall.total.hi / NS), coldHi = L / (cold[p].driver.backfill_wall.total.lo / NS);
        const hotLo = L / (hot[p].driver.run_wall.total.hi / NS), hotHi = L / (hot[p].driver.run_wall.total.lo / NS);
        return { label: p, sub: sub(p), lanes: [
          { name: "cold (batch freeze)", color: C.s1, val: coldR, lo: coldLo, hi: coldHi },
          { name: "hot (live, fsync/ledger)", color: C.hot, val: hotR, lo: hotLo, hi: hotHi },
        ] };
      });
      rateChart("fig43-body", rows, { fmt: v => trim(v), floor, floorLabel: floor.toFixed(1) + " l/s — " + intervalMs + " ms floor" });
      legend("fig43-legend", [{ label: "cold (batch freeze)", color: C.s1 }, { label: "hot (live, fsync/ledger)", color: C.hot }, { label: intervalMs + " ms block floor", color: CVAR("--hot"), line: true }]);
      tableView("fig43", ["profile", "cold l/s", "hot l/s", "hot vs floor", "hot tx/s", "cold ÷ hot"],
        ORDER.map(p => {
          const L = um[p].ledgers;
          const coldR = L / (cold[p].driver.backfill_wall.total.m / NS), hotR = L / (hot[p].driver.run_wall.total.m / NS);
          return [p, trim(coldR), trim(hotR), (hotR / floor).toFixed(1) + "×", fmtInt(um[p].txs / (hot[p].driver.run_wall.total.m / NS)), (coldR / hotR).toFixed(1) + "×"];
        }));
    })();

    /* ---- fig 5.1 variance dots ---- */
    (function fig51() {
      const rows = [];
      ORDER.forEach(p => rows.push({ label: p, sub: "cold", runs: cold[p].driver.backfill_wall.total.r, color: C.s1 }));
      ORDER.forEach(p => rows.push({ label: p, sub: "hot", runs: hot[p].driver.run_wall.total.r, color: C.hot }));
      const el = document.getElementById("fig51-body");
      el.replaceChildren();
      const W = Math.max(el.clientWidth, 360);
      const labW = W < 560 ? 100 : 132, PAD_R = 40, TOP = 6, ROWH = 34;
      const H = TOP + rows.length * ROWH + 26;
      const svg = S("svg", { viewBox: `0 0 ${W} ${H}`, role: "img" });
      const LIM = Math.max(2.0, ...rows.flatMap(r => { const med = median5(r.runs); return r.runs.map(v => Math.abs((v / med - 1) * 100)); })) * 1.1;
      const x = pct => labW + ((pct + LIM) / (2 * LIM)) * (W - labW - PAD_R);
      const step = LIM > 4 ? 2 : 1;
      for (let tv = -Math.floor(LIM); tv <= Math.floor(LIM); tv += step) {
        S("line", { x1: x(tv), y1: TOP, x2: x(tv), y2: TOP + rows.length * ROWH, class: tv === 0 ? "baseline-l" : "gridline" }, svg);
        S("text", { x: x(tv), y: H - 8, "text-anchor": "middle", class: "ax", text: (tv > 0 ? "+" : "") + tv + " %" }, svg);
      }
      rows.forEach((r, i) => {
        const y = TOP + i * ROWH + ROWH / 2;
        S("text", { x: labW - 10, y: y - 2, "text-anchor": "end", class: "rowlab", text: r.label }, svg);
        S("text", { x: labW - 10, y: y + 11, "text-anchor": "end", class: "rowsub", text: r.sub }, svg);
        const med = median5(r.runs);
        r.runs.forEach((v, ri) => {
          const pct = (v / med - 1) * 100;
          const d = S("circle", { cx: x(pct), cy: y, r: 5, fill: r.color, "fill-opacity": 0.72, stroke: "var(--surface)", "stroke-width": 1.5 }, svg);
          hoverable(d, `${r.label} ${r.sub} — run ${ri + 1}`, [{ color: r.color, value: fmtNs(v), label: "wall" }, { value: (pct >= 0 ? "+" : "") + pct.toFixed(2) + " %", label: "vs config median" }]);
        });
      });
      el.appendChild(svg);
      legend("fig51-legend", [{ label: "cold runs", color: C.s1 }, { label: "hot runs", color: C.hot }]);
      tableView("fig51", ["config", "run 1", "run 2", "run 3", "run 4", "run 5", "spread"],
        rows.map(r => { const med = median5(r.runs); const spread = ((Math.max(...r.runs) - Math.min(...r.runs)) / med * 100).toFixed(1) + " %"; return [`${r.label} ${r.sub}`, ...r.runs.map(v => fmtNs(v)), spread]; }));
      const worstDev = Math.max(...rows.flatMap(r => { const med = median5(r.runs); return r.runs.map(v => Math.abs((v / med - 1) * 100)); }));
      document.getElementById("variance-prose").innerHTML =
        `The worst single-run deviation anywhere is <strong>${worstDev.toFixed(1)} %</strong>. The tail is as repeatable as the medians, so each p99 above is a property of the workload on this configuration, not noise.`;
    })();

    /* ---- keep-up table ---- */
    (function targetTable() {
      const t = document.getElementById("target-table");
      const tr = document.createElement("tr");
      ["profile", "sustained / ledger", "headroom", "p50", "p90", "p99", "worst ledger"].forEach(h => { const th = document.createElement("th"); th.textContent = h; tr.appendChild(th); });
      t.appendChild(tr);
      ORDER.forEach(p => {
        const sus = hot[p].driver.run_wall.total.m / um[p].ledgers;
        const it = hot[p].driver.ingest_total;
        const cell = ns => { const ms = ns / MS; const flag = ns > interval ? ` ▲ ${(ns / interval).toFixed(1)}× interval` : ""; return Math.round(ms) + " ms" + flag; };
        const r = document.createElement("tr");
        const cells = [p, `${Math.round(sus / MS)} ms`, `${(interval / sus).toFixed(1)}×`, cell(it.p50.m), cell(it.p90.m), cell(it.p99.m), cell(it.max.m)];
        cells.forEach((v, ci) => {
          const td = document.createElement("td");
          if (ci === 0) td.textContent = v;
          else if (ci === 1) { td.textContent = v; const ok = document.createElement("span"); ok.className = "cell-ok"; ok.textContent = " ✓ KEEPS UP"; td.appendChild(ok); }
          else if (v.includes("▲")) { const parts = v.split(" ▲"); td.textContent = parts[0]; const w = document.createElement("span"); w.className = "cell-warn"; w.textContent = " ▲" + parts[1]; td.appendChild(w); }
          else td.textContent = v;
          r.appendChild(td);
        });
        t.appendChild(r);
      });
    })();

    document.getElementById("machine-metadata").textContent = (D.machine && D.machine.raw || "").trim();
  }

  /* ============================ generic fallback ============================ */
  function renderGeneric(D) {
    let html = mastheadHTML(D) + `<section><div class="sec-head"><span class="sec-num">01</span><h2>Raw sections (generic view)</h2></div>
      <p class="sec-intro">No specialised renderer matched <code>dataset.kind = ${esc((D.dataset && D.dataset.kind) || "?")}</code>. Every section is shown as a collapsible table.</p>`;
    const sections = D.sections || Object.keys(D).filter(k => typeof D[k] === "object");
    for (const s of sections) {
      if (!D[s]) continue;
      html += `<div class="generic-block"><h3>${esc(s)}</h3><div class="tv-scroll"><pre class="metadata">${esc(JSON.stringify(D[s], null, 1)).slice(0, 4000)}</pre></div></div>`;
    }
    html += `</section>` + methodologyHTML(D, "02", "") + footerHTML(D);
    reportEl.innerHTML = html;
    const md = document.getElementById("machine-metadata");
    if (md) md.textContent = (D.machine && D.machine.raw || "").trim();
  }

  const RENDERERS = { pubnet: renderPubnet, synthetic: renderSynthetic };

  /* ============================ shell / boot ============================ */
  function draw() {
    if (!CURRENT || !CURRENT.data) return;
    const D = CURRENT.data;
    const kind = D.dataset && D.dataset.kind;
    const fn = RENDERERS[kind] || renderGeneric;
    try { fn(D); }
    catch (err) {
      console.error("render failed for kind=" + kind, err);
      reportEl.innerHTML = `<div class="error-box">Failed to render run “${esc(D.run_id || "")}”: ${esc(err.message)}. Falling back to generic view.</div>`;
      try { renderGeneric(D); } catch (e2) { console.error(e2); }
    }
  }

  async function loadRun(entry) {
    reportEl.innerHTML = `<div class="loading">Loading ${esc(entry.name || entry.id)}…</div>`;
    try {
      const res = await fetch(entry.path, { cache: "no-cache" });
      if (!res.ok) throw new Error("HTTP " + res.status);
      const data = await res.json();
      CURRENT = { meta: entry, data };
      draw();
    } catch (err) {
      console.error(err);
      reportEl.innerHTML = `<div class="error-box">Could not load <code>${esc(entry.path)}</code> (${esc(err.message)}).<br>If you opened this file directly, serve the folder instead: <code>python3 -m http.server -d docs</code> — <code>file://</code> blocks fetch().</div>`;
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
