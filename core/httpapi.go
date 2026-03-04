package core

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

type httpAPI struct {
	core   *Core
	server *http.Server
}

func StartHTTPAPI(ctx context.Context, listenAddr string, g *Core, log Logger) *http.Server {
	mux := http.NewServeMux()

	writeJSON := func(w http.ResponseWriter, status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}

	// ----------------------------
	// UI (static)
	// ----------------------------
	// Serve /ui/* from /opt/gatewayd/ui (or override with GATEWAYD_UI_DIR)
	uiDir := strings.TrimSpace(os.Getenv("GATEWAYD_UI_DIR"))
	if uiDir == "" {
		uiDir = "/opt/gatewayd/ui"
	}
	uiDir = filepath.Clean(uiDir)

	// Redirect root -> /ui/
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		http.Redirect(w, r, "/ui/", http.StatusFound)
	})

	// Static file server under /ui/
	mux.Handle("/ui/", http.StripPrefix("/ui/", http.FileServer(http.Dir(uiDir))))

	// ----------------------------
	// Liveness
	// ----------------------------
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{
			"ok":  true,
			"ts":  time.Now().UTC().Format(time.RFC3339Nano),
			"svc": "gatewayd",
		})
	})

	// ----------------------------
	// gatewayd health (for UI convenience)
	// ----------------------------
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) {
		st := g.FSMSnapshot()
		writeJSON(w, 200, map[string]any{
			"ok": true,
			"runtime": map[string]any{
				"started": true,
				"now":     time.Now().UnixMilli(),
			},
			"state": map[string]any{
				"fsm_state":    st.State,
				"tx_owner":     st.TXOwner,
				"fsm_updated":  st.UpdatedAt.Format(time.RFC3339Nano),
				"service_name": "gatewayd",
			},
		})
	})

	// ----------------------------
	// mcptt-bridge health proxy
	// ----------------------------
	mux.HandleFunc("/api/mcptt/health", func(w http.ResponseWriter, r *http.Request) {
		bridgeURL := strings.TrimSpace(os.Getenv("MCPTT_BRIDGE_URL"))
		if bridgeURL == "" {
			bridgeURL = "http://127.0.0.1:5072"
		}
		target := strings.TrimRight(bridgeURL, "/") + "/api/health"

		client := &http.Client{Timeout: 2 * time.Second}
		resp, err := client.Get(target)
		if err != nil {
			writeJSON(w, 502, map[string]any{
				"ok":    false,
				"error": "bridge_unreachable",
				"meta": map[string]any{
					"target": target,
					"detail": err.Error(),
				},
			})
			return
		}
		defer resp.Body.Close()

		var payload any
		dec := json.NewDecoder(resp.Body)
		if err := dec.Decode(&payload); err != nil {
			writeJSON(w, 502, map[string]any{
				"ok":    false,
				"error": "bridge_invalid_json",
				"meta": map[string]any{
					"target": target,
					"detail": err.Error(),
				},
			})
			return
		}

		// Pass through the bridge payload, but keep a consistent outer shape if it isn't JSON object.
		writeJSON(w, resp.StatusCode, payload)
	})

	// ----------------------------
	// mcptt_a (session control proxy to mcptt-bridge)
	// ----------------------------
	proxyMCPTT := func(w http.ResponseWriter, r *http.Request, method string, path string) {
		bridgeURL := strings.TrimSpace(os.Getenv("MCPTT_BRIDGE_URL"))
		if bridgeURL == "" {
			bridgeURL = "http://127.0.0.1:5072"
		}
		target := strings.TrimRight(bridgeURL, "/") + path

		req, err := http.NewRequest(method, target, nil)
		if err != nil {
			writeJSON(w, 500, map[string]any{
				"ok":    false,
				"error": "proxy_request_build_failed",
				"meta":  map[string]any{"detail": err.Error()},
			})
			return
		}

		client := &http.Client{Timeout: 4 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			writeJSON(w, 502, map[string]any{
				"ok":    false,
				"error": "bridge_unreachable",
				"meta": map[string]any{
					"target": target,
					"detail": err.Error(),
				},
			})
			return
		}
		defer resp.Body.Close()

		// Try to decode JSON; if it isn't JSON, pass text through.
		var payload any
		dec := json.NewDecoder(resp.Body)
		if err := dec.Decode(&payload); err != nil {
			// fallback: plain text
			b, _ := io.ReadAll(resp.Body)
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(resp.StatusCode)
			_, _ = w.Write(b)
			return
		}

		writeJSON(w, resp.StatusCode, payload)
	}

	mux.HandleFunc("/api/endpoints/mcptt_a/session/start", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		proxyMCPTT(w, r, http.MethodPost, "/api/session/start")
	})

	mux.HandleFunc("/api/endpoints/mcptt_a/session/stop", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		proxyMCPTT(w, r, http.MethodPost, "/api/session/stop")
	})

	mux.HandleFunc("/api/endpoints/mcptt_a/ptt/down", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		proxyMCPTT(w, r, http.MethodPost, "/api/ptt/down")
	})

	mux.HandleFunc("/api/endpoints/mcptt_a/ptt/up", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		proxyMCPTT(w, r, http.MethodPost, "/api/ptt/up")
	})

	// ----------------------------
	// FSM state snapshot
	// ----------------------------
	mux.HandleFunc("/api/state", func(w http.ResponseWriter, r *http.Request) {
		st := g.FSMSnapshot()

		writeJSON(w, 200, map[string]any{
			"ts": time.Now().UTC().Format(time.RFC3339Nano),
			"fsm": map[string]any{
				"state":      st.State,
				"tx_owner":   st.TXOwner,
				"updated_at": st.UpdatedAt.Format(time.RFC3339Nano),
			},
		})
	})

	// ----------------------------
	// Ring buffer events
	// ----------------------------
	mux.HandleFunc("/api/events", func(w http.ResponseWriter, r *http.Request) {
		events := g.ring.Snapshot()

		// Optional ?limit=50
		limitStr := r.URL.Query().Get("limit")
		if limitStr != "" {
			if limit, err := strconv.Atoi(limitStr); err == nil && limit > 0 {
				if limit < len(events) {
					events = events[len(events)-limit:]
				}
			}
		}

		writeJSON(w, 200, map[string]any{
			"ts":     time.Now().UTC().Format(time.RFC3339Nano),
			"count":  len(events),
			"events": events,
		})
	})

	// ----------------------------
	// IO snapshot derived from ring buffer
	// ----------------------------
	mux.HandleFunc("/api/io", func(w http.ResponseWriter, r *http.Request) {
		// Optional ?limit=500 (default = all events in ring snapshot)
		events := g.ring.Snapshot()
		limitStr := r.URL.Query().Get("limit")
		if limitStr != "" {
			if limit, err := strconv.Atoi(limitStr); err == nil && limit > 0 {
				if limit < len(events) {
					events = events[len(events)-limit:]
				}
			}
		}

		// Avoid importing event package here by round-tripping via JSON.
		raw, err := json.Marshal(events)
		if err != nil {
			http.Error(w, "failed to marshal events", http.StatusInternalServerError)
			return
		}
		var arr []map[string]any
		if err := json.Unmarshal(raw, &arr); err != nil {
			http.Error(w, "failed to unmarshal events", http.StatusInternalServerError)
			return
		}

		type ifaceIO struct {
			InterfaceID string `json:"interface_id"`

			LastEventType string `json:"last_event_type,omitempty"`
			LastEventTs   string `json:"last_event_ts,omitempty"`

			LastRxType string `json:"last_rx_event_type,omitempty"`
			LastRxTs   string `json:"last_rx_ts,omitempty"`

			LastTxType string `json:"last_tx_event_type,omitempty"`
			LastTxTs   string `json:"last_tx_ts,omitempty"`

			LastPttType    string `json:"last_ptt_event_type,omitempty"`
			LastPttTs      string `json:"last_ptt_ts,omitempty"`
			PttAsserted    *bool  `json:"ptt_asserted,omitempty"`
			PttLastUpdated string `json:"ptt_updated_at,omitempty"`
		}

		isRx := func(t string) bool {
			t = strings.ToLower(t)
			return strings.Contains(t, "rx") ||
				strings.Contains(t, "floor") ||
				strings.Contains(t, "audio_rx") ||
				strings.Contains(t, "audio_in")
		}
		isTx := func(t string) bool {
			t = strings.ToLower(t)
			return strings.Contains(t, "tx") ||
				strings.Contains(t, "audio_tx") ||
				strings.Contains(t, "audio_out")
		}
		isPtt := func(t string) bool {
			t = strings.ToLower(t)
			return strings.Contains(t, "ptt")
		}
		pttValue := func(t string) (bool, bool) {
			tl := strings.ToLower(t)
			if strings.Contains(tl, "down") || strings.Contains(tl, "assert") || strings.Contains(tl, "start") {
				return true, true
			}
			if strings.Contains(tl, "up") || strings.Contains(tl, "release") || strings.Contains(tl, "stop") {
				return false, true
			}
			return false, false
		}

		byIface := map[string]*ifaceIO{}

		for _, e := range arr {
			iface, _ := e["interface_id"].(string)
			if iface == "" {
				continue
			}
			et, _ := e["event_type"].(string)
			ts, _ := e["ts"].(string)

			s := byIface[iface]
			if s == nil {
				s = &ifaceIO{InterfaceID: iface}
				byIface[iface] = s
			}

			s.LastEventType = et
			s.LastEventTs = ts

			if isRx(et) {
				s.LastRxType = et
				s.LastRxTs = ts
			}
			if isTx(et) {
				s.LastTxType = et
				s.LastTxTs = ts
			}
			if isPtt(et) {
				s.LastPttType = et
				s.LastPttTs = ts
				if v, ok := pttValue(et); ok {
					vv := v
					s.PttAsserted = &vv
					s.PttLastUpdated = ts
				}
			}
		}

		keys := make([]string, 0, len(byIface))
		for k := range byIface {
			keys = append(keys, k)
		}
		sort.Strings(keys)

		out := make([]*ifaceIO, 0, len(keys))
		for _, k := range keys {
			out = append(out, byIface[k])
		}

		writeJSON(w, 200, map[string]any{
			"ts":                time.Now().UTC().Format(time.RFC3339Nano),
			"events_considered": len(arr),
			"ifaces":            out,
		})
	})

	srv := &http.Server{
		Addr:              listenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		log.Info("http_api_start", map[string]any{
			"listen": listenAddr,
			"ui_dir": uiDir,
		})
		err := srv.ListenAndServe()
		if err != nil && err != http.ErrServerClosed {
			log.Error("http_api_failed", map[string]any{
				"listen": listenAddr,
				"error":  err.Error(),
			})
		}
	}()

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		log.Info("http_api_stop", map[string]any{"reason": "context_done"})
	}()

	return srv
}
