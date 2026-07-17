/* Stellar RPC full-history — stakeholder benchmark SUMMARY.
   A standalone, public-facing cut of the same committed run JSONs the internal
   viewer (index.html / app.js) renders. Written for external technical readers:
   latency percentiles, throughput, roadmap tracking, methodology, and where the
   measured slice sits in the end-to-end path — nothing about the storage engine.

   Vanilla JS, single IIFE, no framework, no CDN, no build. Chart + formatting
   helpers are duplicated from app.js on purpose (the two files are intentionally
   not refactored into shared modules). Fetches runs/index.json + run files over
   HTTP (needs `make serve`; file:// will not work). Deep-links via ?run=<id>. */
(function () {
  "use strict";

  const reportEl = document.getElementById("report");
  const selectEl = document.getElementById("run-select");
  const themeBtn = document.getElementById("theme-toggle");
  const fullLink = document.getElementById("full-report");
  const tip = document.getElementById("tip");

  let MANIFEST = null;
  let CURRENT = null;   // { meta, data } of the run on screen
  let redrawTimer = null;

  /* ==================================================================== */
  /* ROADMAP CONSTANTS — the scaling-roadmap targets as of 2026-07-17.     */
  /* This is the page's ONLY hardcoded data; everything else comes from the */
  /* run JSON. TPL = transactions per ledger = TPS × block_time_s. Phase 2  */
  /* has no defined data-availability target (rendered "—", never guessed). */
  /* Load is keyed by run unit id so the modeled-vs-phase comparison is     */
  /* computed from unit_meta at render time, not hardcoded per pack.        */
  /* ==================================================================== */
  const ROADMAP = {
    phases: ["Phase 1", "Phase 2", "Phase 3"],
    blockTimeMs: [2000, 1000, 600],           // per-ledger ingest budget
    load: {                                    // TPS / TPL per phase, by unit id
      sac:      [{ tps: 3000, tpl: 6000 }, { tps: 5000, tpl: 5000 }, { tps: 10000, tpl: 6000 }],
      token:    [{ tps: 2000, tpl: 4000 }, { tps: 4000, tpl: 4000 }, { tps: 6000,  tpl: 3600 }],
      soroswap: [{ tps: 750,  tpl: 1500 }, { tps: 1500, tpl: 1500 }, { tps: 3000,  tpl: 1800 }],
    },
    e2eBudgetMs: [5000, 2500, 2000],           // network-wide submit → client sees result
    daP99Ms:     [940, null, 140],             // RPC data-availability p99 (composite); Phase 2 undefined
    retention:   ["3 months", "6 months", "2 years"],
    txSubmitP99Ms: 60,                          // tx submission p99 target (all phases; not on the DA path)
  };

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
  const fmtInt = x => Math.round(x).toLocaleString("en-US");
  const fmtK = x => x >= 1e6 ? trim(x / 1e6) + " M" : x >= 1e3 ? trim(x / 1e3) + " K" : trim(x);
  const esc = s => String(s).replace(/[&<>"]/g, c => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c]));
  const msRound = ns => Math.round(ns / MS);                 // whole ms, matches the internal keep-up table
  const fmtBudget = ms => ms >= 1000 ? (+(ms / 1000).toFixed(2)) + " s" : ms + " ms";
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
  const CVAR = s => getComputedStyle(document.documentElement).getPropertyValue(s).trim();
  const COLORS = () => ({
    s1: CVAR("--s1"), s2: CVAR("--s2"), s3: CVAR("--s3"), s4: CVAR("--s4"),
    s5: CVAR("--s5"), s6: CVAR("--s6"), hot: CVAR("--hot"), de: CVAR("--deemph"),
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
  // rows: [{label, sub, lanes:[{name,color,pts:{p50,p90,p99,max}(ns), spread?}]}]
  // opts: { reflineNs, reflineLabel } — same primitive as the internal viewer's.
  function dotRangeChart(bodyId, rows, opts = {}) {
    const el = document.getElementById(bodyId);
    if (!el) return;
    el.replaceChildren();
    const W = Math.max(el.clientWidth, 360);
    const labW = W < 560 ? 104 : 150;
    const m = { l: labW, r: 34, t: 10, b: 46 };
    const laneH = 30;
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
    S("text", { x: W - m.r, y: H - 8, "text-anchor": "end", class: "ax-unit", text: "per-ledger latency · log scale" }, svg);
    let y = m.t;
    rows.forEach(row => {
      row.lanes.forEach((lane, li) => {
        const cy = y + laneH / 2;
        if (li === 0) {
          S("text", { x: m.l - 10, y: cy - 3, "text-anchor": "end", class: "rowlab", text: row.label }, svg);
          if (row.sub) S("text", { x: m.l - 10, y: cy + 11, "text-anchor": "end", class: "rowsub", text: row.sub }, svg);
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
          { color: lane.color, value: fmtNs(p.max), label: "max (worst ledger)" },
        ];
        if (lane.spread) tipRows.push({ value: lane.spread, label: "p99 min–max across runs" });
        hoverable(hit, `${row.label} — ${lane.name}`, tipRows);
        y += laneH;
      });
      y += rowGap;
    });
    el.appendChild(svg);
  }

  /* ============================ verdict logic ============================ */
  // Three-state, computed from unit_meta at render time:
  //  modeled TPL ≥ phase TPL AND measured p99 ≤ target → "pass"
  //  modeled TPL ≥ phase TPL AND measured p99 >  target → "fail"
  //  modeled TPL <  phase TPL                            → "notyet" (never a pass)
  //  target null (no target defined for this phase)      → "undef"
  function verdict(modeledTpl, phaseTpl, p99ns, targetNs) {
    if (targetNs == null) return "undef";
    if (modeledTpl < phaseTpl) return "notyet";
    return p99ns <= targetNs ? "pass" : "fail";
  }
  const BADGE = { pass: "PASS", fail: "FAIL", notyet: "NOT YET", undef: "— no target" };
  const meas = s => `<span class="sum-meas">${esc(s)}</span>`;
  const tgt  = s => `<span class="sum-tgt">${esc(s)}</span>`;
  function vCell(state, detailHTML) {
    return `<div class="vcell"><span class="sum-badge ${state}">${BADGE[state]}</span><span class="vdetail">${detailHTML}</span></div>`;
  }

  /* ============================ shared HTML ============================ */
  function mastheadHTML(D) {
    const mac = D.machine || {}, b = D.build || {}, camp = D.campaign || {};
    const buildBits = [];
    if (b.commit) buildBits.push(shortCommit(b.commit) + (b.branch ? ` (${b.branch})` : ""));
    const go = verNum(b.go, /go(\d[\d.]*)/); if (go) buildBits.push("go " + go);
    const machineLine = [mac.instance, mac.vcpus ? mac.vcpus + " vCPU" : "", mac.mem, "local NVMe"].filter(Boolean).join(" · ");
    return `
    <header class="masthead">
      <div class="mast-eyebrow">
        <span>Stellar RPC v2 · Full-History Storage</span>
        <span>Benchmark Summary · ${esc(D.run_date || "")}</span>
      </div>
      <h1>Stellar RPC v2 · Full-History Storage — Benchmark Summary</h1>
      <p class="mast-sub">How fast an RPC node ingests ledgers${(D.dataset || {}).kind === "synthetic" ? " replayed at target network loads" : ""}: the time to durably write one ledger's transactions and events into the full-history store and be ready for the next — <strong>one fsync per ledger</strong>. ${esc(D.run_name || "")}.</p>
      <div class="meta-grid">
        <div class="meta-cell"><div class="meta-k">Machine</div><div class="meta-v">${esc(machineLine)}</div></div>
        <div class="meta-cell"><div class="meta-k">Build</div><div class="meta-v">${esc(buildBits.join(" · ")) || "—"}</div></div>
        <div class="meta-cell"><div class="meta-k">Protocol</div><div class="meta-v">${camp.reps || 5} runs per profile · fresh process each · medians reported</div></div>
      </div>
    </header>`;
  }

  function footerHTML(D, runId) {
    const b = D.build || {}, camp = D.campaign || {}, mac = D.machine || {};
    return `<div class="footer">
      ${esc(D.run_name || "")} · ${esc(mac.instance || "")}, local NVMe · build ${esc(shortCommit(b.commit))}${b.branch ? " (" + esc(b.branch) + ")" : ""}.<br>
      Every value is the median of ${camp.reps || 5} process-level runs with min–max spread; no value was interpolated or smoothed.
      <a href="index.html?run=${esc(runId)}" style="color:var(--accent)">Full internal report ↗</a>
    </div>`;
  }

  function figHTML(id, no, title, legendId, caption) {
    return `<figure class="fig" id="${id}">
      <div class="fig-head"><div><span class="fig-no">${no}</span><span class="fig-title">${title}</span></div>${legendId ? `<div class="legend" id="${legendId}"></div>` : ""}</div>
      <div class="fig-body" id="${id}-body"></div>
      <figcaption>${caption}</figcaption>
      <details class="tv" id="${id}-tv"><summary>Table view — exact values &amp; run spread</summary><div class="tv-scroll"></div></details>
    </figure>`;
  }

  function methodologyHTML(D, num) {
    const ds = D.dataset || {}, mac = D.machine || {}, camp = D.campaign || {};
    const kind = ds.kind;
    const testData = kind === "synthetic"
      ? `Synthetic ledgers generated with stellar-core's <code>apply-load</code> — real transactions applied through the real execution path, then replayed at the target loads. Three profiles span the workload space: simple high-volume SAC transfers, a heavier OpenZeppelin-style contract token, and event-heavy AMM swaps. Per-profile facts (model, TPS, tx/ledger, ledgers, txs, events, pack size) come from the run's dataset metadata and appear on the cards above.`
      : `Real Stellar pubnet ledgers, sourced once as immutable history packs and re-ingested locally for every timed run — no timed number includes network transfer.`;
    // fsync probe: dd oflag=dsync over 2,000 synced 4 KiB writes (8,192,000 bytes).
    const raw = mac.fsync_probe || "";
    let fsyncLine = "";
    if (raw) {
      let secs = null;
      if (/^8192000 bytes/.test(raw)) {
        const m = raw.match(/copied,\s*([\d.]+)\s*s\b/) || raw.match(/in\s+([\d.]+)\s*secs?\b/);
        const v = m ? parseFloat(m[1]) : NaN;
        if (isFinite(v) && v > 0) secs = v;
      }
      const perWrite = secs != null ? ` ≈ ${trim(secs / 2000 * 1e6)} µs per synced write` : "";
      fsyncLine = ` The device really pays for fsync: a probe of 2,000 individually-synced 4&nbsp;KiB writes (<code>dd … oflag=dsync</code>) measured <code>${esc(raw)}</code>${perWrite}.`;
    }
    const machineLine = [mac.instance, mac.vcpus ? mac.vcpus + " vCPU" : "", mac.mem, "local NVMe"].filter(Boolean).join(", ");
    return `<section id="methodology" class="method">
      <div class="sec-head"><span class="sec-num">${num}</span><h2>How this was measured</h2></div>
      <p class="sec-intro">Enough for a skeptical engineer to trust — or challenge — the numbers.</p>
      <dl>
        <dt>Test data</dt><dd>${testData}</dd>
        <dt>Protocol</dt><dd>${camp.reps || 5} repetitions per profile, each a fresh process; reported values are medians across runs with min–max spread. Each run ingests the profile's full ledger range from an empty store through the daemon's bounded ingestion loop, committing one fsync per ledger. The per-ledger time excludes waiting for the next ledger; run wall (used for sustained throughput) includes it. Nothing is averaged across runs or interpolated.</dd>
        <dt>Hardware</dt><dd>A modest single node — ${esc(machineLine)} — so the results are conservative rather than best-case.${fsyncLine} Full machine metadata below.</dd>
        <dt>Limitations</dt><dd>Single node; no concurrent read traffic during ingestion (that is a separate benchmark); synthetic uniform workloads rather than mainnet's mixed traffic; and these numbers cover the ingestion slice only — not the full data-availability composite.</dd>
      </dl>
      <details class="tv"><summary>Full machine metadata</summary><div class="tv-scroll"><pre class="metadata">${esc((mac.raw || "").trim())}</pre></div></details>
    </section>`;
  }

  function pipelineSectionHTML(num) {
    const steps = [
      { t: "Transaction submitted", tag: "elsewhere", tl: "tx submission" },
      { t: "Network consensus · ledger externalized", tag: null },
      { t: "Captive core emits ledger meta", tag: null },
      { t: "RPC reads the meta stream", tag: "elsewhere", tl: "separate benchmark" },
      { t: "RPC ingests into the full-history store", tag: "meas", tl: "measured in this report", measured: true },
      { t: "Data queryable", tag: null },
      { t: "Client reads the result", tag: "elsewhere", tl: "query latency · reads under ingestion load" },
    ];
    const cells = steps.map((s, i) => `
      <div class="sum-step${s.measured ? " measured" : ""}">
        <div class="step-n">${i + 1}</div>
        <div class="step-t">${esc(s.t)}</div>
        ${s.tag ? `<span class="step-tag ${s.tag}">${esc(s.tl)}</span>` : ""}
      </div>`).join("");
    return `<section id="pipeline">
      <div class="sec-head"><span class="sec-num">${num}</span><h2>Where this sits end-to-end</h2></div>
      <p class="sec-intro">These benchmarks measure one hop of the journey from a submitted transaction to a client seeing the result: durably ingesting one ledger's data into the full-history store (next-ledger wait excluded). The other hops are measured by separate benchmarks, reported elsewhere or planned.</p>
      <div class="sum-pipe">${cells}</div>
      <p class="pipe-note">Reading the meta stream from captive core, query latency, reads under concurrent ingestion, and transaction submission are each their own benchmark. Transaction submission carries its own p99 ${ROADMAP.txSubmitP99Ms} ms target at every phase and is <strong>not</strong> part of the data-availability path.</p>
    </section>`;
  }

  /* ============================ SUMMARY renderer ============================ */
  function renderSummary(D) {
    const ds = D.dataset || {};
    const um = ds.unit_meta || {};
    const ORDER = ds.unit_order || Object.keys(um);
    const hot = D.ingest_hot || {};
    const unitLabel = ds.unit_label || "Profile";
    const hasKeepup = D.checks && D.checks.kind === "block_keepup";
    const interval = hasKeepup ? D.checks.interval_ns : null;
    const intervalMs = interval ? msRound(interval) : null;
    const runId = D.run_id || (CURRENT && CURRENT.meta && CURRENT.meta.id) || "";

    // Display name: explicit label, else the modeled tx model, else — for a
    // purely numeric id like a pubnet chunk — the unit label prefixed so a bare
    // number never stands alone (matching the internal viewer), else the id.
    const name = u => {
      const m = um[u] || {};
      if (m.label) return m.label;
      if (m.model) return m.model;
      if (ds.unit_label && /^\d+$/.test(u)) return `${ds.unit_label} ${u}`;
      return u;
    };

    // Units with per-ledger latency (headline + chart).
    const latUnits = ORDER.filter(u => hot[u] && hot[u].driver && hot[u].driver.ingest_total);
    // Units that also carry modeled load + a keep-up interval + a roadmap load def
    // (roadmap tracking). Absent for the pubnet run → the whole section is hidden.
    const roadmapUnits = latUnits.filter(u => hasKeepup && um[u] && um[u].tx_per_ledger != null && ROADMAP.load[u]);
    const showRoadmap = roadmapUnits.length > 0;

    // per-unit derived keep-up math — identical formulas to the internal viewer.
    const sustainedNs = u => hot[u].driver.run_wall && um[u] && um[u].ledgers
      ? hot[u].driver.run_wall.total.m / um[u].ledgers : null;

    // section numbering (pubnet skips roadmap, so numbers stay contiguous)
    let i = 1; const N = () => String(i++).padStart(2, "0");
    const nHead = N();
    const nRoad = showRoadmap ? N() : null;
    const nPipe = N();
    const nMeth = N();

    /* ---- headline stat cards (HTML) ---- */
    const cardHTML = u => {
      const it = hot[u].driver.ingest_total, m = um[u] || {};
      const susNs = interval ? sustainedNs(u) : null;
      const eyebrow = m.tps != null
        ? `${fmtInt(m.tps)} TPS · ${fmtInt(m.tx_per_ledger)} TPL modeled`
        : `${fmtInt(m.ledgers)} ledgers`;
      const susBlock = susNs != null
        ? `<div class="sus"><span>sustained <b>${msRound(susNs)} ms/ledger</b> · ${(NS / susNs).toFixed(1)}/s · <b>${(interval / susNs).toFixed(1)}×</b> headroom</span>${susNs <= interval ? `<span class="keepup">✓ keeps up</span>` : ""}</div>`
        : `<div class="sus"><span>${fmtInt(m.ledgers)} ledgers · ${fmtK(m.txs)} txs · ${fmtK(m.events)} events</span></div>`;
      return `<div class="sum-card">
        <div class="card-model">${esc(eyebrow)}</div>
        <div class="card-title">${esc(name(u))}</div>
        <div class="big">${msRound(it.p99.m)}<small> ms</small></div>
        <div class="big-lab">p99 per-ledger ingest latency</div>
        <div class="pcts">
          <div class="pct"><span class="pn">${msRound(it.p50.m)} ms</span><span class="pl">p50</span></div>
          <div class="pct"><span class="pn">${msRound(it.p90.m)} ms</span><span class="pl">p90</span></div>
          <div class="pct"><span class="pn">${msRound(it.max.m)} ms</span><span class="pl">max</span></div>
        </div>
        ${susBlock}
      </div>`;
    };

    /* ---- workload lines (HTML) ---- */
    const workloadLI = u => {
      const m = um[u] || {};
      const bits = [];
      if (m.tps != null) bits.push(`${fmtInt(m.tps)} TPS modeled at ${fmtInt(m.tx_per_ledger)} tx/ledger`);
      bits.push(`${fmtInt(m.ledgers)} ledgers`);
      bits.push(`${fmtK(m.txs)} txs`);
      const evtx = m.txs ? ` (~${(m.events / m.txs).toFixed(1)} events/tx)` : "";
      bits.push(`${fmtK(m.events)} events${evtx}`);
      const modelPrefix = m.model && m.model !== name(u) ? `${esc(m.model)}: ` : "";
      return `<li><b>${esc(name(u))}</b> — ${modelPrefix}${esc(bits.join(" · "))}.</li>`;
    };

    const headlineHTML = latUnits.length ? `
    <section id="headline">
      <div class="sec-head"><span class="sec-num">${nHead}</span><h2>Per-ledger ingestion latency</h2></div>
      <p class="sec-intro">Time to durably ingest one ledger, end to end, for each workload profile — median of ${D.campaign ? D.campaign.reps : 5} runs, next-ledger wait excluded.${interval ? ` Judged against the modeled ${intervalMs} ms block interval.` : ""}</p>
      <div class="sum-cards" id="sum-cards">${latUnits.map(cardHTML).join("")}</div>
      ${figHTML("fig-latency", "Fig 1", "Per-ledger ingest latency by profile — p50 / p90 / p99 / max", "fig-latency-legend", `Latency percentiles over every ledger of the run (median of 5; log scale).${interval ? " The dashed line is one block interval." : ""} The measured span is ingestion only — see §${nPipe}.`)}
      <ul class="workload-lines">${latUnits.map(workloadLI).join("")}</ul>
    </section>` : `
    <section id="headline">
      <div class="sec-head"><span class="sec-num">${nHead}</span><h2>Per-ledger ingestion latency</h2></div>
      <div class="note-callout">This run has no per-ledger ingestion latency data to summarize.</div>
    </section>`;

    /* ---- roadmap section (HTML) ---- */
    let roadmapHTML = "";
    if (showRoadmap) {
      // (a) constants table
      const ph = ROADMAP.phases;
      const cRow = (label, vals) => `<tr><td>${label}</td>${vals.map(v => `<td>${v}</td>`).join("")}</tr>`;
      const loadCells = u => ROADMAP.load[u].map(x => `${fmtInt(x.tps)} TPS / ${fmtInt(x.tpl)} TPL`);
      const constRows = [
        cRow("Block time <span class=\"rowhead-sub\">= per-ledger ingest budget</span>", ROADMAP.blockTimeMs.map(fmtBudget)),
        ...roadmapUnits.map(u => cRow(esc(name(u) + " load"), loadCells(u))),
        cRow("Network-wide E2E budget <span class=\"rowhead-sub\">submit → client sees result</span>", ROADMAP.e2eBudgetMs.map(fmtBudget)),
        cRow("RPC data-availability p99 <span class=\"rowhead-sub\">meta available → client has it (composite)</span>", ROADMAP.daP99Ms.map(v => v == null ? "—" : fmtBudget(v))),
        cRow("History retention", ROADMAP.retention.map(esc)),
      ].join("");
      const constTable = `<div class="grid-wrap"><table class="roadmap constants">
        <thead><tr><th>Target</th>${ph.map(p => `<th>${p}</th>`).join("")}</tr></thead>
        <tbody>${constRows}</tbody></table></div>
        <p class="legend-note">All values above are <span class="sum-tgt">roadmap targets</span> (blue, dotted). Measured values below are shown in ${meas("bold")}.</p>`;

      // (b) block-time verdict grid
      const btRow = u => {
        const p99 = hot[u].driver.ingest_total.p99.m, tplModel = um[u].tx_per_ledger;
        const cells = ph.map((_, k) => {
          const phaseTpl = ROADMAP.load[u][k].tpl, budgetNs = ROADMAP.blockTimeMs[k] * MS;
          const st = verdict(tplModel, phaseTpl, p99, budgetNs);
          let detail;
          if (st === "notyet") detail = `load ${meas(fmtInt(tplModel) + " TPL")} &lt; ${tgt(fmtInt(phaseTpl) + " TPL")}`;
          else if (st === "fail") detail = `p99 over ${tgt(fmtBudget(ROADMAP.blockTimeMs[k]))} budget`;
          else detail = `≥ ${tgt(fmtInt(phaseTpl) + " TPL")} load · under ${tgt(fmtBudget(ROADMAP.blockTimeMs[k]))}`;
          return `<td>${vCell(st, detail)}</td>`;
        });
        return `<tr><td>${esc(name(u))}</td><td>${meas(msRound(p99) + " ms")}</td><td>${meas(fmtInt(tplModel) + " TPL")}</td>${cells.join("")}</tr>`;
      };
      const btGrid = `<h3 style="font-size:15px;margin:26px 0 0">Block-time keep-up — measured p99 vs each phase's per-ledger budget</h3>
        <div class="grid-wrap"><table class="roadmap" id="roadmap-grid">
        <thead><tr><th>Profile</th><th>Measured p99</th><th>Modeled load</th>${ph.map((p, k) => `<th>${p}<span class="rowhead-sub">≤ ${fmtBudget(ROADMAP.blockTimeMs[k])} budget</span></th>`).join("")}</tr></thead>
        <tbody>${roadmapUnits.map(btRow).join("")}</tbody></table></div>`;

      // (c) data-availability comparison (with composite caveat)
      const daRow = u => {
        const p99 = hot[u].driver.ingest_total.p99.m, tplModel = um[u].tx_per_ledger;
        const cells = ph.map((_, k) => {
          const phaseTpl = ROADMAP.load[u][k].tpl, target = ROADMAP.daP99Ms[k];
          const targetNs = target == null ? null : target * MS;
          const st = verdict(tplModel, phaseTpl, p99, targetNs);
          let detail;
          if (st === "undef") detail = `no ${tgt("Phase 2")} target defined`;
          else if (st === "notyet") detail = `load ${meas(fmtInt(tplModel) + " TPL")} &lt; ${tgt(fmtInt(phaseTpl) + " TPL")}`;
          else if (st === "fail") detail = `ingest slice alone over ${tgt(fmtBudget(target))}`;
          else detail = `slice under ${tgt(fmtBudget(target))} · composite ≥ this`;
          return `<td>${vCell(st, detail)}</td>`;
        });
        return `<tr><td>${esc(name(u))}</td><td>${meas(msRound(p99) + " ms")}</td>${cells.join("")}</tr>`;
      };
      const soroP99 = roadmapUnits.includes("soroswap") ? hot.soroswap.driver.ingest_total.p99.m : null;
      const daP3Ms = ROADMAP.daP99Ms[2], soroP3Tpl = ROADMAP.load.soroswap[2].tpl;
      const soroNotYet = soroP99 != null &&
        verdict(um.soroswap.tx_per_ledger, soroP3Tpl, soroP99, daP3Ms * MS) === "notyet";
      const soroUnder = soroNotYet && soroP99 <= daP3Ms * MS;
      const daFootnote = soroNotYet
        ? `<p class="footnote">The ${esc(name("soroswap"))} pack is modeled at ${meas(fmtInt(um.soroswap.tx_per_ledger) + " TPL")}, below the Phase 3 target of ${tgt(fmtInt(soroP3Tpl) + " TPL")}, so it is <b>not yet measured</b> at Phase 3 load. At the load tested its ingest-slice p99 (${meas(msRound(soroP99) + " ms")}) is ${soroUnder ? "under" : "already over"} the ${tgt(daP3Ms + " ms")} target${soroUnder ? ", but that is not the same as passing at Phase 3 load" : ", even at this lighter load"}.</p>`
        : "";
      const daGrid = `<h3 style="font-size:15px;margin:30px 0 0">Data-availability p99 — measured ingest slice vs the composite target</h3>
        <div class="note-callout caveat">The benchmark measures the <strong>ingestion slice</strong> of the data-availability path. The full metric — meta available in captive core → client has the information — also includes reading the meta stream from captive core and the client's query, so the <strong>composite p99 will be ≥ the number shown</strong>. A pass here means the ingest component fits inside the budget, not that the composite does.</div>
        <div class="grid-wrap"><table class="roadmap" id="da-grid">
        <thead><tr><th>Profile</th><th>Ingest-slice p99</th>${ph.map((p, k) => `<th>${p}<span class="rowhead-sub">${ROADMAP.daP99Ms[k] == null ? "no target" : "≤ " + fmtBudget(ROADMAP.daP99Ms[k]) + " (composite)"}</span></th>`).join("")}</tr></thead>
        <tbody>${roadmapUnits.map(daRow).join("")}</tbody></table></div>${daFootnote}`;

      roadmapHTML = `<section id="roadmap">
        <div class="sec-head"><span class="sec-num">${nRoad}</span><h2>Tracking against the scaling roadmap</h2></div>
        <p class="sec-intro">The RPC v2 scaling roadmap defines three phases of increasing network load. The targets are fixed constants; the verdicts below are computed from each profile's modeled load and measured p99 at render time. A profile only earns a verdict at a phase if it was actually modeled at (or above) that phase's load.</p>
        ${constTable}
        ${btGrid}
        ${daGrid}
      </section>`;
    }

    reportEl.innerHTML =
      mastheadHTML(D) +
      headlineHTML +
      roadmapHTML +
      pipelineSectionHTML(nPipe) +
      methodologyHTML(D, nMeth) +
      footerHTML(D, runId);

    /* ---- populate the latency chart (SVG) ---- */
    if (latUnits.length) {
      const C = COLORS();
      const rows = latUnits.map(u => {
        const it = hot[u].driver.ingest_total;
        const susNs = interval ? sustainedNs(u) : null;
        return {
          label: name(u),
          sub: susNs != null ? `${msRound(susNs)} ms/ledger` : `${fmtInt((um[u] || {}).ledgers || 0)} ledgers`,
          lanes: [{
            name: "per-ledger ingest", color: C.s1,
            pts: { p50: it.p50.m, p90: it.p90.m, p99: it.p99.m, max: it.max.m },
            spread: `${fmtNs(it.p99.lo)} – ${fmtNs(it.p99.hi)}`,
          }],
        };
      });
      const opts = interval ? { reflineNs: interval, reflineLabel: intervalMs + " ms — block interval" } : {};
      dotRangeChart("fig-latency-body", rows, opts);
      legend("fig-latency-legend", [
        { label: "per-ledger ingest", color: C.s1 },
        { label: "● p50 · • p90 · ○ p99 · | max", color: "transparent" },
        ...(interval ? [{ label: intervalMs + " ms block interval", color: CVAR("--hot"), line: true }] : []),
      ]);
      tableView("fig-latency", [unitLabel, "p50", "p90", "p99", "max", "p99 min–max across runs"],
        latUnits.map(u => {
          const it = hot[u].driver.ingest_total;
          return [name(u), fmtNs(it.p50.m), fmtNs(it.p90.m), fmtNs(it.p99.m), fmtNs(it.max.m), `${fmtNs(it.p99.lo)} – ${fmtNs(it.p99.hi)}`];
        }));
    }
  }

  /* ============================ shell / boot ============================ */
  function draw() {
    if (!CURRENT || !CURRENT.data) return;
    const D = CURRENT.data;
    try { renderSummary(D); }
    catch (err) {
      console.error("summary render failed", err);
      reportEl.innerHTML = `<div class="error-box">Failed to render summary for run “${esc(D.run_id || "")}”: ${esc(err.message)}.</div>`;
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
    const entry = MANIFEST.runs.find(r => r.id === id) || defaultRun();
    if (!entry) return;
    selectEl.value = entry.id;
    const url = new URL(location.href);
    url.searchParams.set("run", entry.id);
    if (push) history.pushState({ run: entry.id }, "", url); else history.replaceState({ run: entry.id }, "", url);
    loadRun(entry);
  }

  // default: newest synthetic run (manifest is newest-first), else newest run.
  function defaultRun() {
    return (MANIFEST.runs || []).find(r => r.kind === "synthetic") || (MANIFEST.runs || [])[0];
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
    const def = defaultRun();
    selectRun(initial || (def && def.id), false);
  }

  window.addEventListener("resize", () => { clearTimeout(redrawTimer); redrawTimer = setTimeout(draw, 160); });
  boot();
})();
