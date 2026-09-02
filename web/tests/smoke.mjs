// Phase-A e2e smoke (PRMT-150).
//
// Boots the built ops-portal under MOCK_GATEWAY=1, then asserts every
// functional point from spec-009 §8 Phase-A:
//   R1  /noc shell            — tree + 3 slots
//   R8  /noc?focus=…          — inspector fields + disabled action placeholder
//   R4  /noc?focus=…          — 4 data-label= entries
//   R5  /noc                  — 3 data-series= polylines
//   R6  /noc                  — site switcher + anomaly highlight
//   R3  /alarms               — ≥1 data-alarm-row
//   R3  /noc/cause/<crn>      — root cause + impact + disabled operate
//   SSE /api/stream/<site>    — text/event-stream + parseable TelemetryBatch
//   Auth                     — unauthenticated /noc → 302 /login
//
// Uses Node's built-in `node:assert`/`fetch` — no test framework added per §6.
// Exit codes: 0 = all assertions passed, 1 = any assertion failed.
import { spawn } from "node:child_process";
import { setTimeout as sleep } from "node:timers/promises";
import { fileURLToPath } from "node:url";
import { dirname, resolve } from "node:path";
import assert from "node:assert/strict";

const __dirname = dirname(fileURLToPath(import.meta.url));
const webRoot = resolve(__dirname, "..");
const port = Number(process.env.PORT ?? 3210);
const baseUrl = `http://127.0.0.1:${port}`;

const serverEntry = resolve(
  webRoot,
  "apps/ops-portal/build/server/index.js",
);

function fail(msg) {
  console.error(`smoke: FAIL — ${msg}`);
  process.exit(1);
}

async function waitForReady(child) {
  const deadline = Date.now() + 30_000;
  while (Date.now() < deadline) {
    if (child.exitCode !== null) {
      throw new Error(
        `server exited prematurely with code ${child.exitCode}`,
      );
    }
    try {
      const r = await fetch(`${baseUrl}/healthz`);
      if (r.status === 200) return;
    } catch {
      // not ready yet
    }
    await sleep(200);
  }
  throw new Error("server did not become ready within 30s");
}

const cases = [
  // R1: /noc → data-noc-ready, ≥1 data-tree-node, 3 data-slot=.
  {
    name: "R1 /noc shell — ready + tree + 3 slots",
    run: async () => {
      const r = await fetch(`${baseUrl}/noc`);
      assert.equal(r.status, 200, `status: ${r.status}`);
      const body = await r.text();
      assert.ok(body.includes("data-noc-ready"), "missing data-noc-ready");
      assert.ok(
        /data-tree-node=/.test(body),
        "missing ≥1 data-tree-node= entry",
      );
      for (const slot of ["inspector", "labels", "site-chart"]) {
        assert.ok(
          body.includes(`data-slot="${slot}"`),
          `missing data-slot="${slot}"`,
        );
      }
    },
  },

  // R8: /noc?focus=… → inspector fields + data-action-placeholder[disabled].
  {
    name: "R8 /noc?focus=… — inspector + disabled action placeholder",
    run: async () => {
      const r = await fetch(`${baseUrl}/noc?focus=site01.pod000.cdu000`);
      assert.equal(r.status, 200, `status: ${r.status}`);
      const body = await r.text();
      assert.ok(
        /data-inspector-crn="site01\.pod000\.cdu000"/.test(body),
        "missing data-inspector-crn value",
      );
      assert.ok(
        body.includes("data-inspector-id"),
        "missing data-inspector-id",
      );
      assert.ok(
        body.includes("data-inspector-status"),
        "missing data-inspector-status",
      );
      assert.ok(
        body.includes("data-action-placeholder"),
        "missing data-action-placeholder",
      );
      assert.ok(
        /<button[^>]*data-action-placeholder[^>]*disabled/.test(body) ||
          /<button[^>]*disabled[^>]*data-action-placeholder/.test(body),
        "data-action-placeholder is not disabled",
      );
    },
  },

  // R4: same focus → 4 data-label= entries (uptime, power, state, utilization).
  {
    name: "R4 /noc?focus=… — 4 data-label= entries",
    run: async () => {
      const r = await fetch(`${baseUrl}/noc?focus=site01.pod000.cdu000`);
      assert.equal(r.status, 200, `status: ${r.status}`);
      const body = await r.text();
      assert.ok(
        /data-labels-crn="site01\.pod000\.cdu000"/.test(body),
        "missing data-labels-crn value",
      );
      for (const key of ["uptime", "power", "state", "utilization"]) {
        assert.ok(
          body.includes(`data-label="${key}"`),
          `missing data-label="${key}"`,
        );
      }
    },
  },

  // R5: /noc → 3 data-series= (facility_power, it_power, pue).
  {
    name: "R5 /noc — 3 data-series= polylines",
    run: async () => {
      const r = await fetch(`${baseUrl}/noc`);
      assert.equal(r.status, 200, `status: ${r.status}`);
      const body = await r.text();
      for (const key of ["facility_power", "it_power", "pue"]) {
        const re = new RegExp(`<polyline[^>]*data-series="${key}"`);
        assert.ok(
          re.test(body),
          `missing <polyline data-series="${key}">`,
        );
      }
      assert.ok(
        body.includes("data-site-chart"),
        "missing data-site-chart wrapper",
      );
    },
  },

  // R6: /noc → data-site-switcher + ≥2 sites + ≥1 data-site-anomaly="true".
  {
    name: "R6 /noc — site switcher with ≥2 sites + anomaly highlight",
    run: async () => {
      const r = await fetch(`${baseUrl}/noc`);
      assert.equal(r.status, 200, `status: ${r.status}`);
      const body = await r.text();
      assert.ok(
        body.includes("data-site-switcher"),
        "missing data-site-switcher",
      );
      assert.ok(
        /data-site="site01"/.test(body),
        "missing data-site=\"site01\"",
      );
      assert.ok(
        /data-site="site02"/.test(body),
        "missing data-site=\"site02\"",
      );
      const anomalies = (body.match(/data-site-anomaly="true"/g) ?? []).length;
      assert.ok(
        anomalies >= 1,
        `data-site-anomaly="true" count: ${anomalies} (expected ≥1)`,
      );
    },
  },

  // R3: /alarms → ≥1 data-alarm-row.
  {
    name: "R3 /alarms — ≥1 data-alarm-row",
    run: async () => {
      const r = await fetch(`${baseUrl}/alarms`);
      assert.equal(r.status, 200, `status: ${r.status}`);
      const body = await r.text();
      assert.ok(
        body.includes("data-alarms-page"),
        "missing data-alarms-page",
      );
      const rowCount = (body.match(/data-alarm-row/g) ?? []).length;
      assert.ok(
        rowCount >= 1,
        `data-alarm-row count: ${rowCount} (expected ≥1)`,
      );
    },
  },

  // E3.5 / PRMT-156 + PRMT-199: /tickets → rows + operate transition controls.
  {
    name: "E3.5 /tickets — ready + ≥1 data-ticket-row + operate actions",
    run: async () => {
      const r = await fetch(`${baseUrl}/tickets`);
      assert.equal(r.status, 200, `status: ${r.status}`);
      const body = await r.text();
      assert.ok(
        body.includes("data-tickets-ready"),
        "missing data-tickets-ready",
      );
      assert.ok(
        body.includes("data-tickets-page"),
        "missing data-tickets-page",
      );
      const rowCount = (body.match(/data-ticket-row/g) ?? []).length;
      assert.ok(
        rowCount >= 1,
        `data-ticket-row count: ${rowCount} (expected ≥1)`,
      );
      assert.ok(
        body.includes("data-tickets-operate-ready") ||
          body.includes("data-ticket-actions"),
        "missing ticket operate affordance",
      );
      assert.ok(
        body.includes("data-ticket-action") ||
          body.includes("data-ticket-transition-form"),
        "missing per-row transition controls",
      );
    },
  },

  // E3.5 / PRMT-156: ?severity filter narrows the rows; ?cursor next-page
  // returns empty (loop terminator for the SSR test).
  {
    name: "E3.5 /tickets — filter narrows; cursor empties next page",
    run: async () => {
      const r1 = await fetch(`${baseUrl}/tickets?severity=critical`);
      assert.equal(r1.status, 200, `status: ${r1.status}`);
      const b1 = await r1.text();
      const criticalRows = (b1.match(/data-severity="critical"/g) ?? [])
        .length;
      assert.ok(
        criticalRows >= 1,
        `critical-only rows: ${criticalRows} (expected ≥1)`,
      );
      // All rendered ticket rows must carry severity=critical under the filter.
      const allRows = (b1.match(/data-ticket-row/g) ?? []).length;
      assert.equal(
        criticalRows,
        allRows,
        `filter mismatch: ${criticalRows} critical vs ${allRows} total rows`,
      );

      const r2 = await fetch(`${baseUrl}/tickets?cursor=page-2`);
      assert.equal(r2.status, 200, `status: ${r2.status}`);
      const b2 = await r2.text();
      const page2Rows = (b2.match(/data-ticket-row/g) ?? []).length;
      assert.equal(
        page2Rows,
        0,
        `data-ticket-row on page-2: ${page2Rows} (expected 0)`,
      );
      assert.ok(
        b2.includes("data-tickets-empty"),
        "missing data-tickets-empty on page-2",
      );
    },
  },

  // E3.5 / PRMT-158: /maintenance → data-maintenance-ready + ≥1
  // data-maintenance-row. Read-only upcoming page over
  // /api/maintenance/upcoming (mocked) — mirrors /capacity structure.
  {
    name: "E3.5 /maintenance — ready + ≥1 data-maintenance-row",
    run: async () => {
      const r = await fetch(`${baseUrl}/maintenance`);
      assert.equal(r.status, 200, `status: ${r.status}`);
      const body = await r.text();
      assert.ok(
        body.includes("data-maintenance-ready"),
        "missing data-maintenance-ready",
      );
      assert.ok(
        body.includes("data-maintenance-page"),
        "missing data-maintenance-page",
      );
      const rowCount = (body.match(/data-maintenance-row/g) ?? []).length;
      assert.ok(
        rowCount >= 1,
        `data-maintenance-row count: ${rowCount} (expected ≥1)`,
      );
    },
  },

  // E3.5 / PRMT-159: /spares → data-spares-ready + ≥1 data-spare-row
  // + ≥1 data-low-stock="true" (mock fixture seeds qty<min_qty on
  // the first row). Read-only inventory page over /api/spares
  // (mocked) — mirrors /tickets structure (PRMT-156 exemplar).
  {
    name: "E3.5 /spares — ready + ≥1 data-spare-row + ≥1 low-stock row",
    run: async () => {
      const r = await fetch(`${baseUrl}/spares`);
      assert.equal(r.status, 200, `status: ${r.status}`);
      const body = await r.text();
      assert.ok(
        body.includes("data-spares-page"),
        "missing data-spares-page",
      );
      assert.ok(
        body.includes("data-spares-ready"),
        "missing data-spares-ready",
      );
      const rowCount = (body.match(/data-spare-row/g) ?? []).length;
      assert.ok(
        rowCount >= 1,
        `data-spare-row count: ${rowCount} (expected ≥1)`,
      );
      const lowCount = (body.match(/data-low-stock="true"/g) ?? []).length;
      assert.ok(
        lowCount >= 1,
        `data-low-stock="true" count: ${lowCount} (expected ≥1)`,
      );
    },
  },

  // E3.5 / PRMT-159: /spares?cursor=page-2 → empty page + data-spares-empty.
  {
    name: "E3.5 /spares — cursor empties next page",
    run: async () => {
      const r = await fetch(`${baseUrl}/spares?cursor=page-2`);
      assert.equal(r.status, 200, `status: ${r.status}`);
      const body = await r.text();
      const rowCount = (body.match(/data-spare-row/g) ?? []).length;
      assert.equal(
        rowCount,
        0,
        `data-spare-row on page-2: ${rowCount} (expected 0)`,
      );
      assert.ok(
        body.includes("data-spares-empty"),
        "missing data-spares-empty on page-2",
      );
    },
  },

  // E3.5 / PRMT-160: /inspections → data-inspections-ready + ≥1
  // data-inspection-row + ≥1 disabled operate placeholder + ≥1 row
  // with enabled=false (mock fixture seeds row 2 with enabled=false).
  // Read-only inspection-template page over /api/inspections
  // (mocked) — mirrors /spares structure (PRMT-159 exemplar).
  // Mobile checklist form (M2 P561) is intentionally NOT covered here.
  {
    name: "E3.5 /inspections — ready + ≥1 data-inspection-row + disabled operate",
    run: async () => {
      const r = await fetch(`${baseUrl}/inspections`);
      assert.equal(r.status, 200, `status: ${r.status}`);
      const body = await r.text();
      assert.ok(
        body.includes("data-inspections-page"),
        "missing data-inspections-page",
      );
      assert.ok(
        body.includes("data-inspections-ready"),
        "missing data-inspections-ready",
      );
      const rowCount = (body.match(/data-inspection-row/g) ?? []).length;
      assert.ok(
        rowCount >= 1,
        `data-inspection-row count: ${rowCount} (expected ≥1)`,
      );
      assert.ok(
        body.includes("data-inspections-operate-placeholder"),
        "missing data-inspections-operate-placeholder",
      );
      assert.ok(
        /<button[^>]*data-inspections-operate-placeholder[^>]*disabled/.test(
          body,
        ) ||
          /<button[^>]*disabled[^>]*data-inspections-operate-placeholder/.test(
            body,
          ),
        "data-inspections-operate-placeholder is not disabled",
      );
      const disabledRows = (body.match(
        /data-inspection-enabled="false"/g,
      ) ?? []).length;
      assert.ok(
        disabledRows >= 1,
        `data-inspection-enabled="false" count: ${disabledRows} (expected ≥1)`,
      );
    },
  },

  // E3.5 / PRMT-161: /runbooks → data-runbooks-ready + ≥1
  // data-case-row (mock fixture seeds 3 closed cases). Read-only KB
  // viewer over /api/cases + /api/runbooks/{key} (mocked) — mirrors
  // /tickets structure (PRMT-156 exemplar).
  {
    name: "E3.5 /runbooks — ready + ≥1 data-case-row (list-only)",
    run: async () => {
      const r = await fetch(`${baseUrl}/runbooks`);
      assert.equal(r.status, 200, `status: ${r.status}`);
      const body = await r.text();
      assert.ok(
        body.includes("data-runbooks-page"),
        "missing data-runbooks-page",
      );
      assert.ok(
        body.includes("data-runbooks-ready"),
        "missing data-runbooks-ready",
      );
      const rowCount = (body.match(/data-case-row/g) ?? []).length;
      assert.ok(
        rowCount >= 1,
        `data-case-row count: ${rowCount} (expected ≥1)`,
      );
      // Without ?key= the runbook detail block must be omitted
      // (PRMT-161 §5 "missing key → list-only render").
      assert.ok(
        !body.includes("data-runbook-detail"),
        "data-runbook-detail should be absent when ?key= is missing",
      );
    },
  },

  // E3.5 / PRMT-161: /runbooks?key=<known> → also renders
  // data-runbook-detail (mock seam wraps markdown body).
  {
    name: "E3.5 /runbooks — ?key= renders data-runbook-detail",
    run: async () => {
      const r = await fetch(
        `${baseUrl}/runbooks?key=rb%2Fcdu-deltat-low`,
      );
      assert.equal(r.status, 200, `status: ${r.status}`);
      const body = await r.text();
      assert.ok(
        body.includes("data-runbooks-page"),
        "missing data-runbooks-page",
      );
      assert.ok(
        body.includes("data-runbook-detail"),
        "missing data-runbook-detail",
      );
      assert.ok(
        /data-runbook-key="rb\/cdu-deltat-low"/.test(body),
        "missing data-runbook-key with the requested value",
      );
      assert.ok(
        body.includes("CDU deltaT low"),
        "missing expected runbook title in body",
      );
    },
  },

  // E3.5 / PRMT-162: /reports → data-reports-ready + ≥1
  // data-report-metric (mock fixture seeds MTTR / mean response /
  // MTBF + alarm_top). Read-only ops report page over
  // /api/reports/ops (mocked) — mirrors /capacity structure
  // (PRMT-157 exemplar). Optional ?since= is forwarded to the
  // mock seam and echoed back in the body (verified by the
  // second case).
  {
    name: "E3.5 /reports — ready + ≥1 data-report-metric",
    run: async () => {
      const r = await fetch(`${baseUrl}/reports`);
      assert.equal(r.status, 200, `status: ${r.status}`);
      const body = await r.text();
      assert.ok(
        body.includes("data-reports-page"),
        "missing data-reports-page",
      );
      assert.ok(
        body.includes("data-reports-ready"),
        "missing data-reports-ready",
      );
      const metricCount = (body.match(/data-report-metric=/g) ?? []).length;
      assert.ok(
        metricCount >= 1,
        `data-report-metric count: ${metricCount} (expected ≥1)`,
      );
      // Spot-check that each spec-named metric is rendered (MTTR /
      // mean_response / MTBF). Wire shape pinned to core.opsReportResponse
      // (core/reports.go L31-38); ui mirror (report-panel.tsx) names
      // each metric on its row for this exact purpose.
      for (const key of ["mttr", "mean_response", "mtbf"]) {
        assert.ok(
          body.includes(`data-report-metric="${key}"`),
          `missing data-report-metric="${key}"`,
        );
      }
    },
  },

  // E3.5 / PRMT-162: /reports?since=<RFC3339> → page renders the
  // echoed window.since value in data-reports-since (loader passes
  // ?since= to the mock seam, mock echoes it in window.since).
  {
    name: "E3.5 /reports — ?since= echoes window.since",
    run: async () => {
      const since = "2026-06-21T00:00:00Z";
      const r = await fetch(
        `${baseUrl}/reports?since=${encodeURIComponent(since)}`,
      );
      assert.equal(r.status, 200, `status: ${r.status}`);
      const body = await r.text();
      assert.ok(
        body.includes("data-reports-page"),
        "missing data-reports-page",
      );
      assert.ok(
        body.includes("data-reports-ready"),
        "missing data-reports-ready",
      );
      assert.ok(
        body.includes(`data-reports-since="${since}"`),
        `missing data-reports-since="${since}"`,
      );
    },
  },

  // R3: /noc/cause/<crn> → data-root-cause + ≥1 data-impact-item +
  // data-operate-placeholder[disabled].
  {
    name: "R3 /noc/cause/<crn> — root cause + impact + disabled operate",
    run: async () => {
      const target = "site01.pod000.cdu000";
      const r = await fetch(`${baseUrl}/noc/cause/${target}`);
      assert.equal(r.status, 200, `status: ${r.status}`);
      const body = await r.text();
      assert.ok(
        body.includes("data-cause-ready"),
        "missing data-cause-ready",
      );
      assert.ok(
        body.includes("data-root-cause"),
        "missing data-root-cause",
      );
      const impactCount = (body.match(/data-impact-item/g) ?? []).length;
      assert.ok(
        impactCount >= 1,
        `data-impact-item count: ${impactCount} (expected ≥1)`,
      );
      assert.ok(
        body.includes("data-operate-placeholder"),
        "missing data-operate-placeholder",
      );
      assert.ok(
        /<button[^>]*data-operate-placeholder[^>]*disabled/.test(body) ||
          /<button[^>]*disabled[^>]*data-operate-placeholder/.test(body),
        "data-operate-placeholder is not disabled",
      );
    },
  },


  // SSE: /api/stream/<site> → text/event-stream + parseable TelemetryBatch.
  {
    name: "SSE /api/stream/<site> — text/event-stream + TelemetryBatch",
    run: async () => {
      const ctrl = new AbortController();
      const t = setTimeout(() => ctrl.abort(), 5000);
      let r;
      try {
        r = await fetch(`${baseUrl}/api/stream/site01`, {
          signal: ctrl.signal,
        });
      } finally {
        clearTimeout(t);
      }
      assert.equal(r.status, 200, `status: ${r.status}`);
      const ct = r.headers.get("content-type") ?? "";
      assert.ok(
        ct.includes("text/event-stream"),
        `content-type: ${ct}`,
      );
      assert.ok(r.body, "missing body");
      const reader = r.body.getReader();
      const dec = new TextDecoder();
      let buf = "";
      let dataLine = null;
      const readDeadline = Date.now() + 5000;
      while (dataLine === null && Date.now() < readDeadline) {
        const { value, done } = await reader.read();
        if (done) break;
        buf += dec.decode(value, { stream: true });
        const m = /\ndata: (\{[^\n]*\})\n/.exec(buf);
        if (m && m[1]) {
          dataLine = m[1];
          break;
        }
      }
      try {
        await reader.cancel();
      } catch {
        // already closed
      }
      assert.ok(
        dataLine !== null,
        "no data: JSON line within 5s",
      );
      const batch = JSON.parse(dataLine);
      assert.equal(batch.site, "site01", `batch.site: ${batch.site}`);
      assert.equal(
        batch.encoding,
        "promtext",
        `batch.encoding: ${batch.encoding}`,
      );
      assert.ok(
        Array.isArray(batch.lines) && batch.lines.length >= 1,
        "batch.lines empty or non-array",
      );
      const sampleRe =
        /^cios_(power_w|utilization_ratio|uptime_s|state)\{crn="site01[^"]*"\} -?\d+(\.\d+)?/;
      assert.ok(
        batch.lines.some((l) => sampleRe.test(l)),
        `batch.lines has no recognisable promtext sample: ${JSON.stringify(batch.lines)}`,
      );
    },
  },

  // E3.5: /assets CMDB table
  {
    name: "E3.5 /assets — ready + ≥1 data-asset-row",
    run: async () => {
      const r = await fetch(`${baseUrl}/assets`);
      assert.equal(r.status, 200, `status: ${r.status}`);
      const body = await r.text();
      assert.ok(body.includes("data-assets-ready"), "missing data-assets-ready");
      const rowCount = (body.match(/data-asset-row/g) ?? []).length;
      assert.ok(rowCount >= 1, `data-asset-row count: ${rowCount}`);
    },
  },

  // E3.2 / PRMT-194: /usage → data-usage-ready + ≥1 data-usage-row
  // (mock seeds energy + rack_hour). Read-only 对量 facts over
  // /api/usage (mocked) — no money/invoice UI.
  {
    name: "PRMT-194 /usage — ready + ≥1 data-usage-row (energy + rack_hour)",
    run: async () => {
      const r = await fetch(`${baseUrl}/usage`);
      assert.equal(r.status, 200, `status: ${r.status}`);
      const body = await r.text();
      assert.ok(
        body.includes("data-usage-page"),
        "missing data-usage-page",
      );
      assert.ok(
        body.includes("data-usage-ready"),
        "missing data-usage-ready",
      );
      const rowCount = (body.match(/data-usage-row/g) ?? []).length;
      assert.ok(
        rowCount >= 1,
        `data-usage-row count: ${rowCount} (expected ≥1)`,
      );
      assert.ok(
        /data-usage-kind="energy"/.test(body),
        'missing data-usage-kind="energy"',
      );
      assert.ok(
        /data-usage-kind="rack_hour"/.test(body),
        'missing data-usage-kind="rack_hour"',
      );
    },
  },
  // Auth: unauthenticated /noc → 302 /login?next=/noc.
  {
    name: "Auth — unauthenticated /noc → 302 /login?next=/noc",
    run: async () => {
      // MOCK_GATEWAY=1 short-circuits getSession() to mockSession() (always
      // "logged in"). To exercise the unauthenticated redirect we must
      // temporarily flip MOCK_GATEWAY off; do so by starting a second
      // server on a different port without the env var.
      const anonPort = port + 1;
      const anon = spawn(
        process.execPath,
        [
          resolve(webRoot, "apps/ops-portal/node_modules/@react-router/serve/bin.js"),
          serverEntry,
        ],
        {
          cwd: webRoot,
          env: {
            ...process.env,
            PORT: String(anonPort),
            NODE_ENV: "production",
            // Force the no-mock path: getSession reads the cios_session
            // cookie, finds none, returns null → requireSession throws 302.
            MOCK_GATEWAY: "",
            GATEWAY_BASE_URL: "http://127.0.0.1:1",
          },
          stdio: ["ignore", "pipe", "pipe"],
        },
      );
      let anonLog = "";
      anon.stdout.on("data", (d) => {
        anonLog += d.toString();
      });
      anon.stderr.on("data", (d) => {
        anonLog += d.toString();
      });
      try {
        // Wait for readiness on the anon port.
        const deadline = Date.now() + 30_000;
        let ready = false;
        while (Date.now() < deadline) {
          if (anon.exitCode !== null) {
            throw new Error(
              `anon server exited prematurely with code ${anon.exitCode}`,
            );
          }
          try {
            const h = await fetch(`http://127.0.0.1:${anonPort}/healthz`);
            if (h.status === 200) {
              ready = true;
              break;
            }
          } catch {
            // not ready
          }
          await sleep(200);
        }
        assert.ok(ready, "anon server did not become ready within 30s");

        const r = await fetch(
          `http://127.0.0.1:${anonPort}/noc`,
          { redirect: "manual" },
        );
        assert.equal(r.status, 302, `status: ${r.status}`);
        const loc = r.headers.get("location") ?? "";
        // Location format: `/login?next=%2Fnoc` (URL-encoded next path).
        // Decode and check the prefix + the encoded payload references /noc.
        assert.ok(
          loc.startsWith("/login?next="),
          `Location header missing /login?next= prefix: ${loc}`,
        );
        const decoded = decodeURIComponent(loc.split("=", 2)[1] ?? "");
        assert.ok(
          decoded === "/noc" || decoded.startsWith("/noc?"),
          `Decoded next param not /noc: ${decoded} (raw: ${loc})`,
        );
      } finally {
        anon.kill("SIGTERM");
        await sleep(200);
        if (anon.exitCode === null) anon.kill("SIGKILL");
        // surface anon log only on failure path
        if (process.env.SMOKE_KEEP_ANON_LOG === "1") {
          console.error("anon log:\n" + anonLog);
        }
      }
    },
  },
];

async function main() {
  console.log(`smoke: starting built server on :${port}`);
  const child = spawn(
    process.execPath,
    [
      resolve(webRoot, "apps/ops-portal/node_modules/@react-router/serve/bin.js"),
      serverEntry,
    ],
    {
      cwd: webRoot,
      env: {
        ...process.env,
        PORT: String(port),
        NODE_ENV: "production",
      },
      stdio: ["ignore", "pipe", "pipe"],
    },
  );

  let serverLog = "";
  child.stdout.on("data", (d) => {
    serverLog += d.toString();
  });
  child.stderr.on("data", (d) => {
    serverLog += d.toString();
  });

  let passed = 0;
  let failed = 0;
  try {
    await waitForReady(child);
    for (const c of cases) {
      try {
        await c.run();
        console.log(`smoke: PASS — ${c.name}`);
        passed += 1;
      } catch (err) {
        console.error(`smoke: FAIL — ${c.name}`);
        console.error(`         ${err && err.message ? err.message : err}`);
        failed += 1;
      }
    }
  } catch (err) {
    console.error("smoke: error during setup:", err);
    console.error("smoke: server output:\n" + serverLog);
    fail(err && err.message ? err.message : String(err));
  } finally {
    child.kill("SIGTERM");
    await sleep(200);
    if (child.exitCode === null) {
      child.kill("SIGKILL");
    }
  }

  console.log(`smoke: ${passed} passed, ${failed} failed (of ${cases.length})`);
  if (failed > 0) {
    process.exit(1);
  }
  console.log("smoke: ALL ASSERTIONS PASSED");
}

main().catch((err) => {
  console.error("smoke: unhandled error:", err);
  fail(err && err.message ? err.message : String(err));
});