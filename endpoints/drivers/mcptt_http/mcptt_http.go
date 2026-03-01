package mcptt_http

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gatewayd/endpoints/contract"
)

type cfg struct {
	BaseURL string

	// Endpoints on the MCPTT bridge/core
	EventsPath string
	PTTDownPath string
	PTTUpPath   string
	HealthPath  string

	// Polling + reconnect
	PollInterval       time.Duration
	RequestTimeout     time.Duration
	ConnectTimeout     time.Duration
	BackoffInitial     time.Duration
	BackoffMax         time.Duration
	DegradedAfterFails int
	DownAfterFails     int

	// Dedupe
	DedupeTTL time.Duration
	DedupeMax int

	// Optional auth header
	AuthHeader string
	AuthToken  string
}

type Endpoint struct {
	id     string
	typ    string
	driver string
	rawCfg map[string]any

	connected atomic.Bool
	sink      atomic.Value // func(contract.Event)

	httpc *http.Client

	mu     sync.Mutex
	cancel context.CancelFunc
	runWG  sync.WaitGroup

	conf cfg

	// polling cursor/state (opaque)
	cursor string

	// failure tracking
	consecutiveFails int
	lastState string // "up"|"degraded"|"down"

	// dedupe store
	dedupe *dedupeStore
}

func New(id, typ, driver string, raw map[string]any) (*Endpoint, error) {
	if id == "" {
		return nil, fmt.Errorf("id must not be empty")
	}
	if typ == "" {
		typ = "mcptt"
	}
	if driver == "" {
		driver = "mcptt_http"
	}

	c, err := parseCfg(raw)
	if err != nil {
		return nil, err
	}

	// base client; timeouts applied per-request via context as well
	httpc := &http.Client{
		Timeout: c.RequestTimeout,
	}

	e := &Endpoint{
		id:     id,
		typ:    typ,
		driver: driver,
		rawCfg: raw,
		httpc:  httpc,
		conf:   c,
		dedupe: newDedupeStore(c.DedupeMax, c.DedupeTTL),
	}

	return e, nil
}

func (e *Endpoint) ID() string     { return e.id }
func (e *Endpoint) Type() string   { return e.typ }
func (e *Endpoint) Driver() string { return e.driver }

func (e *Endpoint) Capabilities() contract.Capabilities {
	return contract.Capabilities{
		OutgoingFields: map[contract.FieldName]bool{
			contract.FieldEventType:    true,
			contract.FieldSubscriberID: true,
			contract.FieldTalkgroupID:  true,
			contract.FieldSessionID:    true,
			contract.FieldOriginID:     true,
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

func (e *Endpoint) setStateUp() {
	if e.lastState == "up" {
		return
	}
	e.lastState = "up"
	e.connected.Store(true)
	e.emit(contract.Event{EventType: "link_up"})
}

func (e *Endpoint) setStateDegraded() {
	if e.lastState == "degraded" {
		return
	}
	e.lastState = "degraded"
	e.connected.Store(true)
	e.emit(contract.Event{EventType: "link_degraded"})
}

func (e *Endpoint) setStateDown() {
	if e.lastState == "down" {
		return
	}
	e.lastState = "down"
	e.connected.Store(false)
	e.emit(contract.Event{EventType: "link_down"})
}

func (e *Endpoint) Connect(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	// already running?
	if e.cancel != nil {
		return nil
	}

	// quick initial health check (optional)
	hctx, cancel := context.WithTimeout(ctx, e.conf.ConnectTimeout)
	defer cancel()
	_ = e.healthPing(hctx) // don’t fail hard; we’ll reconnect anyway

	runCtx, runCancel := context.WithCancel(context.Background())
	e.cancel = runCancel

	e.runWG.Add(1)
	go func() {
		defer e.runWG.Done()
		e.runLoop(runCtx)
	}()

	// optimistic state
	e.setStateDegraded()
	return nil
}

func (e *Endpoint) Disconnect(ctx context.Context) error {
	e.mu.Lock()
	cancel := e.cancel
	e.cancel = nil
	e.mu.Unlock()

	if cancel != nil {
		cancel()
	}

	done := make(chan struct{})
	go func() {
		e.runWG.Wait()
		close(done)
	}()

	select {
	case <-done:
		e.setStateDown()
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (e *Endpoint) PTTDown(ctx context.Context, commandContext map[string]any) error {
	return e.postJSON(ctx, e.conf.PTTDownPath, commandContext)
}

func (e *Endpoint) PTTUp(ctx context.Context, commandContext map[string]any) error {
	return e.postJSON(ctx, e.conf.PTTUpPath, commandContext)
}

func (e *Endpoint) HealthCheck(ctx context.Context) (contract.HealthStatus, error) {
	// Conservative: if the loop thinks we're down, report down.
	if !e.connected.Load() {
		return contract.HealthDown, nil
	}

	// Ping bridge; if it fails, degrade (don’t instantly go down).
	if err := e.healthPing(ctx); err != nil {
		return contract.HealthDegraded, nil
	}

	if e.lastState == "degraded" {
		return contract.HealthDegraded, nil
	}
	return contract.HealthHealthy, nil
}

func (e *Endpoint) SetIndicator(ctx context.Context, name string, state string) error {
	_ = ctx
	_ = name
	_ = state
	// MCPTT bridges may support this; for now, do not fail core.
	return nil
}

// ---- internal loop ----

func (e *Endpoint) runLoop(ctx context.Context) {
	// jitter source
	rnd := rand.New(rand.NewSource(time.Now().UnixNano()))

	backoff := e.conf.BackoffInitial
	if backoff <= 0 {
		backoff = 500 * time.Millisecond
	}

	tick := time.NewTicker(e.conf.PollInterval)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if err := e.pollOnce(ctx); err != nil {
				e.consecutiveFails++
				if e.consecutiveFails >= e.conf.DownAfterFails {
					e.setStateDown()
				} else if e.consecutiveFails >= e.conf.DegradedAfterFails {
					e.setStateDegraded()
				}

				// backoff sleep (with jitter), but still responsive to ctx cancel
				sleep := backoff + time.Duration(rnd.Int63n(int64(backoff/3+1)))
				if sleep > e.conf.BackoffMax {
					sleep = e.conf.BackoffMax
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(sleep):
				}
				// exponential increase
				backoff = backoff * 2
				if backoff > e.conf.BackoffMax {
					backoff = e.conf.BackoffMax
				}
				continue
			}

			// success path
			e.consecutiveFails = 0
			backoff = e.conf.BackoffInitial
			if backoff <= 0 {
				backoff = 500 * time.Millisecond
			}
			e.setStateUp()
		}
	}
}

func (e *Endpoint) pollOnce(ctx context.Context) error {
	// GET {base}{eventsPath}?cursor=...
	u := strings.TrimRight(e.conf.BaseURL, "/") + e.conf.EventsPath
	if e.cursor != "" {
		sep := "?"
		if strings.Contains(u, "?") {
			sep = "&"
		}
		u = u + sep + "cursor=" + urlQueryEscape(e.cursor)
	}

	reqCtx, cancel := context.WithTimeout(ctx, e.conf.RequestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	e.applyAuth(req)

	resp, err := e.httpc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("events poll status %d", resp.StatusCode)
	}

	// Accept either:
	// 1) {"events":[{...},{...}], "next_cursor":"..."}  OR
	// 2) [{...},{...}]
	var raw any
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return err
	}

	var events []contract.Event
	var nextCursor string

	switch v := raw.(type) {
	case map[string]any:
		if evAny, ok := v["events"]; ok {
			events = decodeEventArray(evAny)
		}
		if nc, ok := v["next_cursor"].(string); ok {
			nextCursor = nc
		}
	case []any:
		events = decodeEventArray(v)
	default:
		// nothing we can do; treat as error
		return errors.New("unexpected events payload")
	}

	for _, ev := range events {
		// minimal requirement
		if ev.EventType == "" {
			continue
		}
		// dedupe
		if e.dedupe.Seen(hashEvent(ev)) {
			continue
		}
		e.emit(ev)
	}

	if nextCursor != "" {
		e.cursor = nextCursor
	}

	return nil
}

func decodeEventArray(anyVal any) []contract.Event {
	arr, ok := anyVal.([]any)
	if !ok {
		return nil
	}

	out := make([]contract.Event, 0, len(arr))
	for _, item := range arr {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		ev := contract.Event{}
		if s, ok := m["event_type"].(string); ok {
			ev.EventType = s
		}
		if s, ok := m["subscriber_id"].(string); ok {
			ev.SubscriberID = s
		}
		if s, ok := m["talkgroup_id"].(string); ok {
			ev.TalkgroupID = s
		}
		if s, ok := m["session_id"].(string); ok {
			ev.SessionID = s
		}
		if s, ok := m["origin_id"].(string); ok {
			ev.OriginID = s
		}
		if meta, ok := m["meta"].(map[string]any); ok {
			ev.Meta = meta
		}
		out = append(out, ev)
	}
	return out
}

func (e *Endpoint) postJSON(ctx context.Context, path string, body any) error {
	u := strings.TrimRight(e.conf.BaseURL, "/") + path

	b, err := json.Marshal(body)
	if err != nil {
		return err
	}

	reqCtx, cancel := context.WithTimeout(ctx, e.conf.RequestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, u, bytes.NewReader(b))
	if err != nil {
		return err
	}
	e.applyAuth(req)
	req.Header.Set("Content-Type", "application/json")

	resp, err := e.httpc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("command status %d", resp.StatusCode)
	}
	return nil
}

func (e *Endpoint) healthPing(ctx context.Context) error {
	if e.conf.HealthPath == "" {
		// if not configured, just assume ok
		return nil
	}
	u := strings.TrimRight(e.conf.BaseURL, "/") + e.conf.HealthPath

	reqCtx, cancel := context.WithTimeout(ctx, e.conf.RequestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	e.applyAuth(req)

	resp, err := e.httpc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("health status %d", resp.StatusCode)
	}
	return nil
}

func (e *Endpoint) applyAuth(req *http.Request) {
	if e.conf.AuthHeader != "" && e.conf.AuthToken != "" {
		req.Header.Set(e.conf.AuthHeader, e.conf.AuthToken)
	}
}

// ---- cfg parsing (from map[string]any) ----

func parseCfg(m map[string]any) (cfg, error) {
	// Safe defaults for production
	c := cfg{
		BaseURL: "http://127.0.0.1:5070",

		EventsPath:  "/api/events",
		PTTDownPath: "/api/ptt/down",
		PTTUpPath:   "/api/ptt/up",
		HealthPath:  "/api/health",

		PollInterval:       200 * time.Millisecond,
		RequestTimeout:     2 * time.Second,
		ConnectTimeout:     2 * time.Second,
		BackoffInitial:     500 * time.Millisecond,
		BackoffMax:         10 * time.Second,
		DegradedAfterFails: 2,
		DownAfterFails:     6,

		DedupeTTL: 3 * time.Second,
		DedupeMax: 2048,

		AuthHeader: "",
		AuthToken:  "",
	}

	if m == nil {
		return c, errors.New("mcptt_http config missing")
	}

	// required-ish
	if s, ok := asString(m["base_url"]); ok && s != "" {
		c.BaseURL = s
	}

	// optional paths
	if s, ok := asString(m["events_path"]); ok && s != "" {
		c.EventsPath = s
	}
	if s, ok := asString(m["ptt_down_path"]); ok && s != "" {
		c.PTTDownPath = s
	}
	if s, ok := asString(m["ptt_up_path"]); ok && s != "" {
		c.PTTUpPath = s
	}
	if s, ok := asString(m["health_path"]); ok && s != "" {
		c.HealthPath = s
	}

	// timings
	if d, ok := asDurationMS(m["poll_interval_ms"]); ok && d > 0 {
		c.PollInterval = d
	}
	if d, ok := asDurationMS(m["request_timeout_ms"]); ok && d > 0 {
		c.RequestTimeout = d
	}
	if d, ok := asDurationMS(m["connect_timeout_ms"]); ok && d > 0 {
		c.ConnectTimeout = d
	}
	if d, ok := asDurationMS(m["backoff_initial_ms"]); ok && d > 0 {
		c.BackoffInitial = d
	}
	if d, ok := asDurationMS(m["backoff_max_ms"]); ok && d > 0 {
		c.BackoffMax = d
	}

	if n, ok := asInt(m["degraded_after_fails"]); ok && n >= 1 {
		c.DegradedAfterFails = n
	}
	if n, ok := asInt(m["down_after_fails"]); ok && n >= 1 {
		c.DownAfterFails = n
	}

	if d, ok := asDurationMS(m["dedupe_ttl_ms"]); ok && d > 0 {
		c.DedupeTTL = d
	}
	if n, ok := asInt(m["dedupe_max"]); ok && n >= 64 {
		c.DedupeMax = n
	}

	if s, ok := asString(m["auth_header"]); ok {
		c.AuthHeader = s
	}
	if s, ok := asString(m["auth_token"]); ok {
		c.AuthToken = s
	}

	// sanity
	if !strings.HasPrefix(c.EventsPath, "/") {
		c.EventsPath = "/" + c.EventsPath
	}
	if !strings.HasPrefix(c.PTTDownPath, "/") {
		c.PTTDownPath = "/" + c.PTTDownPath
	}
	if !strings.HasPrefix(c.PTTUpPath, "/") {
		c.PTTUpPath = "/" + c.PTTUpPath
	}
	if c.HealthPath != "" && !strings.HasPrefix(c.HealthPath, "/") {
		c.HealthPath = "/" + c.HealthPath
	}

	if c.BaseURL == "" {
		return c, fmt.Errorf("mcptt_http base_url must not be empty")
	}

	return c, nil
}

func asString(v any) (string, bool) {
	s, ok := v.(string)
	return s, ok
}

func asInt(v any) (int, bool) {
	switch t := v.(type) {
	case int:
		return t, true
	case int64:
		return int(t), true
	case float64:
		// JSON numbers decode as float64 when coming from map[string]any
		return int(t), true
	default:
		return 0, false
	}
}

func asDurationMS(v any) (time.Duration, bool) {
	n, ok := asInt(v)
	if !ok {
		return 0, false
	}
	return time.Duration(n) * time.Millisecond, true
}

// ---- dedupe ----

type dedupeStore struct {
	mu   sync.Mutex
	max  int
	ttl  time.Duration
	data map[string]time.Time
}

func newDedupeStore(max int, ttl time.Duration) *dedupeStore {
	return &dedupeStore{
		max:  max,
		ttl:  ttl,
		data: make(map[string]time.Time, max),
	}
}

func (d *dedupeStore) Seen(key string) bool {
	now := time.Now()
	d.mu.Lock()
	defer d.mu.Unlock()

	// cleanup occasionally when big
	if len(d.data) > d.max {
		d.compactLocked(now)
	}

	if exp, ok := d.data[key]; ok {
		if now.Before(exp) {
			return true
		}
		delete(d.data, key)
	}

	d.data[key] = now.Add(d.ttl)
	return false
}

func (d *dedupeStore) compactLocked(now time.Time) {
	for k, exp := range d.data {
		if !now.Before(exp) {
			delete(d.data, k)
		}
	}
	// still too big? drop random-ish (iteration order)
	if len(d.data) <= d.max {
		return
	}
	excess := len(d.data) - d.max
	for k := range d.data {
		delete(d.data, k)
		excess--
		if excess <= 0 {
			break
		}
	}
}

func hashEvent(ev contract.Event) string {
	// stable hash over key fields; meta ignored (often includes timestamps)
	h := sha256.New()
	h.Write([]byte(ev.EventType))
	h.Write([]byte{0})
	h.Write([]byte(ev.SubscriberID))
	h.Write([]byte{0})
	h.Write([]byte(ev.TalkgroupID))
	h.Write([]byte{0})
	h.Write([]byte(ev.SessionID))
	h.Write([]byte{0})
	h.Write([]byte(ev.OriginID))
	sum := h.Sum(nil)
	return hex.EncodeToString(sum)
}

// minimal query escaping for cursors (opaque token)
func urlQueryEscape(s string) string {
	// avoid pulling in net/url for one function; keep it simple & safe enough for cursors
	r := strings.NewReplacer(
		"%", "%25",
		" ", "%20",
		"+", "%2B",
		"&", "%26",
		"?", "%3F",
		"#", "%23",
		"=", "%3D",
	)
	return r.Replace(s)
}
