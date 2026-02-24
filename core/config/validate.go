package config

import (
	"fmt"
)

func ValidateConfig(c *Config) error {
	if c == nil {
		return fmt.Errorf("config is nil")
	}

	// endpoints must exist
	if len(c.Endpoints) == 0 {
		return fmt.Errorf("config.endpoints must not be empty")
	}

	// build a quick lookup for enabled endpoints
	enabled := map[string]bool{}
	for _, ep := range c.Endpoints {
		if ep.ID == "" {
			return fmt.Errorf("endpoint.id must not be empty")
		}
		if enabled[ep.ID] {
			return fmt.Errorf("duplicate endpoint id: %s", ep.ID)
		}
		if ep.Enabled {
			enabled[ep.ID] = true
		} else {
			enabled[ep.ID] = false
		}
	}

	// routing mode required
	if c.Routing.Mode == "" {
		return fmt.Errorf("routing.mode must not be empty")
	}

	switch c.Routing.Mode {
	case ModeSimplePair:
		if c.Routing.SimplePair == nil {
			return fmt.Errorf("routing.simple_pair must be provided when mode=simple_pair")
		}

		sp := c.Routing.SimplePair
		if sp.Source == "" {
			return fmt.Errorf("routing.simple_pair.source must not be empty")
		}
		if !enabled[sp.Source] {
			return fmt.Errorf("routing.simple_pair.source unknown endpoint id: %s", sp.Source)
		}

		dests := sp.DestinationsList()
		if len(dests) == 0 {
			return fmt.Errorf("routing.simple_pair requires destination (string) or destinations (array)")
		}

		for _, d := range dests {
			if d == "" {
				return fmt.Errorf("routing.simple_pair destinations must not contain empty id")
			}
			if !enabled[d] {
				return fmt.Errorf("routing.simple_pair.destination unknown endpoint id: %s", d)
			}
		}

	case ModeRuleBased:
		// Minimal validation for now; rules engine will be expanded later.
		// Still ensure endpoints referenced by rules exist.
		for _, r := range c.Routing.Rules {
			if r.ID == "" {
				return fmt.Errorf("routing.rules[].id must not be empty")
			}
			for _, src := range r.Match.FromInterfaceIDs {
				if src == "" {
					return fmt.Errorf("routing.rules[%s].match.from_interface_ids contains empty id", r.ID)
				}
				if _, ok := enabled[src]; !ok {
					return fmt.Errorf("routing.rules[%s].match.from_interface_ids unknown endpoint id: %s", r.ID, src)
				}
			}
			for _, dst := range r.Action.Destinations {
				if dst.InterfaceID == "" {
					return fmt.Errorf("routing.rules[%s].action.destinations[].interface_id must not be empty", r.ID)
				}
				if _, ok := enabled[dst.InterfaceID]; !ok {
					return fmt.Errorf("routing.rules[%s].action.destinations unknown endpoint id: %s", r.ID, dst.InterfaceID)
				}
			}
		}

	case ModeMultiEndpointBridge:
		// Placeholder validation; we’ll enforce session/origin requirements via required-fields matrix.
		// Still ensure endpoints exist.
		// (No extra config fields yet in v1.)
		_ = enabled

	default:
		return fmt.Errorf("unsupported routing.mode: %s", c.Routing.Mode)
	}

	// api listen optional (defaults handled elsewhere)
	// logging optional (defaults handled elsewhere)

	return nil
}
