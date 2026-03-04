 (function () {
  // Node injects config before load as window.MCPTT_CONFIG
  const CFG = window.MCPTT_CONFIG || {};
  const BASE_URL = CFG.baseUrl || "https://api-frontend.mcptt.com.au";
  const VOIP_TOKEN_URL = BASE_URL + "/api/v1/subscriber/self/voip?filter=getVoipToken";

  let ua = null;
  let call = null;

  let myMsisdn = "";
  let isPttHeld = false;
  let lastFloorRequestAt = 0;
  const FLOOR_REQUEST_WINDOW_MS = 4000;

  // Internal state reported to Node
  // - connected: BACK-COMPAT; now means SIP/WebSocket transport connected (not call active)
  // - sip_transport: disconnected|connecting|connected
  // - registered: true|false|null (null = unknown)
  // - call_active: true when call is active
  let state = {
    connected: false,
    sip_transport: "disconnected",
    registered: null,
    call_active: false,

    ptt: "idle", // idle|pending|tx|rx
    floorOwner: null
  };

  function now() { return Date.now(); }

  function log(msg) {
    try { console.log(`[MCPTT] ${msg}`); } catch {}
  }

  function reportState(patch) {
    state = { ...state, ...(patch || {}) };
    try {
      if (typeof window.__bridgeReportState === "function") {
        window.__bridgeReportState({ ...state, updatedAt: now() });
      }
    } catch {
      // never break
    }
  }

  function emitEvent(event_type, extra = {}) {
    try {
      if (typeof window.__bridgeEmitEvent === "function") {
        window.__bridgeEmitEvent({
          event_type,
          origin_id: "mcptt_bridge",
          ...extra
        });
      }
    } catch {
      // never break
    }
  }

  function resetFloorUi() {
    isPttHeld = false;
    lastFloorRequestAt = 0;
    reportState({ ptt: "idle", floorOwner: null });
  }

  function showTx() {
    reportState({ ptt: "tx", floorOwner: myMsisdn || "you" });
  }

  function showRx(label) {
    reportState({ ptt: "rx", floorOwner: label || "other" });
  }

  function showPending() {
    reportState({ ptt: "pending" });
  }

  function isLikelyOurGrant() {
    const t = now();
    return isPttHeld || (lastFloorRequestAt && (t - lastFloorRequestAt) <= FLOOR_REQUEST_WINDOW_MS);
  }

  // ---------------------------
  // SIP lifecycle helpers (best-effort)
  // ---------------------------

  function setTransport(status, meta) {
    // Avoid spamming state/events if nothing changed
    if (state.sip_transport === status && state.connected === (status === "connected")) return;

    reportState({
      sip_transport: status,
      connected: status === "connected"
    });

    // Emit low-noise lifecycle events
    if (status === "connected") emitEvent("sip_transport_up", { meta });
    if (status === "disconnected") emitEvent("sip_transport_down", { meta });
    if (status === "connecting") emitEvent("sip_transport_connecting", { meta });
  }

  function setRegistered(val, meta) {
    if (state.registered === val) return;
    reportState({ registered: val });

    if (val === true) emitEvent("sip_registered", { meta });
    if (val === false) emitEvent("sip_unregistered", { meta });
  }

  function attachSipLifecycle(uaObj) {
    if (!uaObj) return;

    // 1) If SWSIPVOIP2.UA exposes EventEmitter style hooks
    try {
      if (typeof uaObj.on === "function") {
        // These event names are best-guess; harmless if never fired.
        uaObj.on("connecting", () => setTransport("connecting"));
        uaObj.on("connected", () => setTransport("connected"));
        uaObj.on("disconnected", (e) =>
          setTransport("disconnected", { reason: e?.reason || e?.cause || null })
        );

        uaObj.on("registered", () => setRegistered(true));
        uaObj.on("unregistered", () => setRegistered(false));
        uaObj.on("registrationFailed", (e) =>
          setRegistered(false, { reason: e?.cause || e?.reason || null })
        );
      }
    } catch {}

    // 2) Try to find underlying JsSIP UA / transport / socket
    // Common internal shapes: _ua, ua, jssipUa, _jssip, _jssipUa
    const inner =
      uaObj._ua || uaObj.ua || uaObj.jssipUa || uaObj._jssip || uaObj._jssipUa || null;

    if (inner && typeof inner.on === "function") {
      try {
        inner.on("connected", () => setTransport("connected"));
        inner.on("disconnected", (e) =>
          setTransport("disconnected", { reason: e?.cause || e?.reason || null })
        );
        inner.on("registered", () => setRegistered(true));
        inner.on("unregistered", () => setRegistered(false));
        inner.on("registrationFailed", (e) =>
          setRegistered(false, { reason: e?.cause || e?.reason || null })
        );
      } catch {}
    }

    // 3) Transport object (JsSIP transport emits connect/disconnect/connecting in some builds)
    const transport =
      inner?.transport ||
      inner?._transport ||
      inner?.socket ||
      inner?._socket ||
      uaObj.transport ||
      null;

    if (transport && typeof transport.on === "function") {
      try {
        transport.on("connecting", () => setTransport("connecting"));
        transport.on("connected", () => setTransport("connected"));
        transport.on("disconnected", (e) =>
          setTransport("disconnected", { reason: e?.reason || e?.cause || null })
        );
      } catch {}
    }
  }

  // ---------------------------
  // WebSocket readyState probe (reliable fallback)
  // ---------------------------

  function findWebSocket(obj, depth = 0, seen = new Set()) {
    if (!obj || typeof obj !== "object") return null;
    if (seen.has(obj)) return null;
    seen.add(obj);

    // Direct WebSocket instance
    try {
      if (typeof WebSocket !== "undefined" && obj instanceof WebSocket) return obj;
    } catch {}

    // Heuristic: "looks like" a WebSocket
    if (typeof obj.readyState === "number" && typeof obj.send === "function") {
      return obj;
    }

    if (depth >= 4) return null;

    let keys = [];
    try { keys = Object.keys(obj); } catch { return null; }

    for (const k of keys) {
      if (k === "window" || k === "document") continue;
      let v;
      try { v = obj[k]; } catch { continue; }
      const ws = findWebSocket(v, depth + 1, seen);
      if (ws) return ws;
    }
    return null;
  }

  let wsProbeTimer = null;

  function startWsProbe() {
    if (wsProbeTimer) return;
    wsProbeTimer = setInterval(() => {
      try {
        const root = call || ua;
        const ws = findWebSocket(root);
        if (!ws) return;

        // 0 connecting, 1 open, 2 closing, 3 closed
        const rs = ws.readyState;
        if (rs === 1) setTransport("connected", { via: "ws_probe" });
        else if (rs === 0) setTransport("connecting", { via: "ws_probe" });
        else setTransport("disconnected", { via: "ws_probe", readyState: rs });
      } catch {
        // ignore
      }
    }, 1000);
  }

  function stopWsProbe() {
    if (!wsProbeTimer) return;
    clearInterval(wsProbeTimer);
    wsProbeTimer = null;
  }

  // ---------------------------
  // VoIP token fetch helpers
  // ---------------------------

  function extractVoipTokenFromResponse(json) {
    const candidates = [
      json?.results?.VoipToken,
      json?.results?.voipToken,
      json?.results?.token,
      json?.results?.Token,
      json?.VoipToken,
      json?.voipToken,
      json?.token,
      json?.Token
    ].filter(Boolean);

    return candidates.length ? String(candidates[0]) : null;
  }

  async function fetchVoipToken(subscriberBearer) {
    log("Fetching VoIP token...");
    const resp = await fetch(VOIP_TOKEN_URL, {
      method: "GET",
      headers: {
        authorization: "Bearer " + subscriberBearer,
        "content-type": "application/json"
      }
    });

    if (!resp.ok) {
      const text = await resp.text().catch(() => "");
      throw new Error(
        `VoIP token fetch failed: HTTP ${resp.status} ${resp.statusText} ${text}`.trim()
      );
    }

    const json = await resp.json();
    const token = extractVoipTokenFromResponse(json);

    if (!token) {
      throw new Error("VoIP token not found in response JSON (field name mismatch).");
    }

    log("VoIP token fetched OK");
    return token;
  }

  // ---------------------------
  // Public API (called by Node via page.evaluate)
  // ---------------------------

  async function startSession() {
    if (call) return;

    const msisdn = String(CFG.msisdn || "").trim();
    const groupId = String(CFG.groupId || "").trim();
    const subscriberBearer = String(CFG.subscriberBearer || "").trim();
    const voipHost = String(CFG.voipHost || "").trim();

    if (!msisdn || !groupId || !subscriberBearer || !voipHost) {
      throw new Error("Missing required config: msisdn, groupId, subscriberBearer, voipHost");
    }

    myMsisdn = msisdn;
    resetFloorUi();

    // SDK logging (useful while bringing up)
    window.SWSIPVOIP2.enableLog("SWSIPVOIP2:*,JsSIP:*");

    const voipToken = await fetchVoipToken(subscriberBearer);

    ua = new window.SWSIPVOIP2.UA({ host: voipHost, port: 5063 });

    // Best-effort: attach SIP transport + registration lifecycle hooks
    attachSipLifecycle(ua);

    // Reliable fallback: probe WebSocket readyState
    startWsProbe();

    // If we don't get explicit lifecycle events, at least show "connecting" now.
    setTransport("connecting", { reason: "startSession" });

    const config = {
      subscriberMsisdn: msisdn,
      sessionMedia: window.SWSIPVOIP2.Call.SessionMedia.AUDIO,
      sessionType: window.SWSIPVOIP2.Call.SessionType.WALKIETALKIE,
      sessionId: "group:" + groupId,
      getSubscriberToken: () => Promise.resolve(voipToken),
      voipConfig: {
        SERVER_PORT: 5063,
        HTTP: "https://",
        WS: "wss://",
        SIP: "sip:",
        HOST: voipHost,
        SIP_HOST: voipHost
      }
    };

    call = ua.newCall(config);

    call.on(window.SWSIPVOIP2.Call.Event.STARTED, () => {
      log("Joined group channel");
      reportState({ call_active: true });
      emitEvent("call_up");
    });

    call.on(window.SWSIPVOIP2.Call.Event.FAILED, () => {
      log("Call FAILED");
      reportState({ call_active: false });
      resetFloorUi();
      emitEvent("call_down", { meta: { reason: "FAILED" } });
      call = null;

      // IMPORTANT:
      // - do not mark transport disconnected here (call != transport)
      // - keep ua so wsProbe can keep reporting transport while idle
    });

    call.on(window.SWSIPVOIP2.Call.Event.STOPPED, () => {
      log("Call STOPPED");
      reportState({ call_active: false });
      resetFloorUi();
      emitEvent("call_down", { meta: { reason: "STOPPED" } });
      call = null;

      // IMPORTANT:
      // - do not mark transport disconnected here (call != transport)
      // - keep ua so wsProbe can keep reporting transport while idle
    });

    // Floor control events -> rx/tx state
    const FC = window.SWSIPVOIP2.FloorControl;
    const base = window.SWSIPVOIP2.Call.Event.FLOOR_CONTROL + ":";

    call.on(base + FC.Event.GRANTED, () => {
      if (isLikelyOurGrant()) {
        showTx();
        emitEvent("tx_start");
        log("Floor GRANTED (you)");
      } else {
        showRx("other");
        emitEvent("rx_start");
        log("Floor GRANTED (other)");
      }
    });

    call.on(base + FC.Event.TAKEN, () => {
      if (isLikelyOurGrant()) {
        showTx();
        emitEvent("tx_start");
        log("Floor TAKEN (you)");
      } else {
        showRx("other");
        emitEvent("rx_start");
        log("Floor TAKEN (other)");
      }
    });

    call.on(base + FC.Event.IDLE, () => {
      // ensure we close out rx/tx before idle
      if (state.ptt === "tx") emitEvent("tx_stop");
      if (state.ptt === "rx") emitEvent("rx_stop");
      resetFloorUi();
      log("Floor IDLE");
    });

    log("Starting call...");
    call.start();
  }

  async function stopSession() {
    if (call) {
      try { call.stop(); } catch {}
    }
    call = null;

    // Best-effort: stop/terminate UA if supported
    try { if (ua && typeof ua.stop === "function") ua.stop(); } catch {}
    try { if (ua && typeof ua.terminate === "function") ua.terminate(); } catch {}

    ua = null;

    // Stop probe on explicit stop
    stopWsProbe();

    // Explicitly mark link down on manual stop
    setTransport("disconnected", { reason: "STOP" });
    // #this line commented out for removing false health status. Will remove after further testing# setRegistered(false, { reason: "STOP" });
    reportState({ call_active: false });

    resetFloorUi();
    emitEvent("link_down", { meta: { reason: "STOP" } });
  }

  function pttDown() {
    if (!call) throw new Error("Cannot PTT: no active call");
    isPttHeld = true;
    lastFloorRequestAt = now();
    showPending();
    emitEvent("local_ptt_down");
    log("PTT down -> requestFloor()");
    call.requestFloor();
  }

  function pttUp() {
    if (!call) throw new Error("Cannot release PTT: no active call");
    isPttHeld = false;
    emitEvent("local_ptt_up");
    log("PTT up -> releaseFloor()");
    call.releaseFloor();
  }

  // Expose API for Node bridge.js
  window.mcpttBridge = {
    startSession,
    stopSession,
    pttDown,
    pttUp,
    getState: () => ({ ...state })
  };

  // ---------------------------
  // State heartbeat (keeps /api/health.state.updatedAt fresh while idle)
  // ---------------------------
  const HEARTBEAT_MS = Number(CFG.stateHeartbeatMs || 1000);
  setInterval(() => {
    try {
      if (typeof window.__bridgeReportState === "function") {
        window.__bridgeReportState({ ...state, updatedAt: now() });
      }
    } catch {
      // never break
    }
  }, (Number.isFinite(HEARTBEAT_MS) && HEARTBEAT_MS >= 250) ? HEARTBEAT_MS : 1000);


  // Initial state/event
  reportState({
    connected: false,
    sip_transport: "disconnected",
    registered: null,
    call_active: false,
    ptt: "idle",
    floorOwner: null
  });
  emitEvent("link_down", { meta: { reason: "INIT" } });
})();
