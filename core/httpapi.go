package core

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
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
