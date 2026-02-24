package mcptt_stub

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"gatewayd/endpoints/contract"
)

type Endpoint struct {
	id     string
	typ    string
	driver string
	cfg    map[string]any

	connected atomic.Bool
	sink      atomic.Value // stores func(contract.Event)
}

func New(id, typ, driver string, cfg map[string]any) (*Endpoint, error) {
	if id == "" {
		return nil, fmt.Errorf("id must not be empty")
	}
	if typ == "" {
		typ = "mcptt"
	}
	if driver == "" {
		driver = "mcptt_stub"
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
		SupportsIndicator: true,
	}
}

// OPTIONAL: allow core to attach an event sink.
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

func (e *Endpoint) demoRole() string {
	if e.cfg == nil {
		return ""
	}
	if v, ok := e.cfg["demo_role"]; ok {
		if s, ok2 := v.(string); ok2 {
			return s
		}
	}
	return ""
}

func (e *Endpoint) Connect(ctx context.Context) error {
	_ = ctx
	e.connected.Store(true)

	e.emit(contract.Event{EventType: "link_up"})

	role := e.demoRole()

	// Deterministic demo behavior
	go func() {
		switch role {
		case "contender":
			// IMPORTANT:
			// Owner reaches tx_start at ~1300ms (300+600+300+100).
			// So contender must attempt AFTER that to guarantee "owner wins first".
			time.Sleep(1500 * time.Millisecond)
			e.emit(contract.Event{EventType: "tx_start"})
			time.Sleep(300 * time.Millisecond)
			e.emit(contract.Event{EventType: "tx_stop"})
			return

		default:
			// owner/default sequence
			time.Sleep(300 * time.Millisecond)
			e.emit(contract.Event{EventType: "rx_start"})
			time.Sleep(600 * time.Millisecond)
			e.emit(contract.Event{EventType: "rx_stop"})

			time.Sleep(300 * time.Millisecond)
			e.emit(contract.Event{EventType: "local_ptt_down"})
			time.Sleep(100 * time.Millisecond)
			e.emit(contract.Event{EventType: "tx_start"})
			time.Sleep(800 * time.Millisecond)
			e.emit(contract.Event{EventType: "tx_stop"})
			time.Sleep(100 * time.Millisecond)
			e.emit(contract.Event{EventType: "local_ptt_up"})
			return
		}
	}()

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
	_ = commandContext
	return nil
}

func (e *Endpoint) PTTUp(ctx context.Context, commandContext map[string]any) error {
	_ = ctx
	_ = commandContext
	return nil
}

func (e *Endpoint) HealthCheck(ctx context.Context) (contract.HealthStatus, error) {
	_ = ctx
	if !e.connected.Load() {
		return contract.HealthDegraded, nil
	}
	return contract.HealthHealthy, nil
}

func (e *Endpoint) SetIndicator(ctx context.Context, name string, state string) error {
	_ = ctx
	_ = name
	_ = state
	return nil
}
