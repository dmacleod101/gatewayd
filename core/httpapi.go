package core

import (
	"context"
	"encoding/json"
	"net/http"
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

	// ----------------------------
	// Liveness
	// ----------------------------
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":  true,
			"ts":  time.Now().UTC().Format(time.RFC3339Nano),
			"svc": "gatewayd",
		})
	})

	// ----------------------------
	// FSM state snapshot
	// ----------------------------
	mux.HandleFunc("/api/state", func(w http.ResponseWriter, r *http.Request) {
		st := g.FSMSnapshot()

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
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

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
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
		// This keeps it resilient to event struct changes without needing module path knowledge.
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

		// Event classifiers (tolerant). We can tighten once we see your actual event_type vocabulary.
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
			// returns (value, ok)
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

		// Treat later items as “newer”.
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

			// last event of any type
			s.LastEventType = et
			s.LastEventTs = ts

			// rx/tx/ptt buckets
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

		// stable output ordering
		keys := make([]string, 0, len(byIface))
		for k := range byIface {
			keys = append(keys, k)
		}
		sort.Strings(keys)

		out := make([]*ifaceIO, 0, len(keys))
		for _, k := range keys {
			out = append(out, byIface[k])
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
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
