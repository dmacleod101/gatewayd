package config

import (
	"encoding/json"
	"fmt"
	"os"
)

type Config struct {
	Version   string     `json:"version"`
	Endpoints []Endpoint `json:"endpoints"`
	Routing   Routing    `json:"routing"`
	API       API        `json:"api"`
	Logging   Logging    `json:"logging"`
}

type Endpoint struct {
	ID      string         `json:"id"`
	Type    string         `json:"type"`
	Driver  string         `json:"driver"`
	Enabled bool           `json:"enabled"`
	Config  map[string]any `json:"config"`
}

type RoutingMode string

const (
	ModeSimplePair          RoutingMode = "simple_pair"
	ModeRuleBased           RoutingMode = "rule_based"
	ModeMultiEndpointBridge RoutingMode = "multi_endpoint_bridge"
)

type Routing struct {
	Mode        RoutingMode  `json:"mode"`
	Features    Features     `json:"features"`
	SimplePair  *SimplePair  `json:"simple_pair,omitempty"`
	Rules       []Rule       `json:"rules,omitempty"`
	Failover    Failover     `json:"failover"`
	Arbitration Arbitration  `json:"arbitration"`
}

type Features struct {
	Match          MatchFeatures `json:"match"`
	LoopPrevention bool          `json:"loop_prevention"`
}

type MatchFeatures struct {
	SubscriberID bool `json:"subscriber_id"`
	TalkgroupID  bool `json:"talkgroup_id"`
}

type SimplePair struct {
	Source string `json:"source"`

	// Backwards compatible single destination:
	Destination string `json:"destination,omitempty"`

	// Preferred (priority list):
	Destinations []string `json:"destinations,omitempty"`
}

// DestinationsList returns the effective destination priority list.
// - If Destinations[] is set, it is used.
// - Else if Destination is set, it becomes a 1-item list.
// - Else returns empty slice.
func (sp *SimplePair) DestinationsList() []string {
	if sp == nil {
		return nil
	}
	if len(sp.Destinations) > 0 {
		return sp.Destinations
	}
	if sp.Destination != "" {
		return []string{sp.Destination}
	}
	return nil
}

type Rule struct {
	ID      string     `json:"id"`
	Enabled bool       `json:"enabled"`
	Match   RuleMatch  `json:"match"`
	Action  RuleAction `json:"action"`
}

type RuleMatch struct {
	FromInterfaceIDs          []string `json:"from_interface_ids"`
	EventType                 []string `json:"event_type"`
	SubscriberID              string   `json:"subscriber_id"`
	TalkgroupID               string   `json:"talkgroup_id"`
	RequireDestinationHealthy bool     `json:"require_destination_healthy"`
}

type RuleAction struct {
	Destinations []Destination `json:"destinations"`
}

type Destination struct {
	InterfaceID string `json:"interface_id"`
	Priority    *int   `json:"priority,omitempty"`
}

type Failover struct {
	Enabled bool `json:"enabled"`
}

type ArbitrationPolicy string

const (
	ArbFirstWins       ArbitrationPolicy = "first_wins"
	ArbPriorityPreempt ArbitrationPolicy = "priority_preempt"
)

type Arbitration struct {
	Policy          ArbitrationPolicy `json:"policy"`
	Priorities      map[string]int    `json:"priorities"`
	TXLockTimeoutMS int               `json:"tx_lock_timeout_ms"`
}

type API struct {
	Listen string `json:"listen"`
}

type Logging struct {
	Level string `json:"level"`
	JSON  bool   `json:"json"`
}

func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse config json: %w", err)
	}

	if c.Version == "" {
		c.Version = "1"
	}
	if c.Logging.Level == "" {
		c.Logging.Level = "info"
	}

	return &c, nil
}
