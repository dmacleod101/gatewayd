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

	chip       string
	lineOffset int
	activeHigh bool
	pulseMS    int
	debounceMS int

	// ---- COR input (optional) ----
	corEnabled     bool
	corChip        string
	corLineOffset  int
	corActiveHigh  bool
	corDebounceMS  int
	corBias        string
	corEmitInitial bool
	corLine        *gpiocdev.Line
	corCancel      context.CancelFunc

	// COR state is kept separate from e.mu to avoid deadlocks on Close/Disconnect.
	corMu           sync.Mutex
	corLastChange   time.Time
	corLastAsserted bool
	corHasState     bool
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

	// ---- Output config (existing) ----
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

	// ---- COR config (optional) ----
	corLineOffset, corHasLine := getInt(e.cfg, "cor_line_offset")
	corActiveHigh, corHasAH := getBool(e.cfg, "cor_active_high")
	corChip, _ := getString(e.cfg, "cor_chip")
	corDebounceMS, _ := getInt(e.cfg, "cor_debounce_ms")
	corBias, _ := getString(e.cfg, "cor_bias")
	corEmitInitial, _ := getBool(e.cfg, "cor_emit_initial")

	if corDebounceMS < 0 {
		return fmt.Errorf("gpio_gpiod: cor_debounce_ms must be >= 0")
	}

	// defaults
	if strings.TrimSpace(corBias) == "" {
		corBias = "as_is"
	}

	if corHasLine || corHasAH {
		if !corHasLine || corLineOffset < 0 {
			return fmt.Errorf("gpio_gpiod: config.cor_line_offset is required and must be >= 0 when enabling COR")
		}
		if !corHasAH {
			return fmt.Errorf("gpio_gpiod: config.cor_active_high is required when enabling COR")
		}
		if strings.TrimSpace(corChip) == "" {
			corChip = chipNorm
		}
		e.corEnabled = true
		e.corChip = normalizeChip(corChip)
		e.corLineOffset = corLineOffset
		e.corActiveHigh = corActiveHigh
		e.corDebounceMS = corDebounceMS
		e.corBias = strings.ToLower(strings.TrimSpace(corBias))
		e.corEmitInitial = corEmitInitial
	} else {
		e.corEnabled = false
	}

	e.connected.Store(true)

	obsVal, obsAsserted, obsErr := e.readObservedLocked()
	fields := map[string]any{
		"interface_id": e.id,
		"chip":         e.chip,
		"line":         e.lineOffset,
		"active_high":  e.activeHigh,
	}
	if obsErr == nil {
		fields["observed_value"] = obsVal
		fields["observed_asserted"] = obsAsserted
	} else {
		fields["observed_error"] = obsErr.Error()
	}

	fields["cor_enabled"] = e.corEnabled
	if e.corEnabled {
		fields["cor_chip"] = e.corChip
		fields["cor_line"] = e.corLineOffset
		fields["cor_active_high"] = e.corActiveHigh
		fields["cor_debounce_ms"] = e.corDebounceMS
		fields["cor_bias"] = e.corBias
		fields["cor_emit_initial"] = e.corEmitInitial
	}

	e.logJSON("info", "connect_ok", fields)
	e.emit(contract.Event{EventType: "link_up"})

	if e.corEnabled {
		if err := e.startCORWatcherLocked(); err != nil {
			e.logJSON("error", "cor_watch_start_failed", map[string]any{
				"interface_id":     e.id,
				"cor_chip":         e.corChip,
				"cor_line":         e.corLineOffset,
				"cor_active_high":  e.corActiveHigh,
				"cor_debounce_ms":  e.corDebounceMS,
				"cor_bias":         e.corBias,
				"cor_emit_initial": e.corEmitInitial,
				"error":            err.Error(),
			})
			_ = e.disconnectLocked()
			return fmt.Errorf("gpio_gpiod: cor watch start failed: %w", err)
		}
	}

	return nil
}

func (e *Endpoint) Disconnect(ctx context.Context) error {
	_ = ctx

	e.mu.Lock()
	defer e.mu.Unlock()

	if !e.connected.Load() {
		return nil
	}

	if err := e.disconnectLocked(); err != nil {
		return err
	}

	e.logJSON("info", "disconnect_ok", map[string]any{
		"interface_id": e.id,
		"chip":         e.chip,
		"line":         e.lineOffset,
		"active_high":  e.activeHigh,
		"cor_enabled":  e.corEnabled,
	})
	e.emit(contract.Event{EventType: "link_down"})
	return nil
}

func (e *Endpoint) disconnectLocked() error {
	e.stopCORWatcherLocked()

	if e.pulseTimer != nil {
		e.pulseTimer.Stop()
		e.pulseTimer = nil
	}

	if e.line != nil {
		_ = e.line.Close()
		e.line = nil
	}

	e.connected.Store(false)
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

	fields := map[string]any{
		"interface_id": e.id,
		"chip":         e.chip,
		"line":         e.lineOffset,
		"active_high":  e.activeHigh,
		"pulse_ms":     e.pulseMS,
	}

	obsVal, obsAsserted, obsErr := e.readObservedLocked()
	if obsErr == nil {
		fields["observed_value"] = obsVal
		fields["observed_asserted"] = obsAsserted
	} else {
		fields["observed_error"] = obsErr.Error()
	}

	e.logJSON("info", "ptt_down_ok", fields)

	if e.pulseMS > 0 {
		d := time.Duration(e.pulseMS) * time.Millisecond
		e.pulseTimer = time.AfterFunc(d, func() {
			_ = e.PTTUp(context.Background(), map[string]any{"auto_pulse": true})
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

	fields := map[string]any{
		"interface_id": e.id,
		"chip":         e.chip,
		"line":         e.lineOffset,
		"active_high":  e.activeHigh,
	}

	obsVal, obsAsserted, obsErr := e.readObservedLocked()
	if obsErr == nil {
		fields["observed_value"] = obsVal
		fields["observed_asserted"] = obsAsserted
	} else {
		fields["observed_error"] = obsErr.Error()
	}

	e.logJSON("info", "ptt_up_ok", fields)
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

	if e.corEnabled && e.corLine != nil {
		_, cerr := e.corLine.Value()
		if cerr != nil {
			e.logJSON("error", "healthcheck_cor_failed", map[string]any{
				"interface_id":    e.id,
				"cor_chip":        e.corChip,
				"cor_line":        e.corLineOffset,
				"cor_active_high": e.corActiveHigh,
				"cor_bias":        e.corBias,
				"error":           cerr.Error(),
			})
			return contract.HealthDown, nil
		}
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
func (e *Endpoint) ReadAsserted(ctx context.Context) (bool, error) {
	_ = ctx
	e.mu.Lock()
	defer e.mu.Unlock()

	if !e.connected.Load() || e.line == nil {
		return false, fmt.Errorf("gpio_gpiod: not connected")
	}
	_, asserted, err := e.readObservedLocked()
	return asserted, err
}

func (e *Endpoint) readObservedLocked() (value int, asserted bool, err error) {
	if e.line == nil {
		return 0, false, fmt.Errorf("no line")
	}
	v, err := e.line.Value()
	if err != nil {
		return 0, false, err
	}
	if e.activeHigh {
		return v, v == 1, nil
	}
	return v, v == 0, nil
}

func (e *Endpoint) shouldDebounceLocked() bool {
	if e.debounceMS <= 0 || e.lastChange.IsZero() {
		return false
	}
	return time.Since(e.lastChange) < time.Duration(e.debounceMS)*time.Millisecond
}

// ---- COR watcher (gpiocdev v0.9.1 API compatible) ----

func (e *Endpoint) startCORWatcherLocked() error {
	e.stopCORWatcherLocked()

	cctx, cancel := context.WithCancel(context.Background())
	e.corCancel = cancel

	biasOpt, biasName, berr := corBiasOption(e.corBias)
	if berr != nil {
		cancel()
		e.corCancel = nil
		return berr
	}

	// Handler runs inside gpiocdev internals. Do NOT take e.mu in here.
	var lineLocal *gpiocdev.Line

	handler := func(_ gpiocdev.LineEvent) {
		select {
		case <-cctx.Done():
			return
		default:
		}

		ll := lineLocal
		if ll == nil {
			// Edge fired before assignment; ignore safely.
			return
		}

		// Read current level from the line (LineEvent in this API doesn't include Value).
		v, err := ll.Value()
		if err != nil {
			e.logJSON("error", "cor_read_failed", map[string]any{
				"interface_id":    e.id,
				"cor_chip":        e.corChip,
				"cor_line":        e.corLineOffset,
				"cor_active_high": e.corActiveHigh,
				"cor_bias":        biasName,
				"error":           err.Error(),
			})
			return
		}

		now := time.Now()
		asserted := e.corAssertedFromValue(v)

		e.corMu.Lock()
		defer e.corMu.Unlock()

		if e.corDebounceMS > 0 && !e.corLastChange.IsZero() {
			if now.Sub(e.corLastChange) < time.Duration(e.corDebounceMS)*time.Millisecond {
				return
			}
		}

		if e.corHasState && asserted == e.corLastAsserted {
			return
		}

		e.corHasState = true
		e.corLastAsserted = asserted
		e.corLastChange = now

		if asserted {
			e.emit(contract.Event{
				EventType: "rx_start",
				Meta: map[string]any{
					"source":          "cor",
					"cor_chip":        e.corChip,
					"cor_line":        e.corLineOffset,
					"cor_active_high": e.corActiveHigh,
					"cor_bias":        biasName,
					"cor_value":       v,
					"cor_asserted":    asserted,
					"cor_debounce_ms": e.corDebounceMS,
				},
			})
			e.logJSON("info", "cor_rx_start", map[string]any{
				"interface_id": e.id,
				"cor_chip":     e.corChip,
				"cor_line":     e.corLineOffset,
				"cor_bias":     biasName,
				"cor_value":    v,
			})
		} else {
			e.emit(contract.Event{
				EventType: "rx_stop",
				Meta: map[string]any{
					"source":          "cor",
					"cor_chip":        e.corChip,
					"cor_line":        e.corLineOffset,
					"cor_active_high": e.corActiveHigh,
					"cor_bias":        biasName,
					"cor_value":       v,
					"cor_asserted":    asserted,
					"cor_debounce_ms": e.corDebounceMS,
				},
			})
			e.logJSON("info", "cor_rx_stop", map[string]any{
				"interface_id": e.id,
				"cor_chip":     e.corChip,
				"cor_line":     e.corLineOffset,
				"cor_bias":     biasName,
				"cor_value":    v,
			})
		}
	}

	opts := []gpiocdev.LineReqOption{
		gpiocdev.AsInput,
		gpiocdev.WithBothEdges,
		gpiocdev.WithEventHandler(handler),
		gpiocdev.WithConsumer("gatewayd:" + e.id + ":cor"),
	}
	if biasOpt != nil {
		opts = append(opts, biasOpt)
	}

	line, err := gpiocdev.RequestLine(e.corChip, e.corLineOffset, opts...)
	if err != nil {
		cancel()
		e.corCancel = nil
		return fmt.Errorf("request cor line failed (chip=%s line=%d): %w", e.corChip, e.corLineOffset, err)
	}

	lineLocal = line
	e.corLine = line

	// Seed initial state (do NOT emit by default - avoids boot forcing RX).
	initVal, initErr := line.Value()
	if initErr == nil {
		asserted := e.corAssertedFromValue(initVal)

		e.corMu.Lock()
		e.corLastAsserted = asserted
		e.corHasState = true
		e.corLastChange = time.Time{}
		e.corMu.Unlock()

		e.logJSON("info", "cor_initial_state", map[string]any{
			"interface_id":     e.id,
			"cor_chip":         e.corChip,
			"cor_line":         e.corLineOffset,
			"cor_active_high":  e.corActiveHigh,
			"cor_bias":         biasName,
			"cor_value":        initVal,
			"cor_asserted":     asserted,
			"cor_debounce_ms":  e.corDebounceMS,
			"cor_emit_initial": e.corEmitInitial,
		})

		if asserted && e.corEmitInitial {
			e.emit(contract.Event{
				EventType: "rx_start",
				Meta: map[string]any{
					"source":          "cor",
					"initial":         true,
					"cor_chip":        e.corChip,
					"cor_line":        e.corLineOffset,
					"cor_active_high": e.corActiveHigh,
					"cor_bias":        biasName,
					"cor_value":       initVal,
					"cor_asserted":    asserted,
					"cor_debounce_ms": e.corDebounceMS,
				},
			})
		}
	} else {
		e.logJSON("error", "cor_initial_read_failed", map[string]any{
			"interface_id":    e.id,
			"cor_chip":        e.corChip,
			"cor_line":        e.corLineOffset,
			"cor_active_high": e.corActiveHigh,
			"cor_bias":        biasName,
			"error":           initErr.Error(),
		})
	}

	return nil
}

func (e *Endpoint) stopCORWatcherLocked() {
	if e.corCancel != nil {
		e.corCancel()
		e.corCancel = nil
	}
	if e.corLine != nil {
		_ = e.corLine.Close()
		e.corLine = nil
	}

	e.corMu.Lock()
	e.corHasState = false
	e.corLastAsserted = false
	e.corLastChange = time.Time{}
	e.corMu.Unlock()
}

func (e *Endpoint) corAssertedFromValue(v int) bool {
	if e.corActiveHigh {
		return v == 1
	}
	return v == 0
}

func corBiasOption(raw string) (gpiocdev.LineReqOption, string, error) {
	s := strings.ToLower(strings.TrimSpace(raw))
	switch s {
	case "", "as_is", "asis", "as-is":
		return gpiocdev.WithBiasAsIs, "as_is", nil
	case "disabled", "disable", "none", "off":
		return gpiocdev.WithBiasDisabled, "disabled", nil
	case "pull_up", "pullup", "up":
		return gpiocdev.WithPullUp, "pull_up", nil
	case "pull_down", "pulldown", "down":
		return gpiocdev.WithPullDown, "pull_down", nil
	default:
		return nil, s, fmt.Errorf("gpio_gpiod: invalid cor_bias %q (use pull_up, pull_down, disabled, as_is)", raw)
	}
}

// ---- helpers ----

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
