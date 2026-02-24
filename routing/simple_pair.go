package routing

import (
	"context"
	"fmt"
	"strings"

	"gatewayd/core/config"
	"gatewayd/core/event"
	"gatewayd/endpoints/contract"
)

type SimplePairRouter struct {
	// Allowed sources (mcptt endpoints). In simple_pair we allow multiple MCPTT sources
	// so arbitration/preemption is meaningful across mcptt_a/mcptt_b/etc.
	allowedSources map[string]bool

	// Priority-ordered destination candidates
	destIDs []string

	// whether to choose first healthy dest
	failoverEnabled bool

	endpoints map[string]contract.Endpoint
}

func NewSimplePairRouter(cfg *config.Config, eps []contract.Endpoint) (*SimplePairRouter, error) {
	if cfg.Routing.SimplePair == nil {
		return nil, fmt.Errorf("simple_pair config missing")
	}

	sp := cfg.Routing.SimplePair
	destIDs := sp.DestinationsList()
	if len(destIDs) == 0 {
		return nil, fmt.Errorf("simple_pair requires destination or destinations[]")
	}

	r := &SimplePairRouter{
		allowedSources:  map[string]bool{},
		destIDs:         destIDs,
		failoverEnabled: cfg.Routing.Failover.Enabled,
		endpoints:       map[string]contract.Endpoint{},
	}

	for _, ep := range eps {
		r.endpoints[ep.ID()] = ep
	}

	// Validate destination IDs exist
	for _, id := range r.destIDs {
		if id == "" {
			return nil, fmt.Errorf("simple_pair destinations must not contain empty id")
		}
		if r.endpoints[id] == nil {
			return nil, fmt.Errorf("simple_pair destination endpoint not found: %s", id)
		}
	}

	// Determine allowed sources:
	// 1) Always include the configured source if present.
	// 2) Additionally include *all* endpoints of type "mcptt".
	//
	// This allows multiple MCPTT clients (mcptt_a, mcptt_b, ...) to compete and preempt,
	// while still keeping routing deterministic and safe.
	if sp.Source != "" {
		if r.endpoints[sp.Source] == nil {
			return nil, fmt.Errorf("simple_pair source endpoint not found: %s", sp.Source)
		}
		r.allowedSources[sp.Source] = true
	}

	for _, ep := range eps {
		if strings.EqualFold(ep.Type(), "mcptt") {
			r.allowedSources[ep.ID()] = true
		}
	}

	if len(r.allowedSources) == 0 {
		return nil, fmt.Errorf("simple_pair has no allowed sources (expected at least one mcptt endpoint)")
	}

	return r, nil
}

// SelectDestination is used by core for TX destination locking:
// - TXStart will call SelectDestination once and lock that ID for the session.
// - TXStop will use the locked ID (not selection).
func (r *SimplePairRouter) SelectDestination(ctx context.Context) (string, contract.Endpoint, error) {
	return r.selectDestination(ctx)
}

func (r *SimplePairRouter) isAllowedSource(interfaceID string) bool {
	return r.allowedSources[interfaceID]
}

// HandleWithDestination routes TXStart/TXStop to a specific destination ID WITHOUT re-selecting
// and WITHOUT performing health checks. This is required for destination locking across a TX session.
func (r *SimplePairRouter) HandleWithDestination(ctx context.Context, ev event.Event, destID string) (bool, string, string, error) {
	// Only route from allowed mcptt sources
	if !r.isAllowedSource(ev.InterfaceID) {
		return false, "", "", nil
	}

	if destID == "" {
		return false, "", "", fmt.Errorf("destination id must not be empty")
	}
	dest := r.endpoints[destID]
	if dest == nil {
		return false, "", destID, fmt.Errorf("destination endpoint not found: %s", destID)
	}

	switch ev.Type {
	case event.TXStart:
		if err := dest.PTTDown(ctx, map[string]any{
			"reason":       "simple_pair_tx_start",
			"source":       ev.InterfaceID,
			"event_type":   string(ev.Type),
			"event_ts_utc": ev.TS.Format("2006-01-02T15:04:05.999999999Z07:00"),
			"dest":         destID,
			"locked":       true,
		}); err != nil {
			return false, "ptt_down", destID, err
		}
		return true, "ptt_down", destID, nil

	case event.TXStop:
		if err := dest.PTTUp(ctx, map[string]any{
			"reason":       "simple_pair_tx_stop",
			"source":       ev.InterfaceID,
			"event_type":   string(ev.Type),
			"event_ts_utc": ev.TS.Format("2006-01-02T15:04:05.999999999Z07:00"),
			"dest":         destID,
			"locked":       true,
		}); err != nil {
			return false, "ptt_up", destID, err
		}
		return true, "ptt_up", destID, nil
	}

	return false, "", "", nil
}

// selectDestination chooses the destination endpoint deterministically.
// If failover is enabled, it returns the first Healthy endpoint in priority order.
// If failover is disabled, it returns the first configured destination.
// If failover enabled and none are healthy, it returns an error.
func (r *SimplePairRouter) selectDestination(ctx context.Context) (string, contract.Endpoint, error) {
	if len(r.destIDs) == 0 {
		return "", nil, fmt.Errorf("no simple_pair destinations configured")
	}

	// No failover: always choose first (deterministic).
	if !r.failoverEnabled {
		id := r.destIDs[0]
		return id, r.endpoints[id], nil
	}

	// Failover: choose first healthy destination in list.
	for _, id := range r.destIDs {
		ep := r.endpoints[id]
		if ep == nil {
			continue
		}

		st, err := ep.HealthCheck(ctx)
		if err != nil {
			// treat health check errors as not healthy
			continue
		}
		if st == contract.HealthHealthy {
			return id, ep, nil
		}
	}

	return "", nil, fmt.Errorf("no healthy destinations available")
}

// Handle returns (commandSent, commandName, destinationID, err).
func (r *SimplePairRouter) Handle(ctx context.Context, ev event.Event) (bool, string, string, error) {
	// Only route from allowed mcptt sources
	if !r.isAllowedSource(ev.InterfaceID) {
		return false, "", "", nil
	}

	destID, dest, err := r.selectDestination(ctx)
	if err != nil {
		switch ev.Type {
		case event.TXStart:
			return false, "ptt_down", "", err
		case event.TXStop:
			return false, "ptt_up", "", err
		default:
			return false, "", "", nil
		}
	}

	switch ev.Type {
	case event.TXStart:
		if err := dest.PTTDown(ctx, map[string]any{
			"reason":       "simple_pair_tx_start",
			"source":       ev.InterfaceID,
			"event_type":   string(ev.Type),
			"event_ts_utc": ev.TS.Format("2006-01-02T15:04:05.999999999Z07:00"),
			"dest":         destID,
			"locked":       false,
		}); err != nil {
			return false, "ptt_down", destID, err
		}
		return true, "ptt_down", destID, nil

	case event.TXStop:
		if err := dest.PTTUp(ctx, map[string]any{
			"reason":       "simple_pair_tx_stop",
			"source":       ev.InterfaceID,
			"event_type":   string(ev.Type),
			"event_ts_utc": ev.TS.Format("2006-01-02T15:04:05.999999999Z07:00"),
			"dest":         destID,
			"locked":       false,
		}); err != nil {
			return false, "ptt_up", destID, err
		}
		return true, "ptt_up", destID, nil
	}

	return false, "", "", nil
}
