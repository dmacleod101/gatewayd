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
  let state = {
    connected: false,
    ptt: "idle", // idle|pending|tx|rx
    floorOwner: null
  };

  function now() { return Date.now(); }

  function log(msg) {
    try { console.log(`[MCPTT] ${msg}`); } catch {}
  }

  function reportState(patch) {
    state = { ...state, ...patch };
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
        "authorization": "Bearer " + subscriberBearer,
        "content-type": "application/json"
      }
    });

    if (!resp.ok) {
      const text = await resp.text().catch(() => "");
      throw new Error(`VoIP token fetch failed: HTTP ${resp.status} ${resp.statusText} ${text}`.trim());
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
      reportState({ connected: true });
      emitEvent("link_up");
    });

    call.on(window.SWSIPVOIP2.Call.Event.FAILED, () => {
      log("Call FAILED");
      reportState({ connected: false });
      resetFloorUi();
      emitEvent("link_down", { meta: { reason: "FAILED" } });
      call = null;
      ua = null;
    });

    call.on(window.SWSIPVOIP2.Call.Event.STOPPED, () => {
      log("Call STOPPED");
      reportState({ connected: false });
      resetFloorUi();
      emitEvent("link_down", { meta: { reason: "STOPPED" } });
      call = null;
      ua = null;
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
    ua = null;
    reportState({ connected: false });
    resetFloorUi();
    emitEvent("link_down", { meta: { reason: "STOP" } });
  }

  function pttDown() {
    if (!call) throw new Error("Cannot PTT: not connected");
    isPttHeld = true;
    lastFloorRequestAt = now();
    showPending();
    emitEvent("local_ptt_down");
    log("PTT down -> requestFloor()");
    call.requestFloor();
  }

  function pttUp() {
    if (!call) throw new Error("Cannot release PTT: not connected");
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

  // Initial state/event
  reportState({ connected: false, ptt: "idle", floorOwner: null });
  emitEvent("link_down", { meta: { reason: "INIT" } });
})();
