package gpio_stub

import (
	"context"
	"fmt"
	"sync/atomic"

	"gatewayd/endpoints/contract"
)

type Endpoint struct {
	id     string
	typ    string
	driver string
	cfg    map[string]any

	connected atomic.Bool
	sink      atomic.Value // func(contract.Event)
}

func New(id, typ, driver string, cfg map[string]any) (*Endpoint, error) {
	if id == "" {
		return nil, fmt.Errorf("id must not be empty")
	}
	if typ == "" {
		typ = "radio"
	}
	if driver == "" {
		driver = "gpio_stub"
	}
	return &Endpoint{id: id, typ: typ, driver: driver, cfg: cfg}, nil
}

func (e *Endpoint) ID() string     { return e.id }
func (e *Endpoint) Type() string   { return e.typ }
func (e *Endpoint) Driver() string { return e.driver }

func (e *Endpoint) Capabilities() contract.Capabilities {
	return contract.Capabilities{
		OutgoingFields: map[contract.FieldName]bool{
			contract.FieldEventType: true,
		},
		SupportsIndicator: false,
	}
}

func (e *Endpoint) SetEventSink(sink func(contract.Event)) {
	e.sink.Store(sink)
}

func (e *Endpoint) emit(ev contract.Event) {
	v := e.sink.Load()
	if v == nil {
		return
	}
	if fn, ok := v.(func(contract.Event)); ok && fn != nil {
		fn(ev)
	}
}

func (e *Endpoint) Connect(ctx context.Context) error {
	_ = ctx
	e.connected.Store(true)
	e.emit(contract.Event{EventType: "link_up"})
	return nil
}

func (e *Endpoint) Disconnect(ctx context.Context) error {
	_ = ctx
	e.connected.Store(false)
	e.emit(contract.Event{EventType: "link_down"})
	return nil
}

func (e *Endpoint) PTTDown(ctx context.Context, commandContext map[string]any) error {
	_ = ctx
	// Acknowledge deterministically so core logs show it happened.
	e.emit(contract.Event{
		EventType: "cmd_ack",
		Meta: map[string]any{
			"received_command": "ptt_down",
			"context":          commandContext,
		},
	})
	return nil
}

func (e *Endpoint) PTTUp(ctx context.Context, commandContext map[string]any) error {
	_ = ctx
	e.emit(contract.Event{
		EventType: "cmd_ack",
		Meta: map[string]any{
			"received_command": "ptt_up",
			"context":          commandContext,
		},
	})
	return nil
}

func (e *Endpoint) demoHealth() string {
	if e.cfg == nil {
		return ""
	}
	v, ok := e.cfg["demo_health"]
	if !ok {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return s
}

func (e *Endpoint) HealthCheck(ctx context.Context) (contract.HealthStatus, error) {
	_ = ctx

	// If we're not connected, treat as degraded (keeps behaviour consistent).
	if !e.connected.Load() {
		return contract.HealthDegraded, nil
	}

	// Deterministic demo override for failover testing.
	switch e.demoHealth() {
	case "down":
		return contract.HealthDown, nil
	case "degraded":
		return contract.HealthDegraded, nil
	default:
		return contract.HealthHealthy, nil
	}
}

func (e *Endpoint) SetIndicator(ctx context.Context, name string, state string) error {
	_ = ctx
	_ = name
	_ = state
	return contract.ErrNotSupported{What: "set_indicator"}
}
