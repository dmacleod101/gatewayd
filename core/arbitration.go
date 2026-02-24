package core

import (
	"fmt"
	"time"

	"gatewayd/core/config"
	"gatewayd/core/event"
)

type ArbDecision struct {
	Allow    bool
	Preempt  bool
	Reason   string
	OldOwner string
	NewOwner string
}

type Arbitrator struct {
	policy     config.ArbitrationPolicy
	priorities map[string]int
	timeout    time.Duration
}

func NewArbitrator(a config.Arbitration) *Arbitrator {
	to := time.Duration(0)
	if a.TXLockTimeoutMS > 0 {
		to = time.Duration(a.TXLockTimeoutMS) * time.Millisecond
	}
	p := a.Priorities
	if p == nil {
		p = map[string]int{}
	}
	return &Arbitrator{
		policy:     a.Policy,
		priorities: p,
		timeout:    to,
	}
}

func (a *Arbitrator) Timeout() time.Duration { return a.timeout }

func (a *Arbitrator) priority(interfaceID string) int {
	if v, ok := a.priorities[interfaceID]; ok {
		return v
	}
	return 0
}

// DecideEvent determines whether an event should be allowed to affect the FSM/routing.
// It only arbitrates TXStart/TXStop. Other events are always allowed.
func (a *Arbitrator) DecideEvent(now time.Time, fsmState string, txOwner string, lastChange time.Time, ev event.Event) (ArbDecision, error) {
	// Default allow
	if ev.Type != event.TXStart && ev.Type != event.TXStop {
		return ArbDecision{Allow: true, Reason: "not_tx_event"}, nil
	}

	// TX lock timeout handling is applied outside this function (in core loop),
	// because it can change state even without a TXStart/TXStop event.
	_ = now
	_ = lastChange

	switch ev.Type {
	case event.TXStart:
		// If not currently in TX, allow
		if fsmState != "tx" || txOwner == "" {
			return ArbDecision{
				Allow:   true,
				Reason: "tx_idle",
			}, nil
		}

		// If same owner, allow
		if txOwner == ev.InterfaceID {
			return ArbDecision{
				Allow:   true,
				Reason: "tx_owner_repeat",
			}, nil
		}

		// Another interface wants TX while locked
		switch a.policy {
		case config.ArbFirstWins, "":
			return ArbDecision{
				Allow:    false,
				Preempt:  false,
				Reason:   "first_wins_locked",
				OldOwner: txOwner,
				NewOwner: ev.InterfaceID,
			}, nil

		case config.ArbPriorityPreempt:
			oldP := a.priority(txOwner)
			newP := a.priority(ev.InterfaceID)

			if newP > oldP {
				return ArbDecision{
					Allow:    true,
					Preempt:  true,
					Reason:   fmt.Sprintf("priority_preempt %d>%d", newP, oldP),
					OldOwner: txOwner,
					NewOwner: ev.InterfaceID,
				}, nil
			}

			return ArbDecision{
				Allow:    false,
				Preempt:  false,
				Reason:   fmt.Sprintf("priority_blocked %d<=%d", newP, oldP),
				OldOwner: txOwner,
				NewOwner: ev.InterfaceID,
			}, nil

		default:
			return ArbDecision{}, fmt.Errorf("unknown arbitration policy: %s", a.policy)
		}

	case event.TXStop:
		// Only current owner can end TX (sane default)
		if fsmState == "tx" && txOwner != "" && txOwner != ev.InterfaceID {
			return ArbDecision{
				Allow:    false,
				Preempt:  false,
				Reason:   "tx_stop_not_owner",
				OldOwner: txOwner,
				NewOwner: ev.InterfaceID,
			}, nil
		}
		return ArbDecision{Allow: true, Reason: "tx_stop_allowed"}, nil
	}

	return ArbDecision{Allow: true, Reason: "default"}, nil
}
