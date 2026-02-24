package contract

import "context"

// ---- Event + Fields ----

type FieldName string

const (
	FieldEventType    FieldName = "event_type"
	FieldSubscriberID FieldName = "subscriber_id"
	FieldTalkgroupID  FieldName = "talkgroup_id"
	FieldSessionID    FieldName = "session_id"
	FieldOriginID     FieldName = "origin_id"
)

type Event struct {
	EventType     string         `json:"event_type"`
	SubscriberID  string         `json:"subscriber_id,omitempty"`
	TalkgroupID   string         `json:"talkgroup_id,omitempty"`
	SessionID     string         `json:"session_id,omitempty"`
	OriginID      string         `json:"origin_id,omitempty"`
	Meta          map[string]any `json:"meta,omitempty"`
}

// ---- Health ----

type HealthStatus string

const (
	HealthHealthy  HealthStatus = "healthy"
	HealthDegraded HealthStatus = "degraded"
	HealthDown     HealthStatus = "down"
)

// ---- Capabilities ----

type Capabilities struct {
	OutgoingFields     map[FieldName]bool
	SupportsIndicator  bool
}

// ---- Endpoint contract ----

type Endpoint interface {
	ID() string
	Type() string
	Driver() string

	Capabilities() Capabilities

	Connect(ctx context.Context) error
	Disconnect(ctx context.Context) error

	PTTDown(ctx context.Context, commandContext map[string]any) error
	PTTUp(ctx context.Context, commandContext map[string]any) error

	HealthCheck(ctx context.Context) (HealthStatus, error)

	SetIndicator(ctx context.Context, name string, state string) error
}

type EventSource interface {
	SetEventSink(func(Event))
}

type ErrNotSupported struct {
	What string
}

func (e ErrNotSupported) Error() string { return "not supported: " + e.What }
