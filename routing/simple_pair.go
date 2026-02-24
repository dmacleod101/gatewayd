package routing

import (
	"context"
	"fmt"

	"gatewayd/core/config"
	"gatewayd/core/event"
	"gatewayd/endpoints/contract"
)

type SimplePairRouter struct {
	sourceID string

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
		sourceID:        sp.Source,
		destIDs:         destIDs,
		failoverEnabled: cfg.Routing.Failover.Enabled,
		endpoints:       map[string]contract.Endpoint{},
	}

	for _, ep := range eps {
		r.endpoints[ep.ID()] = ep
	}

	if r.sourceID == "" {
		return nil, fmt.Errorf("simple_pair.source must not be empty")
	}
	if r.endpoints[r.sourceID] == nil {
		return nil, fmt.Errorf("simple_pair source endpoint not found: %s", r.sourceID)
	}

	// Validate that all destination IDs exist
	for _, id := range r.destIDs {
		if id == "" {
			return nil, fmt.Errorf("simple_pair destinations must not contain empty id")
		}
		if r.endpoints[id] == nil {
			return nil, fmt.Errorf("simple_pair destination endpoint not found: %s", id)
		}
	}

	return r, nil
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
	// Only route from the configured source
	if ev.InterfaceID != r.sourceID {
		return false, "", "", nil
	}

	destID, dest, err := r.selectDestination(ctx)
	if err != nil {
		// tell core what we tried to do (cmd name will be set below)
		// (core will log route_command_failed)
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
		}); err != nil {
			return false, "ptt_up", destID, err
		}
		return true, "ptt_up", destID, nil
	}

	return false, "", "", nil
}
