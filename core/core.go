package core

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"time"

	"gatewayd/core/config"
	"gatewayd/core/event"
	"gatewayd/core/statemachine"
	"gatewayd/endpoints/contract"
	"gatewayd/routing"
)

type Logger interface {
	Info(msg string, fields map[string]any)
	Error(msg string, fields map[string]any)
}

type Core struct {
	cfg       *config.Config
	endpoints []contract.Endpoint

	req event.Requirements
	fsm statemachine.Context

	// Snapshot of FSM for lock-free reads (e.g., HTTP API)
	fsmSnap atomic.Value // stores statemachine.Context

	ring *RingBuffer
	log  Logger

	incoming chan event.Event

	// routers (only one active per mode for now)
	routerSimplePair *routing.SimplePairRouter
	routerRuleBased  *routing.RuleBasedRouter

	arb *Arbitrator

	// ---- TX destination locking (routing-mode aware) ----
	// When TX starts, we pick a destination once and keep it for the duration of the TX session.
	// TXStop will always use the same destination, even if health changes mid-session.
	txDestLockDestID  string
	txDestLockOwnerID string
	txDestLockSession string
	txDestLockedAtUTC time.Time
}

func New(c *config.Config, eps []contract.Endpoint, log Logger) *Core {
	req := event.Requirements{
		RequireSubscriberID: c.Routing.Features.Match.SubscriberID,
		RequireTalkgroupID:  c.Routing.Features.Match.TalkgroupID,
		RequireSessionID:    c.Routing.Mode == config.ModeMultiEndpointBridge,
		RequireOriginID:     c.Routing.Features.LoopPrevention && c.Routing.Mode == config.ModeMultiEndpointBridge,
	}

	g := &Core{
		cfg:       c,
		endpoints: eps,
		req:       req,
		fsm:       statemachine.New(),
		ring:      NewRingBuffer(200),
		log:       log,
		incoming:  make(chan event.Event, 1000),
		arb:       NewArbitrator(c.Routing.Arbitration),
	}

	// Seed snapshot immediately so API reads are valid even before first event
	g.fsmSnap.Store(g.fsm)

	// Mode-specific router wiring
	switch c.Routing.Mode {
	case config.ModeSimplePair:
		r, err := routing.NewSimplePairRouter(c, eps)
		if err != nil {
			panic(err)
		}
		g.routerSimplePair = r

	case config.ModeRuleBased:
		r, err := routing.NewRuleBasedRouter(c, eps)
		if err != nil {
			panic(err)
		}
		g.routerRuleBased = r
	}

	return g
}

// FSMSnapshot returns the most recent FSM context snapshot.
// Safe for concurrent access without locking the core loop.
func (g *Core) FSMSnapshot() statemachine.Context {
	v := g.fsmSnap.Load()
	if v == nil {
		return statemachine.New()
	}
	if st, ok := v.(statemachine.Context); ok {
		return st
	}
	return statemachine.New()
}

func (g *Core) IngestEndpointEvent(interfaceID string, ev contract.Event) error {
	e, err := event.FromEndpoint(interfaceID, ev)
	if err != nil {
		return err
	}
	select {
	case g.incoming <- e:
		return nil
	default:
		return fmt.Errorf("core incoming queue full")
	}
}

func (g *Core) forceReleaseTxLock(reason string) {
	if g.fsm.State != statemachine.TX {
		return
	}
	if g.fsm.TXOwner == "" {
		return
	}

	prevOwner := g.fsm.TXOwner
	g.fsm.State = statemachine.Idle
	g.fsm.TXOwner = ""
	g.fsm.UpdatedAt = time.Now().UTC()

	// Keep snapshot in sync
	g.fsmSnap.Store(g.fsm)

	// Clear destination lock as well (TX ended).
	g.clearTXDestinationLock("force_release:" + reason)

	g.log.Error("tx_lock_released", map[string]any{
		"reason":     reason,
		"prev_owner": prevOwner,
	})
}

func (g *Core) clearTXDestinationLock(reason string) {
	if g.txDestLockDestID == "" && g.txDestLockOwnerID == "" && g.txDestLockSession == "" {
		return
	}
	g.log.Info("tx_destination_unlock", map[string]any{
		"reason":     reason,
		"dest":       g.txDestLockDestID,
		"owner":      g.txDestLockOwnerID,
		"session_id": g.txDestLockSession,
		"locked_at":  g.txDestLockedAtUTC.Format(time.RFC3339Nano),
	})
	g.txDestLockDestID = ""
	g.txDestLockOwnerID = ""
	g.txDestLockSession = ""
	g.txDestLockedAtUTC = time.Time{}
}

func (g *Core) lockTXDestination(destID string, ownerID string, sessionID string, reason string) {
	g.txDestLockDestID = destID
	g.txDestLockOwnerID = ownerID
	g.txDestLockSession = sessionID
	g.txDestLockedAtUTC = time.Now().UTC()

	g.log.Info("tx_destination_lock", map[string]any{
		"reason":     reason,
		"dest":       destID,
		"owner":      ownerID,
		"session_id": sessionID,
		"locked_at":  g.txDestLockedAtUTC.Format(time.RFC3339Nano),
	})
}

func (g *Core) txSessionIDForLock(e event.Event) string {
	if e.SessionID != nil && *e.SessionID != "" {
		return *e.SessionID
	}
	return ""
}

func (g *Core) ensureTXDestinationLocked(ctx context.Context, e event.Event, prev statemachine.Context, reason string) (string, error) {
	owner := g.fsm.TXOwner
	if owner == "" {
		return "", fmt.Errorf("tx owner missing")
	}

	ownerChanged := prev.State != statemachine.TX || prev.TXOwner != g.fsm.TXOwner

	// If we already have a lock for this owner, keep it.
	if g.txDestLockDestID != "" && g.txDestLockOwnerID == owner && !ownerChanged {
		return g.txDestLockDestID, nil
	}

	var destID string
	var err error

	switch g.cfg.Routing.Mode {
	case config.ModeSimplePair:
		if g.routerSimplePair == nil {
			return "", fmt.Errorf("router not available (simple_pair)")
		}
		destID, _, err = g.routerSimplePair.SelectDestination(ctx)

	case config.ModeRuleBased:
		if g.routerRuleBased == nil {
			return "", fmt.Errorf("router not available (rule_based)")
		}
		destID, _, err = g.routerRuleBased.SelectDestination(ctx, e)

	default:
		return "", fmt.Errorf("tx destination locking not supported for mode: %s", g.cfg.Routing.Mode)
	}

	if err != nil {
		return "", err
	}

	g.lockTXDestination(destID, owner, g.txSessionIDForLock(e), reason)
	return destID, nil
}

// lockedDestinationForTXStop returns the locked destination for this TX session.
// If no lock exists, returns ("", false) so TXStop can be treated as stale.
func (g *Core) lockedDestinationForTXStop() (string, bool) {
	if g.txDestLockDestID == "" {
		return "", false
	}
	return g.txDestLockDestID, true
}

// emitSyntheticTxEvent writes a synthetic tx_start/tx_stop into the ring buffer so /api/io can reflect
// gateway-driven TX on endpoints that don't naturally emit tx_* events (e.g. GPIO PTT).
func (g *Core) emitSyntheticTxEvent(destID string, typ string, source string) {
	if destID == "" {
		return
	}
	g.ring.Add(map[string]any{
		"ts":           time.Now().UTC().Format(time.RFC3339Nano),
		"interface_id": destID,
		"event_type":   typ, // "tx_start" or "tx_stop"
		"origin_id":    "gatewayd",
		"meta": map[string]any{
			"synthetic": true,
			"source":    source,
		},
	})
}

func (g *Core) Run(ctx context.Context) error {
	// Attach event sinks (out-of-band interface_id) + connect endpoints
	for _, ep := range g.endpoints {
		if src, ok := ep.(contract.EventSource); ok {
			interfaceID := ep.ID()
			src.SetEventSink(func(ev contract.Event) {
				if err := g.IngestEndpointEvent(interfaceID, ev); err != nil {
					g.log.Error("event_ingest_failed", map[string]any{
						"interface_id": interfaceID,
						"error":        err.Error(),
					})
				}
			})
		}

		if err := ep.Connect(ctx); err != nil {
			return fmt.Errorf("endpoint connect failed (%s): %w", ep.ID(), err)
		}
	}

	// Optional TX lock timeout ticker
	var tick <-chan time.Time
	if g.arb.Timeout() > 0 {
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		tick = ticker.C
	}

	g.log.Info("core_run_start", map[string]any{
		"routing_mode":        g.cfg.Routing.Mode,
		"router_simple_pair":  g.routerSimplePair != nil,
		"router_rule_based":   g.routerRuleBased != nil,
		"tx_lock_timeout_ms":  g.cfg.Routing.Arbitration.TXLockTimeoutMS,
		"arb_policy":          g.cfg.Routing.Arbitration.Policy,
		"loop_prevention":     g.cfg.Routing.Features.LoopPrevention,
		"match_subscriber_id": g.cfg.Routing.Features.Match.SubscriberID,
		"match_talkgroup_id":  g.cfg.Routing.Features.Match.TalkgroupID,
	})

	for {
		select {
		case <-ctx.Done():
			for _, ep := range g.endpoints {
				_ = ep.Disconnect(context.Background())
			}
			g.log.Info("core_run_stop", map[string]any{"reason": "context_done"})
			return nil

		case <-tick:
			if g.arb.Timeout() > 0 && g.fsm.State == statemachine.TX {
				age := time.Since(g.fsm.UpdatedAt)
				if age > g.arb.Timeout() {
					g.forceReleaseTxLock("tx_lock_timeout")
				}
			}

		case e := <-g.incoming:
			// Validate required fields based on mode/feature flags
			if err := e.Validate(g.req); err != nil {
				g.log.Error("event_invalid", map[string]any{
					"error":        err.Error(),
					"interface_id": e.InterfaceID,
					"event_type":   string(e.Type),
				})
				continue
			}

			// Always log receipt first (makes ordering sane)
			g.log.Info("event_received", map[string]any{
				"interface_id": e.InterfaceID,
				"event_type":   string(e.Type),
			})

			// Arbitration gate
			decision, err := g.arb.DecideEvent(time.Now().UTC(), string(g.fsm.State), g.fsm.TXOwner, g.fsm.UpdatedAt, e)
			if err != nil {
				g.log.Error("arbitration_error", map[string]any{"error": err.Error()})
				continue
			}
			if !decision.Allow {
				g.log.Info("event_blocked", map[string]any{
					"interface_id": e.InterfaceID,
					"event_type":   string(e.Type),
					"reason":       decision.Reason,
					"tx_owner":     g.fsm.TXOwner,
				})
				continue
			}

			// Preempt ownership switch
			if decision.Preempt {
				old := g.fsm.TXOwner

				g.log.Info("tx_preempt", map[string]any{
					"reason":    decision.Reason,
					"old_owner": decision.OldOwner,
					"new_owner": decision.NewOwner,
				})

				g.fsm.State = statemachine.TX
				g.fsm.TXOwner = decision.NewOwner
				g.fsm.UpdatedAt = time.Now().UTC()

				// Keep snapshot in sync
				g.fsmSnap.Store(g.fsm)

				// Clear any previous destination lock.
				g.clearTXDestinationLock("preempt_owner_switch")

				g.log.Info("tx_owner_switched", map[string]any{
					"old_owner": old,
					"new_owner": g.fsm.TXOwner,
				})
			}

			// Ring buffer (for API)
			g.ring.Add(map[string]any{
				"ts":            e.TS.Format(time.RFC3339Nano),
				"interface_id":  e.InterfaceID,
				"event_type":    string(e.Type),
				"subscriber_id": e.SubscriberID,
				"talkgroup_id":  e.TalkgroupID,
				"session_id":    e.SessionID,
				"origin_id":     e.OriginID,
			})

			prev := g.fsm

			// ---- IMPORTANT: For TXStop, route BEFORE clearing lock / leaving TX ----
			if e.Type == event.TXStop {
				if destID, ok := g.lockedDestinationForTXStop(); ok {

					switch g.cfg.Routing.Mode {
					case config.ModeSimplePair:
						if g.routerSimplePair == nil {
							g.log.Error("route_router_missing", map[string]any{"mode": g.cfg.Routing.Mode})
							break
						}
						sent, cmd, usedDestID, rerr := g.routerSimplePair.HandleWithDestination(ctx, e, destID)
						if rerr != nil {
							g.log.Error("route_command_failed", map[string]any{
								"command": cmd,
								"error":   rerr.Error(),
								"source":  e.InterfaceID,
								"dest":    usedDestID,
							})
						} else if sent {
							g.log.Info("command_sent", map[string]any{
								"command": cmd,
								"source":  e.InterfaceID,
								"dest":    usedDestID,
							})
							// If TXStop results in ptt_up, reflect that as tx_stop on the dest.
							if cmd == "ptt_up" {
								g.emitSyntheticTxEvent(usedDestID, "tx_stop", e.InterfaceID)
							}
							g.clearTXDestinationLock("tx_stop_processed")
						} else {
							g.log.Error("tx_stop_not_sent", map[string]any{
								"source": e.InterfaceID,
								"dest":   usedDestID,
							})
						}

					case config.ModeRuleBased:
						if g.routerRuleBased == nil {
							g.log.Error("route_router_missing", map[string]any{"mode": g.cfg.Routing.Mode})
							break
						}
						sent, cmd, usedDestID, rerr := g.routerRuleBased.HandleWithDestination(ctx, e, destID)
						if rerr != nil {
							g.log.Error("route_command_failed", map[string]any{
								"command": cmd,
								"error":   rerr.Error(),
								"source":  e.InterfaceID,
								"dest":    usedDestID,
							})
						} else if sent {
							g.log.Info("command_sent", map[string]any{
								"command": cmd,
								"source":  e.InterfaceID,
								"dest":    usedDestID,
							})
							// If TXStop results in ptt_up, reflect that as tx_stop on the dest.
							if cmd == "ptt_up" {
								g.emitSyntheticTxEvent(usedDestID, "tx_stop", e.InterfaceID)
							}
							g.clearTXDestinationLock("tx_stop_processed")
						} else {
							g.log.Error("tx_stop_not_sent", map[string]any{
								"source": e.InterfaceID,
								"dest":   usedDestID,
							})
						}

					default:
						// No-op
					}

				} else {
					// Treat as stale TXStop (e.g. preempted owner arriving late)
					g.log.Info("tx_stop_ignored_no_lock", map[string]any{
						"source": e.InterfaceID,
						"reason": "no_tx_lock_present",
					})
				}
			}

			// FSM step (deterministic)
			next, serr := statemachine.Step(g.fsm, statemachine.Input{
				EventType:   e.Type,
				InterfaceID: e.InterfaceID,
			})
			if serr != nil {
				g.log.Error("fsm_error", map[string]any{"error": serr.Error()})
				continue
			}
			g.fsm = next

			// Keep snapshot in sync
			g.fsmSnap.Store(g.fsm)

			if prev.State != next.State || prev.TXOwner != next.TXOwner {
				g.log.Info("state_transition", map[string]any{
					"from":     prev.State,
					"to":       next.State,
					"tx_owner": next.TXOwner,
				})
			}

			// Mode-specific routing/action for non-TXStop cases
			if e.Type != event.TXStop {

				switch g.cfg.Routing.Mode {

				case config.ModeSimplePair:
					if g.routerSimplePair == nil {
						g.log.Error("route_router_missing", map[string]any{"mode": g.cfg.Routing.Mode})
						break
					}
					switch e.Type {
					case event.TXStart:
						lockedDest, lerr := g.ensureTXDestinationLocked(ctx, e, prev, "tx_start_lock")
						if lerr != nil {
							g.log.Error("tx_destination_lock_failed", map[string]any{
								"error":  lerr.Error(),
								"source": e.InterfaceID,
							})
							break
						}

						sent, cmd, destID, rerr := g.routerSimplePair.HandleWithDestination(ctx, e, lockedDest)
						if rerr != nil {
							g.log.Error("route_command_failed", map[string]any{
								"command": cmd,
								"error":   rerr.Error(),
								"source":  e.InterfaceID,
								"dest":    destID,
							})
						} else if sent {
							g.log.Info("command_sent", map[string]any{
								"command": cmd,
								"source":  e.InterfaceID,
								"dest":    destID,
							})
							// If TXStart results in ptt_down, reflect that as tx_start on the dest.
							if cmd == "ptt_down" {
								g.emitSyntheticTxEvent(destID, "tx_start", e.InterfaceID)
							}
						}

					default:
						sent, cmd, destID, rerr := g.routerSimplePair.Handle(ctx, e)
						if rerr != nil {
							g.log.Error("route_command_failed", map[string]any{
								"command": cmd,
								"error":   rerr.Error(),
								"source":  e.InterfaceID,
								"dest":    destID,
							})
						} else if sent {
							g.log.Info("command_sent", map[string]any{
								"command": cmd,
								"source":  e.InterfaceID,
								"dest":    destID,
							})
							// Some non-TXStart paths may still assert PTT.
							if cmd == "ptt_down" {
								g.emitSyntheticTxEvent(destID, "tx_start", e.InterfaceID)
							} else if cmd == "ptt_up" {
								g.emitSyntheticTxEvent(destID, "tx_stop", e.InterfaceID)
							}
						}
					}

				case config.ModeRuleBased:
					if g.routerRuleBased == nil {
						g.log.Error("route_router_missing", map[string]any{"mode": g.cfg.Routing.Mode})
						break
					}
					switch e.Type {
					case event.TXStart:
						lockedDest, lerr := g.ensureTXDestinationLocked(ctx, e, prev, "tx_start_lock")
						if lerr != nil {
							g.log.Error("tx_destination_lock_failed", map[string]any{
								"error":  lerr.Error(),
								"source": e.InterfaceID,
							})
							break
						}

						sent, cmd, destID, rerr := g.routerRuleBased.HandleWithDestination(ctx, e, lockedDest)
						if rerr != nil {
							g.log.Error("route_command_failed", map[string]any{
								"command": cmd,
								"error":   rerr.Error(),
								"source":  e.InterfaceID,
								"dest":    destID,
							})
						} else if sent {
							g.log.Info("command_sent", map[string]any{
								"command": cmd,
								"source":  e.InterfaceID,
								"dest":    destID,
							})
							if cmd == "ptt_down" {
								g.emitSyntheticTxEvent(destID, "tx_start", e.InterfaceID)
							}
						}

					default:
						sent, cmd, destID, rerr := g.routerRuleBased.Handle(ctx, e)
						if rerr != nil {
							g.log.Error("route_command_failed", map[string]any{
								"command": cmd,
								"error":   rerr.Error(),
								"source":  e.InterfaceID,
								"dest":    destID,
							})
						} else if sent {
							g.log.Info("command_sent", map[string]any{
								"command": cmd,
								"source":  e.InterfaceID,
								"dest":    destID,
							})
							if cmd == "ptt_down" {
								g.emitSyntheticTxEvent(destID, "tx_start", e.InterfaceID)
							} else if cmd == "ptt_up" {
								g.emitSyntheticTxEvent(destID, "tx_stop", e.InterfaceID)
							}
						}
					}

				default:
					// No-op
				}
			}

			// Defensive: if we left TX, clear any stale lock (should be cleared on successful TXStop)
			if prev.State == statemachine.TX && g.fsm.State != statemachine.TX {
				if g.txDestLockDestID != "" {
					g.clearTXDestinationLock("left_tx_state")
				}
			}
		}
	}
}

/* -------------------------
   Minimal JSON logger (v1)
-------------------------- */

type JSONLogger struct{}

func (l JSONLogger) Info(msg string, fields map[string]any)  { l.emit("info", msg, fields) }
func (l JSONLogger) Error(msg string, fields map[string]any) { l.emit("error", msg, fields) }

func (l JSONLogger) emit(level, msg string, fields map[string]any) {
	out := map[string]any{
		"ts":    time.Now().UTC().Format(time.RFC3339Nano),
		"level": level,
		"msg":   msg,
	}
	for k, v := range fields {
		out[k] = v
	}
	b, _ := json.Marshal(out)
	fmt.Println(string(b))
}
