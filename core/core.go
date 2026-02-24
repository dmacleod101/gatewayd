package core

import (
	"context"
	"encoding/json"
	"fmt"
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

	ring *RingBuffer
	log  Logger

	incoming chan event.Event

	// routers (only one active per mode for now)
	routerSimplePair *routing.SimplePairRouter

	arb *Arbitrator
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

	// Mode-specific router wiring
	if c.Routing.Mode == config.ModeSimplePair {
		r, err := routing.NewSimplePairRouter(c, eps)
		if err != nil {
			// Fail fast at startup. (We'll replace this with a clean startup error later.)
			panic(err)
		}
		g.routerSimplePair = r
	}

	return g
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

	g.log.Error("tx_lock_released", map[string]any{
		"reason":     reason,
		"prev_owner": prevOwner,
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
		"routing_mode": g.cfg.Routing.Mode,
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
			// deterministic lock timeout check
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

			// Apply arbitration gate (TXStart/TXStop only)
			decision, err := g.arb.DecideEvent(time.Now().UTC(), string(g.fsm.State), g.fsm.TXOwner, g.fsm.UpdatedAt, e)
			if err != nil {
				g.log.Error("arbitration_error", map[string]any{
					"error": err.Error(),
				})
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

			// IMPORTANT:
			// If arbitration decides this TXStart is a preemption, we must switch TX ownership
			// BEFORE we process the event through the FSM. Otherwise "tx_stop_not_owner" occurs.
			if decision.Preempt {
				old := g.fsm.TXOwner

				g.log.Info("tx_preempt", map[string]any{
					"reason":    decision.Reason,
					"old_owner": decision.OldOwner,
					"new_owner": decision.NewOwner,
				})

				// Apply ownership switch deterministically.
				g.fsm.State = statemachine.TX
				g.fsm.TXOwner = decision.NewOwner
				g.fsm.UpdatedAt = time.Now().UTC()

				g.log.Info("tx_owner_switched", map[string]any{
					"old_owner": old,
					"new_owner": g.fsm.TXOwner,
				})
			}

			// Add to ring buffer (for API later)
			g.ring.Add(map[string]any{
				"ts":            e.TS.Format(time.RFC3339Nano),
				"interface_id":  e.InterfaceID,
				"event_type":    string(e.Type),
				"subscriber_id": e.SubscriberID,
				"talkgroup_id":  e.TalkgroupID,
				"session_id":    e.SessionID,
				"origin_id":     e.OriginID,
			})

			// FSM step (deterministic)
			prev := g.fsm
			next, err := statemachine.Step(g.fsm, statemachine.Input{
				EventType:   e.Type,
				InterfaceID: e.InterfaceID,
			})
			if err != nil {
				g.log.Error("fsm_error", map[string]any{
					"error": err.Error(),
				})
				continue
			}
			g.fsm = next

			// Log state changes only (keeps logs lean)
			if prev.State != next.State || prev.TXOwner != next.TXOwner {
				g.log.Info("state_transition", map[string]any{
					"from":     prev.State,
					"to":       next.State,
					"tx_owner": next.TXOwner,
				})
			}

			// Log event receipt
			g.log.Info("event_received", map[string]any{
				"interface_id": e.InterfaceID,
				"event_type":   string(e.Type),
			})

			// Mode-specific routing/action
			if g.routerSimplePair != nil {
				sent, cmd, destID, err := g.routerSimplePair.Handle(ctx, e)
				if err != nil {
					g.log.Error("route_command_failed", map[string]any{
						"command": cmd,
						"error":   err.Error(),
						"source":  e.InterfaceID,
						"dest":    destID,
					})
				} else if sent {
					g.log.Info("command_sent", map[string]any{
						"command": cmd,
						"source":  e.InterfaceID,
						"dest":    destID,
					})
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
