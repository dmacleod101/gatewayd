package statemachine

import (
	"fmt"
	"time"

	"gatewayd/core/event"
)

type State string

const (
	Idle State = "idle"
	RX   State = "rx"
	TX   State = "tx"
)

type Context struct {
	State     State
	TXOwner   string
	UpdatedAt time.Time
}

type Input struct {
	EventType   event.EventType
	InterfaceID string
}

func New() Context {
	return Context{
		State:     Idle,
		TXOwner:   "",
		UpdatedAt: time.Now().UTC(),
	}
}

// Step is a deterministic FSM transition function.
// It only uses current state + input, and returns the next state.
func Step(cur Context, in Input) (Context, error) {
	next := cur

	switch cur.State {
	case Idle:
		switch in.EventType {
		case event.RXStart:
			next.State = RX
			next.UpdatedAt = time.Now().UTC()
			return next, nil
		case event.TXStart:
			next.State = TX
			next.TXOwner = in.InterfaceID
			next.UpdatedAt = time.Now().UTC()
			return next, nil
		default:
			return next, nil
		}

	case RX:
		switch in.EventType {
		case event.RXStop:
			next.State = Idle
			next.UpdatedAt = time.Now().UTC()
			return next, nil
		case event.TXStart:
			// RX -> TX is allowed (core arbitration decides who can do it).
			next.State = TX
			next.TXOwner = in.InterfaceID
			next.UpdatedAt = time.Now().UTC()
			return next, nil
		default:
			return next, nil
		}

	case TX:
		switch in.EventType {
		case event.TXStop:
			// Only the current owner should stop TX; arbitration should already enforce this.
			next.State = Idle
			next.TXOwner = ""
			next.UpdatedAt = time.Now().UTC()
			return next, nil
		default:
			return next, nil
		}

	default:
		return next, fmt.Errorf("unknown state: %s", cur.State)
	}
}
