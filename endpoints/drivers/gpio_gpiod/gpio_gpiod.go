package gpio_gpiod

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gatewayd/endpoints/contract"

	"github.com/warthog618/go-gpiocdev"
)

type Endpoint struct {
	id     string
	typ    string
	driver string
	cfg    map[string]any

	connected atomic.Bool
	sink      atomic.Value // func(contract.Event)

	mu         sync.Mutex
	line       *gpiocdev.Line
	lastChange time.Time
	pulseTimer *time.Timer

	// Parsed GPIO params (cached after Connect())
	chip       string
	lineOffset int
	activeHigh bool
	pulseMS    int
	debounceMS int
}

func New(id, typ, driver string, cfg map[string]any) (*Endpoint, error) {
	if id == "" {
		return nil, fmt.Errorf("id must not be empty")
	}
	if typ == "" {
		typ = "gpio"
	}
	if driver == "" {
		driver = "gpio_gpiod"
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

	e.mu.Lock()
	defer e.mu.Unlock()

	if e.connected.Load() {
		return nil
	}

	chip, ok := getString(e.cfg, "chip")
	if !ok || strings.TrimSpace(chip) == "" {
		return fmt.Errorf("gpio_gpiod: config.chip is required")
	}
	lineOffset, ok := getInt(e.cfg, "line_offset")
	if !ok || lineOffset < 0 {
		return fmt.Errorf("gpio_gpiod: config.line_offset is required and must be >= 0")
	}
	activeHigh, ok := getBool(e.cfg, "active_high")
	if !ok {
		return fmt.Errorf("gpio_gpiod: config.active_high is required")
	}
	pulseMS, _ := getInt(e.cfg, "pulse_ms")
	debounceMS, _ := getInt(e.cfg, "debounce_ms")
	if pulseMS < 0 || debounceMS < 0 {
		return fmt.Errorf("gpio_gpiod: pulse_ms and debounce_ms must be >= 0")
	}

	chipNorm := normalizeChip(chip)
	inactive := inactiveValue(activeHigh)

	line, err := gpiocdev.RequestLine(
		chipNorm,
		lineOffset,
		gpiocdev.AsOutput(inactive),
		gpiocdev.WithConsumer("gatewayd:"+e.id),
	)
	if err != nil {
		e.logJSON("error", "connect_failed", map[string]any{
			"interface_id": e.id,
			"chip":         chipNorm,
			"line":         lineOffset,
			"active_high":  activeHigh,
			"error":        err.Error(),
		})
		return fmt.Errorf("gpio_gpiod: request line failed (chip=%s line=%d): %w", chipNorm, lineOffset, err)
	}

	e.chip = chipNorm
	e.lineOffset = lineOffset
	e.activeHigh = activeHigh
	e.pulseMS = pulseMS
	e.debounceMS = debounceMS
	e.line = line
	e.lastChange = time.Time{}

	e.connected.Store(true)

	e.logJSON("info", "connect_ok", map[string]any{
		"interface_id": e.id,
		"chip":         e.chip,
		"line":         e.lineOffset,
		"active_high":  e.activeHigh,
	})

	e.emit(contract.Event{EventType: "link_up"})
	return nil
}

func (e *Endpoint) Disconnect(ctx context.Context) error {
	_ = ctx

	e.mu.Lock()
	defer e.mu.Unlock()

	if !e.connected.Load() {
		return nil
	}

	if e.pulseTimer != nil {
		e.pulseTimer.Stop()
		e.pulseTimer = nil
	}

	if e.line != nil {
		_ = e.line.Close()
		e.line = nil
	}

	e.connected.Store(false)

	e.logJSON("info", "disconnect_ok", map[string]any{
		"interface_id": e.id,
		"chip":         e.chip,
		"line":         e.lineOffset,
		"active_high":  e.activeHigh,
	})

	e.emit(contract.Event{EventType: "link_down"})
	return nil
}

func (e *Endpoint) PTTDown(ctx context.Context, commandContext map[string]any) error {
	_ = ctx

	e.mu.Lock()
	defer e.mu.Unlock()

	e.emit(contract.Event{
		EventType: "cmd_ack",
		Meta: map[string]any{
			"received_command": "ptt_down",
			"context":          commandContext,
		},
	})

	if !e.connected.Load() || e.line == nil {
		return fmt.Errorf("gpio_gpiod: not connected")
	}

	if e.shouldDebounceLocked() {
		e.logJSON("info", "ptt_down_debounced", map[string]any{
			"interface_id": e.id,
			"chip":         e.chip,
			"line":         e.lineOffset,
			"active_high":  e.activeHigh,
		})
		return nil
	}

	if e.pulseTimer != nil {
		e.pulseTimer.Stop()
		e.pulseTimer = nil
	}

	if err := e.line.SetValue(activeValue(e.activeHigh)); err != nil {
		e.logJSON("error", "ptt_down_failed", map[string]any{
			"interface_id": e.id,
			"chip":         e.chip,
			"line":         e.lineOffset,
			"active_high":  e.activeHigh,
			"error":        err.Error(),
		})
		return fmt.Errorf("gpio_gpiod: ptt_down SetValue failed: %w", err)
	}

	e.lastChange = time.Now()

	e.logJSON("info", "ptt_down_ok", map[string]any{
		"interface_id": e.id,
		"chip":         e.chip,
		"line":         e.lineOffset,
		"active_high":  e.activeHigh,
		"pulse_ms":     e.pulseMS,
	})

	if e.pulseMS > 0 {
		d := time.Duration(e.pulseMS) * time.Millisecond
		e.pulseTimer = time.AfterFunc(d, func() {
			_ = e.PTTUp(context.Background(), map[string]any{
				"auto_pulse": true,
			})
		})
	}

	return nil
}

func (e *Endpoint) PTTUp(ctx context.Context, commandContext map[string]any) error {
	_ = ctx

	e.mu.Lock()
	defer e.mu.Unlock()

	e.emit(contract.Event{
		EventType: "cmd_ack",
		Meta: map[string]any{
			"received_command": "ptt_up",
			"context":          commandContext,
		},
	})

	if !e.connected.Load() || e.line == nil {
		return fmt.Errorf("gpio_gpiod: not connected")
	}

	if e.shouldDebounceLocked() {
		e.logJSON("info", "ptt_up_debounced", map[string]any{
			"interface_id": e.id,
			"chip":         e.chip,
			"line":         e.lineOffset,
			"active_high":  e.activeHigh,
		})
		return nil
	}

	if e.pulseTimer != nil {
		e.pulseTimer.Stop()
		e.pulseTimer = nil
	}

	if err := e.line.SetValue(inactiveValue(e.activeHigh)); err != nil {
		e.logJSON("error", "ptt_up_failed", map[string]any{
			"interface_id": e.id,
			"chip":         e.chip,
			"line":         e.lineOffset,
			"active_high":  e.activeHigh,
			"error":        err.Error(),
		})
		return fmt.Errorf("gpio_gpiod: ptt_up SetValue failed: %w", err)
	}

	e.lastChange = time.Now()

	e.logJSON("info", "ptt_up_ok", map[string]any{
		"interface_id": e.id,
		"chip":         e.chip,
		"line":         e.lineOffset,
		"active_high":  e.activeHigh,
	})

	return nil
}

func (e *Endpoint) HealthCheck(ctx context.Context) (contract.HealthStatus, error) {
	_ = ctx

	e.mu.Lock()
	defer e.mu.Unlock()

	if !e.connected.Load() || e.line == nil {
		return contract.HealthDegraded, nil
	}

	_, err := e.line.Value()
	if err != nil {
		e.logJSON("error", "healthcheck_failed", map[string]any{
			"interface_id": e.id,
			"chip":         e.chip,
			"line":         e.lineOffset,
			"active_high":  e.activeHigh,
			"error":        err.Error(),
		})
		return contract.HealthDown, nil
	}

	return contract.HealthHealthy, nil
}

func (e *Endpoint) SetIndicator(ctx context.Context, name string, state string) error {
	_ = ctx
	_ = name
	_ = state
	return contract.ErrNotSupported{What: "set_indicator"}
}

// ReadAsserted is a helper for tests/diagnostics (not part of contract.Endpoint).
// It returns true if the line is currently in the "asserted" (PTT active) state.
func (e *Endpoint) ReadAsserted(ctx context.Context) (bool, error) {
	_ = ctx

	e.mu.Lock()
	defer e.mu.Unlock()

	if !e.connected.Load() || e.line == nil {
		return false, fmt.Errorf("gpio_gpiod: not connected")
	}

	v, err := e.line.Value()
	if err != nil {
		return false, err
	}

	if e.activeHigh {
		return v == 1, nil
	}
	return v == 0, nil
}

// --- helpers ---

func (e *Endpoint) shouldDebounceLocked() bool {
	if e.debounceMS <= 0 || e.lastChange.IsZero() {
		return false
	}
	return time.Since(e.lastChange) < time.Duration(e.debounceMS)*time.Millisecond
}

func activeValue(activeHigh bool) int {
	if activeHigh {
		return 1
	}
	return 0
}

func inactiveValue(activeHigh bool) int {
	if activeHigh {
		return 0
	}
	return 1
}

func normalizeChip(chip string) string {
	c := strings.TrimSpace(chip)
	if c == "" {
		return c
	}
	if strings.HasPrefix(c, "/dev/") {
		return c
	}
	c = strings.TrimPrefix(c, "dev/")
	c = strings.TrimPrefix(c, "/")
	if strings.HasPrefix(c, "gpiochip") {
		return c
	}
	return chip
}

func getString(m map[string]any, k string) (string, bool) {
	if m == nil {
		return "", false
	}
	v, ok := m[k]
	if !ok || v == nil {
		return "", false
	}
	if s, ok := v.(string); ok {
		return s, true
	}
	return fmt.Sprintf("%v", v), true
}

func getBool(m map[string]any, k string) (bool, bool) {
	if m == nil {
		return false, false
	}
	v, ok := m[k]
	if !ok || v == nil {
		return false, false
	}
	if b, ok := v.(bool); ok {
		return b, true
	}
	if s, ok := v.(string); ok {
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "true", "1", "yes", "y":
			return true, true
		case "false", "0", "no", "n":
			return false, true
		}
	}
	return false, false
}

func getInt(m map[string]any, k string) (int, bool) {
	if m == nil {
		return 0, false
	}
	v, ok := m[k]
	if !ok || v == nil {
		return 0, false
	}
	switch t := v.(type) {
	case int:
		return t, true
	case int32:
		return int(t), true
	case int64:
		return int(t), true
	case float64:
		return int(t), true
	case float32:
		return int(t), true
	case string:
		var i int
		_, err := fmt.Sscanf(strings.TrimSpace(t), "%d", &i)
		if err == nil {
			return i, true
		}
	}
	return 0, false
}

func (e *Endpoint) logJSON(level string, command string, fields map[string]any) {
	if fields == nil {
		fields = map[string]any{}
	}
	fields["ts"] = time.Now().UTC().Format(time.RFC3339Nano)
	fields["level"] = level
	fields["command"] = command

	if _, ok := fields["interface_id"]; !ok {
		fields["interface_id"] = e.id
	}
	if _, ok := fields["chip"]; !ok {
		fields["chip"] = e.chip
	}
	if _, ok := fields["line"]; !ok {
		fields["line"] = e.lineOffset
	}
	if _, ok := fields["active_high"]; !ok {
		fields["active_high"] = e.activeHigh
	}

	b, _ := json.Marshal(fields)
	log.Printf("%s\n", string(b))
}
