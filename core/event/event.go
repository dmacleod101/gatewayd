package event

import (
	"fmt"
	"time"

	"gatewayd/endpoints/contract"
)

type EventType string

const (
	LinkUp        EventType = "link_up"
	LinkDown      EventType = "link_down"
	StateIdle     EventType = "state_idle"
	RXStart       EventType = "rx_start"
	RXStop        EventType = "rx_stop"
	TXStart       EventType = "tx_start"
	TXStop        EventType = "tx_stop"
	LocalPTTDown  EventType = "local_ptt_down"
	LocalPTTUp    EventType = "local_ptt_up"
	Fault         EventType = "fault"
	CmdAck        EventType = "cmd_ack"
)

type Event struct {
	TS          time.Time
	InterfaceID string
	Type        EventType

	SubscriberID *string
	TalkgroupID  *string
	SessionID    *string
	OriginID     *string

	Meta map[string]any
}

type Requirements struct {
	RequireSubscriberID bool
	RequireTalkgroupID  bool
	RequireSessionID    bool
	RequireOriginID     bool
}

func strPtrOrNil(s string) *string {
	if s == "" {
		return nil
	}
	v := s // ensure stable address
	return &v
}

func FromEndpoint(interfaceID string, epEv contract.Event) (Event, error) {
	if interfaceID == "" {
		return Event{}, fmt.Errorf("interface_id must not be empty")
	}
	if epEv.EventType == "" {
		return Event{}, fmt.Errorf("event_type must not be empty")
	}

	return Event{
		TS:          time.Now().UTC(),
		InterfaceID: interfaceID,
		Type:        EventType(epEv.EventType),

		SubscriberID: strPtrOrNil(epEv.SubscriberID),
		TalkgroupID:  strPtrOrNil(epEv.TalkgroupID),
		SessionID:    strPtrOrNil(epEv.SessionID),
		OriginID:     strPtrOrNil(epEv.OriginID),

		Meta: epEv.Meta,
	}, nil
}

func (e Event) Validate(req Requirements) error {
	if e.Type == "" {
		return fmt.Errorf("event_type missing")
	}

	if req.RequireSubscriberID && (e.SubscriberID == nil || *e.SubscriberID == "") {
		return fmt.Errorf("subscriber_id required")
	}
	if req.RequireTalkgroupID && (e.TalkgroupID == nil || *e.TalkgroupID == "") {
		return fmt.Errorf("talkgroup_id required")
	}
	if req.RequireSessionID && (e.SessionID == nil || *e.SessionID == "") {
		return fmt.Errorf("session_id required")
	}
	if req.RequireOriginID && (e.OriginID == nil || *e.OriginID == "") {
		return fmt.Errorf("origin_id required")
	}

	return nil
}
