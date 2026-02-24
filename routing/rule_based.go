package routing

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"gatewayd/core/config"
	"gatewayd/core/event"
	"gatewayd/endpoints/contract"
)

type RuleBasedRouter struct {
	cfg       *config.Config
	endpoints map[string]contract.Endpoint
}

func NewRuleBasedRouter(cfg *config.Config, eps []contract.Endpoint) (*RuleBasedRouter, error) {
	r := &RuleBasedRouter{
		cfg:       cfg,
		endpoints: map[string]contract.Endpoint{},
	}

	for _, ep := range eps {
		r.endpoints[ep.ID()] = ep
	}

	return r, nil
}

// SelectDestination chooses a destination for TXStart/TXStop based on the first matching rule.
// For non-TX events, it returns ("", nil, nil) to indicate no routing decision is required.
func (r *RuleBasedRouter) SelectDestination(ctx context.Context, ev event.Event) (string, contract.Endpoint, error) {
	// Only TXStart/TXStop are routable in the core today.
	switch ev.Type {
	case event.TXStart, event.TXStop:
		// continue
	default:
		return "", nil, nil
	}

	rule := r.matchRule(ev)
	if rule == nil {
		return "", nil, fmt.Errorf("no rule matched")
	}

	dests := r.sortedDestinations(rule)

	for _, d := range dests {
		ep := r.endpoints[d.InterfaceID]
		if ep == nil {
			continue
		}

		if rule.Match.RequireDestinationHealthy {
			st, err := ep.HealthCheck(ctx)
			if err != nil {
				continue
			}
			if st != contract.HealthHealthy {
				continue
			}
		}

		return d.InterfaceID, ep, nil
	}

	return "", nil, fmt.Errorf("no suitable destination found")
}

func (r *RuleBasedRouter) HandleWithDestination(ctx context.Context, ev event.Event, destID string) (bool, string, string, error) {
	// Only TXStart/TXStop produce commands.
	switch ev.Type {
	case event.TXStart, event.TXStop:
		// continue
	default:
		return false, "", "", nil
	}

	dest := r.endpoints[destID]
	if dest == nil {
		return false, "", destID, fmt.Errorf("destination not found")
	}

	switch ev.Type {
	case event.TXStart:
		if err := dest.PTTDown(ctx, map[string]any{
			"reason": "rule_based_tx_start",
			"source": ev.InterfaceID,
			"dest":   destID,
			"locked": true,
		}); err != nil {
			return false, "ptt_down", destID, err
		}
		return true, "ptt_down", destID, nil

	case event.TXStop:
		if err := dest.PTTUp(ctx, map[string]any{
			"reason": "rule_based_tx_stop",
			"source": ev.InterfaceID,
			"dest":   destID,
			"locked": true,
		}); err != nil {
			return false, "ptt_up", destID, err
		}
		return true, "ptt_up", destID, nil
	}

	return false, "", "", nil
}

func (r *RuleBasedRouter) Handle(ctx context.Context, ev event.Event) (bool, string, string, error) {
	// Only TXStart/TXStop produce commands.
	switch ev.Type {
	case event.TXStart, event.TXStop:
		// continue
	default:
		return false, "", "", nil
	}

	destID, dest, err := r.SelectDestination(ctx, ev)
	if err != nil {
		return false, "", "", err
	}
	if dest == nil || destID == "" {
		// Non-TX events should have already returned above,
		// but keep this deterministic and safe.
		return false, "", "", nil
	}

	switch ev.Type {
	case event.TXStart:
		if err := dest.PTTDown(ctx, map[string]any{
			"reason": "rule_based_tx_start",
			"source": ev.InterfaceID,
			"dest":   destID,
			"locked": false,
		}); err != nil {
			return false, "ptt_down", destID, err
		}
		return true, "ptt_down", destID, nil

	case event.TXStop:
		if err := dest.PTTUp(ctx, map[string]any{
			"reason": "rule_based_tx_stop",
			"source": ev.InterfaceID,
			"dest":   destID,
			"locked": false,
		}); err != nil {
			return false, "ptt_up", destID, err
		}
		return true, "ptt_up", destID, nil
	}

	return false, "", "", nil
}

func (r *RuleBasedRouter) matchRule(ev event.Event) *config.Rule {
	// IMPORTANT: iterate by index so we can return a stable pointer to the slice element.
	for i := range r.cfg.Routing.Rules {
		rule := &r.cfg.Routing.Rules[i]

		if !rule.Enabled {
			continue
		}
		if !r.matchRuleCondition(rule.Match, ev) {
			continue
		}
		return rule
	}
	return nil
}

func (r *RuleBasedRouter) matchRuleCondition(m config.RuleMatch, ev event.Event) bool {
	if len(m.FromInterfaceIDs) > 0 {
		found := false
		for _, id := range m.FromInterfaceIDs {
			if id == ev.InterfaceID {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}

	if len(m.EventType) > 0 {
		found := false
		for _, et := range m.EventType {
			if strings.EqualFold(et, string(ev.Type)) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}

	if m.SubscriberID != "" {
		if ev.SubscriberID == nil || *ev.SubscriberID != m.SubscriberID {
			return false
		}
	}

	if m.TalkgroupID != "" {
		if ev.TalkgroupID == nil || *ev.TalkgroupID != m.TalkgroupID {
			return false
		}
	}

	return true
}

func (r *RuleBasedRouter) sortedDestinations(rule *config.Rule) []config.Destination {
	dests := append([]config.Destination{}, rule.Action.Destinations...)

	sort.SliceStable(dests, func(i, j int) bool {
		pi := 999999
		pj := 999999

		if dests[i].Priority != nil {
			pi = *dests[i].Priority
		}
		if dests[j].Priority != nil {
			pj = *dests[j].Priority
		}

		if pi != pj {
			return pi < pj
		}

		// Stable sort keeps original order when equal.
		return false
	})

	return dests
}
