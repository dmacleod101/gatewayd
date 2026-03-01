const fs = require("fs");
const path = require("path");
const http = require("http");
const https = require("https");
const { URL } = require("url");
const querystring = require("querystring");
const express = require("express");
const { chromium } = require("playwright");

/* -----------------------------
   Config
----------------------------- */

function readJson(p) {
  const raw = fs.readFileSync(p, "utf-8");
  return JSON.parse(raw);
}

const DEFAULT_CONFIG_PATH = "/etc/gatewayd/mcptt-bridge.json";
const cfgPath = process.env.MCPTT_BRIDGE_CONFIG || DEFAULT_CONFIG_PATH;

let CFG = {};
try {
  CFG = readJson(cfgPath);
} catch (e) {
  console.error(`[mcptt-bridge] Failed to read config ${cfgPath}:`, e.message || e);
  process.exit(2);
}

const LISTEN_HOST = CFG.listenHost || "127.0.0.1";
const LISTEN_PORT = Number(CFG.listenPort || 5072);

if (!Number.isFinite(LISTEN_PORT) || LISTEN_PORT <= 0 || LISTEN_PORT > 65535) {
  console.error(`[mcptt-bridge] Invalid listenPort: ${CFG.listenPort}`);
  process.exit(2);
}

/* -----------------------------
   Subscriber bearer management
----------------------------- */

let bearer = String(CFG.subscriberBearer || "").trim();
let bearerExpiryEpochMs = null; // best-effort
let bearerLastFetchEpochMs = null;
let bearerLastError = null;
let bearerRefreshTimer = null;

function redact(s) {
  if (!s) return "";
  const str = String(s);
  if (str.length <= 10) return "[REDACTED]";
  return str.slice(0, 6) + "..." + str.slice(-4);
}

function authEnabled() {
  return !!(CFG.auth && CFG.auth.enabled);
}

function parseTokenResponse(json) {
  const access =
    json?.access_token ||
    json?.AccessToken ||
    json?.token ||
    json?.Token ||
    null;

  const tokenType = (json?.token_type || json?.tokenType || "Bearer").toString();
  const expiresIn = Number(json?.expires_in || json?.expiresIn || 0);

  if (!access) {
    throw new Error("Token response missing access_token");
  }
  return {
    access_token: access,
    token_type: tokenType,
    expires_in: Number.isFinite(expiresIn) ? expiresIn : 0
  };
}

function httpRequestJson(urlStr, method, headers, bodyStr) {
  return new Promise((resolve, reject) => {
    const u = new URL(urlStr);
    const isHttps = u.protocol === "https:";

    const opts = {
      method,
      hostname: u.hostname,
      port: u.port || (isHttps ? 443 : 80),
      path: u.pathname + (u.search || ""),
      headers: headers || {}
    };

    const lib = isHttps ? https : http;
    const req = lib.request(opts, (res) => {
      let data = "";
      res.setEncoding("utf8");
      res.on("data", (chunk) => (data += chunk));
      res.on("end", () => {
        const status = res.statusCode || 0;
        if (status < 200 || status >= 300) {
          return reject(new Error(`HTTP ${status} ${res.statusMessage || ""} ${data}`.trim()));
        }
        try {
          const json = JSON.parse(data || "{}");
          resolve(json);
        } catch (e) {
          reject(new Error("Failed to parse JSON token response: " + (e?.message || e)));
        }
      });
    });

    req.on("error", reject);

    if (bodyStr) req.write(bodyStr);
    req.end();
  });
}

async function fetchSubscriberBearer() {
  if (!authEnabled()) return;

  const a = CFG.auth || {};
  const tokenUrl = String(a.token_url || "").trim();
  const clientId = String(a.client_id || "").trim();
  const clientSecret = String(a.client_secret || "").trim();
  const username = String(a.username || "").trim();
  const password = String(a.password || "").trim();
  const grantType = String(a.grant_type || "password").trim();
  const scope = String(a.scope || "").trim();

  if (!tokenUrl || !clientId || !clientSecret || !username || !password) {
    throw new Error("auth.enabled=true but auth fields are missing (token_url/client_id/client_secret/username/password)");
  }

  const form = {
    grant_type: grantType,
    client_id: clientId,
    client_secret: clientSecret,
    username,
    password
  };
  if (scope) form.scope = scope;

  const body = querystring.stringify(form);

  const headers = {
    "content-type": "application/x-www-form-urlencoded",
    "content-length": Buffer.byteLength(body)
  };

  const json = await httpRequestJson(tokenUrl, "POST", headers, body);
  const tok = parseTokenResponse(json);

  bearer = tok.access_token;
  bearerLastFetchEpochMs = Date.now();
  bearerLastError = null;

  if (tok.expires_in && tok.expires_in > 0) {
    bearerExpiryEpochMs = Date.now() + tok.expires_in * 1000;
  } else {
    bearerExpiryEpochMs = null;
  }

  console.log(`[mcptt-bridge] Bearer token obtained. token=${redact(bearer)} expires_in=${tok.expires_in || "unknown"}`);
}

function scheduleBearerRefresh() {
  if (!authEnabled()) return;

  if (bearerRefreshTimer) {
    clearTimeout(bearerRefreshTimer);
    bearerRefreshTimer = null;
  }

  const marginSec = Number(CFG.auth?.refresh_margin_seconds || 120);
  const marginMs = (Number.isFinite(marginSec) ? marginSec : 120) * 1000;

  let delayMs = 30 * 60 * 1000;

  if (bearerExpiryEpochMs) {
    const until = bearerExpiryEpochMs - Date.now() - marginMs;
    delayMs = Math.max(10 * 1000, Math.min(until, 6 * 60 * 60 * 1000));
  }

  bearerRefreshTimer = setTimeout(async () => {
    try {
      await fetchSubscriberBearer();
    } catch (e) {
      bearerLastError = String(e?.message || e);
      console.error("[mcptt-bridge] Bearer refresh failed:", bearerLastError);
    } finally {
      scheduleBearerRefresh();
    }
  }, delayMs);

  console.log(`[mcptt-bridge] Bearer refresh scheduled in ${(delayMs / 1000).toFixed(0)}s`);
}

/* -----------------------------
   Event buffer
----------------------------- */

const EVENT_BUFFER_MAX = Number(CFG.eventBufferMax || 5000);
let eventSeq = 0;
let eventBuffer = [];

function pushEvent(ev) {
  if (!ev || typeof ev !== "object") return;
  if (!ev.event_type || typeof ev.event_type !== "string") return;

  eventSeq += 1;
  const entry = { seq: eventSeq, ts: Date.now(), ...ev };
  eventBuffer.push(entry);

  if (eventBuffer.length > EVENT_BUFFER_MAX) {
    eventBuffer.splice(0, eventBuffer.length - EVENT_BUFFER_MAX);
  }
}

function parseCursor(s) {
  if (!s) return 0;
  const str = String(s).trim();
  if (str.startsWith("seq:")) {
    const n = Number(str.slice(4));
    return Number.isFinite(n) && n >= 0 ? Math.floor(n) : 0;
  }
  const n = Number(str);
  return Number.isFinite(n) && n >= 0 ? Math.floor(n) : 0;
}

function makeCursor(n) {
  return `seq:${n}`;
}

/* -----------------------------
   Headless runtime (actually headful under Xvfb)
----------------------------- */

let browser = null;
let context = null;
let page = null;

let headlessLastError = null;

let lastState = {
  connected: false,
  ptt: "idle",
  floorOwner: null,
  updatedAt: null
};

function buildMcpttConfigForPage() {
  return {
    msisdn: CFG.msisdn,
    groupId: CFG.groupId,
    voipHost: CFG.voipHost,
    baseUrl: CFG.baseUrl,
    subscriberBearer: bearer
  };
}

async function startHeadless() {
  headlessLastError = null;

  // IMPORTANT:
  // - We run Chromium "headful" so getUserMedia works, but you must run the service under Xvfb.
  // - Also remove Playwright default mute and auto-allow mic prompt.
  const runHeadless = (CFG.chromiumHeadless === true); // allow override, default false
  console.log(`[mcptt-bridge] Starting Chromium... headless=${runHeadless}`);

  browser = await chromium.launch({
    headless: runHeadless,
    ignoreDefaultArgs: ["--mute-audio"],
    args: [
      "--no-sandbox",
      "--disable-dev-shm-usage",
      "--disable-background-timer-throttling",
      "--disable-renderer-backgrounding",
      "--autoplay-policy=no-user-gesture-required",
      "--use-fake-ui-for-media-stream"
    ]
  });

  context = await browser.newContext({
    viewport: { width: 1280, height: 720 }
  });

  page = await context.newPage();

  page.on("console", (msg) => {
    try { console.log(`[page:${msg.type()}] ${msg.text()}`); } catch {}
  });

  page.on("pageerror", (err) => {
    const m = err?.message || String(err);
    console.error("[page:error]", m);
    headlessLastError = m;
  });

  // Inject config before any script runs
  await page.addInitScript((cfg) => {
    window.MCPTT_CONFIG = cfg;
    window.__bridgeReportState = window.__bridgeReportState || function () {};
    window.__bridgeEmitEvent = window.__bridgeEmitEvent || function () {};
  }, buildMcpttConfigForPage());

  await page.exposeFunction("__bridgeReportState", (st) => {
    lastState = { ...lastState, ...(st || {}), updatedAt: Date.now() };
  });

  await page.exposeFunction("__bridgeEmitEvent", (ev) => {
    pushEvent(ev);
  });

  const url = `http://${LISTEN_HOST}:${LISTEN_PORT}/ui/index.html`;
  const origin = `http://${LISTEN_HOST}:${LISTEN_PORT}`;

  // Explicitly grant mic permission to our origin
  try {
    await context.grantPermissions(["microphone"], { origin });
  } catch (e) {
    console.error("[mcptt-bridge] grantPermissions(microphone) failed:", e?.message || e);
  }

  console.log("[mcptt-bridge] Headless will load:", url);
  console.log("[mcptt-bridge] Using bearer:", redact(bearer));

  await page.goto(url, { waitUntil: "load", timeout: 60000 });

  if (CFG.autoStart === true) {
    console.log("[mcptt-bridge] autoStart enabled: starting session...");
    await page.evaluate(async () => window.mcpttBridge.startSession());
  }

  console.log("[mcptt-bridge] Headless runtime ready.");
}

/* -----------------------------
   Command helpers
----------------------------- */

async function requirePage() {
  if (!page) throw Object.assign(new Error("MCPTT runtime not started"), { httpStatus: 503 });
}

async function cmdEval(fnName) {
  await requirePage();
  try {
    return await page.evaluate(async (name) => {
      const b = window.mcpttBridge;
      if (!b || typeof b[name] !== "function") throw new Error(`mcpttBridge.${name} not available`);
      return await b[name]();
    }, fnName);
  } catch (e) {
    const err = new Error(e?.message || String(e));
    err.httpStatus = 409;
    throw err;
  }
}

/* -----------------------------
   Express API
----------------------------- */

const appEx = express();
appEx.use(express.json({ limit: "256kb" }));

// Serve the UI over HTTP instead of file://
const PUBLIC_DIR = path.join(__dirname, "public");
appEx.use("/ui", express.static(PUBLIC_DIR));

appEx.get("/api/health", (_req, res) => {
  res.json({
    ok: true,
    runtime: { started: !!page, eventSeq, now: Date.now(), headless_last_error: headlessLastError },
    state: lastState
  });
});

appEx.get("/api/auth/status", (_req, res) => {
  res.json({
    ok: true,
    auth: {
      enabled: authEnabled(),
      bearer_present: !!bearer,
      bearer_redacted: bearer ? redact(bearer) : null,
      last_fetch_ms: bearerLastFetchEpochMs,
      expires_ms: bearerExpiryEpochMs,
      last_error: bearerLastError
    }
  });
});

appEx.get("/api/events", (req, res) => {
  const cursor = parseCursor(req.query.cursor);
  const limitRaw = Number(req.query.limit || 200);
  const limit = Number.isFinite(limitRaw) ? Math.max(1, Math.min(1000, Math.floor(limitRaw))) : 200;

  const events = eventBuffer.filter(e => e.seq > cursor).slice(0, limit);
  const next = events.length ? events[events.length - 1].seq : cursor;

  const out = events.map(e => ({
    event_type: e.event_type,
    subscriber_id: e.subscriber_id,
    talkgroup_id: e.talkgroup_id,
    session_id: e.session_id,
    origin_id: e.origin_id,
    meta: { ...(e.meta || {}), ts: e.ts, seq: e.seq }
  }));

  res.json({ ok: true, events: out, next_cursor: makeCursor(next) });
});

function httpError(res, e) {
  const status = e?.httpStatus || 500;
  return res.status(status).json({ ok: false, error: String(e.message || e) });
}

appEx.post("/api/session/start", async (_req, res) => {
  try { await cmdEval("startSession"); res.json({ ok: true }); }
  catch (e) { httpError(res, e); }
});

appEx.post("/api/session/stop", async (_req, res) => {
  try { await cmdEval("stopSession"); res.json({ ok: true }); }
  catch (e) { httpError(res, e); }
});

appEx.post("/api/ptt/down", async (_req, res) => {
  try { await cmdEval("pttDown"); res.json({ ok: true }); }
  catch (e) { httpError(res, e); }
});

appEx.post("/api/ptt/up", async (_req, res) => {
  try { await cmdEval("pttUp"); res.json({ ok: true }); }
  catch (e) { httpError(res, e); }
});

const server = http.createServer(appEx);

/* -----------------------------
   Startup / shutdown
----------------------------- */

async function main() {
  if (authEnabled()) {
    try {
      await fetchSubscriberBearer();
    } catch (e) {
      bearerLastError = String(e?.message || e);
      console.error("[mcptt-bridge] Initial bearer fetch failed:", bearerLastError);
    }
    scheduleBearerRefresh();
  }

  await new Promise((resolve, reject) => {
    server.once("error", reject);
    server.listen(LISTEN_PORT, LISTEN_HOST, () => resolve());
  });

  console.log(`[mcptt-bridge] API listening on http://${LISTEN_HOST}:${LISTEN_PORT}`);

  // Start browser after HTTP server is up (so /ui works)
  try {
    await startHeadless();
  } catch (e) {
    headlessLastError = e?.message || String(e);
    console.error("[mcptt-bridge] Headless failed to start:", headlessLastError);
  }

  process.on("SIGTERM", shutdown);
  process.on("SIGINT", shutdown);
}

let shuttingDown = false;
async function shutdown() {
  if (shuttingDown) return;
  shuttingDown = true;

  console.log("[mcptt-bridge] Shutting down...");
  try { server.close(); } catch {}
  try { if (bearerRefreshTimer) clearTimeout(bearerRefreshTimer); } catch {}

  try { if (page) await page.close(); } catch {}
  try { if (context) await context.close(); } catch {}
  try { if (browser) await browser.close(); } catch {}

  process.exit(0);
}

main().catch((e) => {
  console.error("[mcptt-bridge] Fatal:", e?.stack || e?.message || e);
  process.exit(1);
});
