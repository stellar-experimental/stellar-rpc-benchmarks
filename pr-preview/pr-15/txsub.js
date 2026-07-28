/* Interim transaction-submission results page (stellar/stellar-rpc#869).
   Fetches docs/txsub/index.json, the harvest summaries it lists, and
   targets.json over HTTP, and renders them into #report. Every number on the
   page comes from those files. Deliberately separate from the run-JSON viewer
   (app.js): these summaries are a different shape (one run, exact per-request
   percentiles) and get a unified home when the multi-family refactor lands. */
(function () {
  "use strict";

  const report = document.getElementById("report");

  /* ---- theme toggle (mirrors the viewer: stamps data-theme, no persistence).
     Charts here are plain CSS on the shared tokens, so no re-render needed. ---- */
  document.getElementById("theme-toggle").addEventListener("click", () => {
    const cur = document.documentElement.getAttribute("data-theme")
      || (matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light");
    document.documentElement.setAttribute("data-theme", cur === "dark" ? "light" : "dark");
  });

  /* ---- formatting (all durations are integer nanoseconds) ---- */
  function fmt(ns) {
    if (ns == null || !isFinite(ns)) return "—";
    const ms = ns / 1e6;
    if (ms >= 1000) return +(ms / 1000).toFixed(2) + " s";
    if (ms >= 10) return +ms.toFixed(1) + " ms";
    return +ms.toFixed(2) + " ms";
  }
  const fmtInt = (n) => (n == null ? "—" : Number(n).toLocaleString("en-US"));
  const pct = (x) => Math.round(x * 100) + " %";
  function esc(s) { return String(s).replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c])); }

  const MODE_LABEL = { "sac-transfer": "SAC transfer", "oz-transfer": "OZ token transfer", "soroswap": "Soroswap swap" };
  const label = (mode) => MODE_LABEL[mode] || mode;

  const statusText = (byStatus) =>
    Object.entries(byStatus || {}).map(([s, c]) => `${fmtInt(c)} ${esc(s)}`).join(" · ") || "—";

  function figure(no, title, legend, body, caption) {
    return `<figure class="fig">
      <div class="fig-head"><div><span class="fig-no">${no}</span><span class="fig-title">${title}</span></div>${legend || ""}</div>
      <div class="fig-body">${body}</div>
      ${caption ? `<figcaption>${caption}</figcaption>` : ""}
    </figure>`;
  }

  const secHead = (no, title) =>
    `<div class="sec-head"><span class="sec-num">${no}</span><h2>${title}</h2></div>`;

  /* ---- shared handler percentile table (headline + confirmation) ---- */
  function handlerTable(id, profiles) {
    const rows = profiles.map(({ mode, data }) => {
      const h = data.handler;
      return `<tr>
        <td>${esc(label(mode))}</td>
        <td>${fmtInt(h.count)}</td>
        <td>${statusText(h.by_status)}</td>
        <td>${fmt(h.p50_ns)}</td>
        <td>${fmt(h.p90_ns)}</td>
        <td class="ts-p99">${fmt(h.p99_ns)}</td>
        <td>${fmt(h.max_ns)}</td>
      </tr>`;
    }).join("");
    return `<div class="tv-scroll"><table class="data" id="${id}">
      <thead><tr><th>Profile</th><th>n</th><th>status</th><th>p50</th><th>p90</th><th class="ts-p99h">p99</th><th>max</th></tr></thead>
      <tbody>${rows}</tbody></table></div>`;
  }

  /* ---- 01 · the submission budget ---- */
  function budgetSection(no, manifest, targets, headline) {
    let worst = null;
    for (const p of headline._profiles) {
      const v = p.data.handler && p.data.handler.p99_ns;
      if (typeof v === "number" && (!worst || v > worst.ns)) worst = { ns: v, mode: p.mode };
    }
    const rtt = manifest.assumed_network_rtt_ns;
    const model = targets && targets.fixed_estimates && targets.fixed_estimates.tx_submit_p99_ns;

    const tile = (v, k, sub) =>
      `<div class="tile"><div class="tile-value">${v}</div><div class="tile-label"><strong>${k}.</strong> ${sub}</div></div>`;
    let tiles = "";
    if (model != null) {
      tiles += tile(fmt(model), "Target value",
        `What the E2E latency model currently allocates to the submission slice.`);
    }
    if (worst && rtt != null) {
      const basis = worst.ns + rtt;
      const chip = model == null ? ""
        : basis <= model
          ? ` <span class="chip">▪ ${fmt(model - basis)} under the model</span>`
          : ` <span class="chip warn">▲ ${fmt(basis - model)} over the model</span>`;
      tiles += tile(fmt(basis), "Calculated value" + chip,
        `worst-profile handler p99 (${esc(label(worst.mode))}, ${fmt(worst.ns)}, standalone) + the assumed ${fmt(rtt)} network round trip.`);
    }

    return `<section id="budget">
      ${secHead(no, "The tx submission budget")}
      <p class="sec-intro">The tx submission slice of the end-to-end latency model decomposes as
        <strong>client↔RPC network time</strong> — assumed at ~${fmt(rtt)} round trip, not measured here —
        plus the <strong>in-RPC time</strong> this benchmark measures. </p>
      <div class="tiles">${tiles}</div>
    </section>`;
  }

  /* ---- 02 · headline results (standalone) ---- */
  function p99Bars(profiles) {
    const max = Math.max(...profiles.map((p) => p.data.handler.p99_ns));
    const rows = profiles.map(({ mode, data }) => {
      const h = data.handler;
      const w = max > 0 ? ((h.p99_ns / max) * 100).toFixed(1) : 0;
      return `<div class="cbar-lab">${esc(label(mode))}<small>n = ${fmtInt(h.count)}</small></div>
        <div class="cbar-track"><div class="cbar-fill" style="width:${w}%"></div></div>
        <div class="cbar-val">${fmt(h.p99_ns)}</div>`;
    }).join("");
    return `<div class="cbar">${rows}</div>`;
  }

  function headlineSection(no, run) {
    return `<section id="headline">
      ${secHead(no, "Main results — standalone network")}
      <p class="sec-intro">Tx submission duration per profile, measured at RPC's <code>sendTransaction</code> handler: Load: ${fmtInt(run.target_rps)} rps ×
        ${fmtInt(run.duration_s)} s per profile against RPC ${esc(run.rpc_version)} on ${esc(run.instance)}.</p>
      ${figure("Fig 2.1", "Handler p99 per profile", "", p99Bars(run._profiles),
        "The tail (p99) is the number that matters for the submission budget; the full distribution is in the table below.")}
      ${handlerTable("headline-table", run._profiles)}
    </section>`;
  }

  /* ---- 03 · the split (core leg vs RPC residue, means only) ---- */
  function splitBars(profiles) {
    const maxMean = Math.max(...profiles.map((p) => p.data.mean_residue.handler_mean_ns));
    return `<div class="split-rows">` + profiles.map(({ mode, data }) => {
      const r = data.mean_residue;
      const seg = (v, colorVar, name) => {
        const w = (v / maxMean) * 100;
        const share = pct(v / r.handler_mean_ns);
        const lab = w >= 9 ? `<span class="ts-seg-lab">${fmt(v)} · ${share}</span>` : "";
        return `<div class="ts-seg" style="width:${w.toFixed(1)}%;background:var(${colorVar})"
                     title="${name} mean ${fmt(v)} (${share} of the handler mean)">${lab}</div>`;
      };
      return `<div class="split-row">
        <div class="split-lab">${esc(label(mode))}<span class="mono-sub">handler mean ${fmt(r.handler_mean_ns)}</span></div>
        <div class="ts-splitbar">
          ${seg(r.core_leg_mean_ns, "--s1", "core leg")}
          ${seg(r.residue_mean_ns, "--s5", "RPC residue")}
          <div class="ts-rest"></div>
        </div>
      </div>`;
    }).join("") + `</div>`;
  }

  function coreLegTable(profiles) {
    const rows = profiles.map(({ mode, data }) =>
      Object.entries(data.core_leg.by_status || {}).map(([status, q]) => `<tr>
        <td>${esc(label(mode))}</td>
        <td>${esc(status)}</td>
        <td>${fmtInt(q.count)}</td>
        <td>${fmt(q.p50_ns)}</td>
        <td>${fmt(q.p90_ns)}</td>
        <td class="ts-p99">${fmt(q.p99_ns)}</td>
        <td>${fmt(q.mean_ns)}</td>
      </tr>`).join("")).join("");
    return `<div class="tv-scroll"><table class="data" id="coreleg-table">
      <thead><tr><th>Profile</th><th>core status</th><th>count</th><th>p50</th><th>p90</th><th class="ts-p99h">p99</th><th>mean</th></tr></thead>
      <tbody>${rows}</tbody></table></div>`;
  }

  function splitSection(no, run) {
    const profiles = run._profiles.filter((p) => p.data.mean_residue && p.data.core_leg && p.data.core_leg.present);
    const legend = `<div class="legend">
      <span class="lg-item"><span class="lg-sw" style="background:var(--s1)"></span>Core leg — RPC→Core <code>POST /tx</code></span>
      <span class="lg-item"><span class="lg-sw" style="background:var(--s5)"></span>RPC residue — XDR decode, hash, JSONify</span>
    </div>`;
    return `<section id="split">
      ${secHead(no, "Where the time goes")}
      <p class="sec-intro">The handler <strong>mean</strong> splits additively into the RPC→Core
        <code>POST /tx</code> leg and RPC's own processing (XDR decode, hash, JSONify).</p>
      ${figure("Fig 3.1", "Handler mean: core leg vs RPC residue", legend, splitBars(profiles),
        "Bars share one scale (the largest handler mean). Segment labels are the mean and its share of that profile's handler mean.")}
      ${coreLegTable(profiles)}
      <p class="phase-caption">Core-leg quantiles (from the Prometheus Summary, per Core status) are shown side
        by side with the handler's — quantiles are never subtracted anywhere on this page.</p>
    </section>`;
  }

  /* ---- 04 · testnet confirmation ---- */
  function confirmationSection(no, run, headline) {
    let agreeTxt = "";
    if (headline) {
      const deltas = run._profiles.map((tp) => {
        const hp = headline._profiles.find((p) => p.mode === tp.mode);
        return hp ? Math.abs(tp.data.handler.p50_ns - hp.data.handler.p50_ns) / hp.data.handler.p50_ns : null;
      }).filter((d) => d != null);
      if (deltas.length) agreeTxt = `Handler medians agree with the standalone run within ~${+(Math.max(...deltas) * 100).toFixed(1)}&nbsp;%; `;
    }
    const ns = run._profiles.map((p) => p.data.handler.count);
    const nTxt = ns.length ? (Math.min(...ns) === Math.max(...ns) ? fmtInt(ns[0]) : `${fmtInt(Math.min(...ns))}–${fmtInt(Math.max(...ns))}`) : "—";
    return `<section id="confirmation">
      ${secHead(no, "Testnet confirmation")}
      <p class="sec-intro">${agreeTxt}the much smaller sample (n = ${nTxt} per profile at
        ${fmtInt(run.target_rps)} rps × ${fmtInt(run.duration_s)} s) makes tail quantiles noisy, so the
        standalone run stays the main result.</p>
      ${handlerTable("confirm-table", run._profiles)}
    </section>`;
  }

  /* ---- 05 · method & provenance ---- */
  function methodSection(no, runs) {
    const gcsLink = (gs) => `<a href="https://console.cloud.google.com/storage/browser/${esc(String(gs).replace("gs://", ""))}">${esc(gs)} ↗</a>`;
    const crossCheck = (() => {
      const checked = [];
      for (const r of runs) {
        for (const p of r._profiles || []) {
          if (p.data.json_rpc_cross_check && p.data.json_rpc_cross_check.present) checked.push(p);
        }
      }
      if (!checked.length) return "";
      const off = checked.filter((p) => p.data.json_rpc_cross_check.mean_ns !== p.data.handler.mean_ns);
      if (!off.length) return " Its mean matches the log-derived handler mean exactly for every profile.";
      return ` Its mean differs from the log-derived handler mean for ${off.map((p) =>
        `${esc(label(p.mode))} (${fmt(p.data.handler.mean_ns)} log vs ${fmt(p.data.json_rpc_cross_check.mean_ns)} metric)`).join(", ")}.`;
    })();
    const cfgRows = runs.map((r) => `<tr>
      <td>${esc(r.network)}</td>
      <td>${esc(r.role)}</td>
      <td>${esc(r.date)}</td>
      <td>${fmtInt(r.target_rps)} rps × ${fmtInt(r.duration_s)} s</td>
      <td>${esc(r.rpc_version)}</td>
      <td>${esc(r.instance)}</td>
    </tr>`).join("");
    const bundles = runs.filter((r) => r.gcs).map((r) =>
      `<div class="meta-cell"><div class="meta-k">${esc(r.network)} bundle</div><div class="meta-v">${gcsLink(r.gcs)}</div></div>`).join("");
    return `<section id="method" class="method">
      ${secHead(no, "Methodology & Data")}
      ${bundles ? `<div class="meta-grid" style="margin-top:18px">${bundles}</div>` : ""}
      <p class="sec-intro">Each bundle holds:</p>
      <ul class="bundle-files">
        <li><code>rpc-log-&lt;profile&gt;.log</code> — the RPC process log; the per-request lines the
          durations come from.</li>
        <li><code>metrics-&lt;profile&gt;.prom</code> — a snapshot of RPC's <code>/metrics</code> taken right
          after the run; holds the two Prometheus Summaries below.</li>
        <li><code>client-&lt;profile&gt;.ndjson</code> — the load generator's per-request records.</li>
        <li><code>meta.json</code> — the run config.</li>
        <li><code>summary-&lt;profile&gt;.json</code> — the aggregate <code>harvest.py</code>
          (stellar-rpc-blaster <code>scripts/tx-submission/</code>) produces from the above. These six files
          are committed verbatim under <code>docs/txsub/</code> and are what this page renders.</li>
      </ul>
      <dl>
        <dt>Handler duration (the main measurement)</dt>
        <dd>Every <code>finished JSONRPC request</code> line in RPC's log, joined to its
          <code>starting JSONRPC request</code> line on the <code>req</code> id. Every request's duration
          is kept; a percentile is the actual duration at that rank.</dd>
        <dt>Core leg</dt>
        <dd>The <code>soroban_rpc_txsub_submission_duration_seconds</code> Prometheus Summary — the RPC→Core
          <code>POST /tx</code> leg — scraped right after each run: per-status p50/p90/p99 plus an exact mean
          from <code>sum</code>/<code>count</code>.</dd>
        <dt>Cross-check</dt>
        <dd>The <code>soroban_rpc_json_rpc_request_duration_seconds{endpoint=&quot;sendTransaction&quot;}</code>
          Summary measures the same handler duration from the Prometheus side. It is a consistency check
          only, never a second data series.${crossCheck}</dd>
      </dl>
      <div class="tv-scroll" style="margin-top:18px"><table class="data" id="config-table">
        <thead><tr><th>Network</th><th>role</th><th>date</th><th>load</th><th>rpc version</th><th>instance</th></tr></thead>
        <tbody>${cfgRows}</tbody></table></div>
    </section>`;
  }

  /* ---- shell ---- */
  function masthead(manifest) {
    const dates = [...new Set((manifest.runs || []).map((r) => r.date).filter(Boolean))];
    return `<header class="masthead">
      <div class="mast-eyebrow"><span>RPC · Transaction Submission</span><span>${esc(dates.join(" · "))}</span></div>
      <h1>RPC: Benchmarking Transaction Submission</h1>
      <p class="mast-sub">A benchmark that measures the time the <code>stellar-rpc</code> process spends
        inside a single <code>sendTransaction</code> request. We measure three transaction profiles on two networks (standalone and testnet). 
        Context:
        <a href="https://github.com/stellar/stellar-rpc/issues/869">stellar/stellar-rpc#869</a> · back to the
        <a href="index.html">benchmark reports</a>.</p>
    </header>`;
  }

  function render(manifest, targets, failures) {
    const runs = manifest.runs || [];
    const headline = runs.find((r) => r.role === "headline");
    const confirmation = runs.find((r) => r.role === "confirmation");

    let html = masthead(manifest);
    if (failures.length) {
      html += `<div class="error-box">Could not load
        ${failures.map((f) => `<code>${esc(f.file)}</code> (${esc(f.error)})`).join(", ")} —
        the rest of the page renders without ${failures.length === 1 ? "it" : "them"}.</div>`;
    }
    let sec = 0;
    const num = () => String(++sec).padStart(2, "0");
    if (headline && headline._profiles.length) {
      html += budgetSection(num(), manifest, targets, headline);
      html += headlineSection(num(), headline);
      if (headline._profiles.some((p) => p.data.mean_residue && p.data.core_leg && p.data.core_leg.present)) {
        html += splitSection(num(), headline);
      }
    }
    if (confirmation && confirmation._profiles.length) {
      html += confirmationSection(num(), confirmation, headline);
    }
    html += methodSection(num(), runs);
    report.innerHTML = html;
  }

  /* ---- boot ---- */
  async function getJSON(path) {
    const res = await fetch(path, { cache: "no-cache" });
    if (!res.ok) throw new Error("HTTP " + res.status);
    return res.json();
  }

  async function boot() {
    let manifest;
    try {
      manifest = await getJSON("txsub/index.json");
    } catch (err) {
      report.innerHTML = `<div class="error-box">Could not load <code>txsub/index.json</code>
        (${esc(err.message)}). Serve the folder with <code>make serve</code> — <code>file://</code> will not work.</div>`;
      return;
    }
    let targets = null;
    try { targets = await getJSON("targets.json"); } catch (err) { /* model tile omitted */ }

    const failures = [];
    await Promise.all((manifest.runs || []).map(async (run) => {
      run._profiles = [];
      const modes = Object.keys(run.summaries || {});
      await Promise.all(modes.map(async (mode) => {
        try {
          const data = await getJSON("txsub/" + run.summaries[mode]);
          run._profiles.push({ mode, data });
        } catch (err) {
          failures.push({ file: "txsub/" + run.summaries[mode], error: err.message });
        }
      }));
      run._profiles.sort((a, b) => modes.indexOf(a.mode) - modes.indexOf(b.mode));
    }));

    render(manifest, targets, failures);
  }

  boot();
})();
