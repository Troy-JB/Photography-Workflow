package project

type State string

// Project lifecycle only. Contract, payment, asset and proof states are owned by
// their own aggregates (see plan §3); projects keep projections for gating.
const (
	StateDraft           State = "draft"
	StateQuoted          State = "quoted"
	StateContractPending State = "contract_pending"
	StateContracted      State = "contracted"
	StateBooked          State = "booked"
	StateShooting        State = "shooting"
	StatePostProduction  State = "post_production"
	StateCompleted       State = "completed"
	StateArchived        State = "archived"
	StateOnHold          State = "on_hold"
	StateCancelled       State = "cancelled"
)

var allowed = map[State][]State{
	StateDraft:           {StateQuoted},
	StateQuoted:          {StateContractPending},
	StateContractPending: {StateContracted},
	StateContracted:      {StateBooked},
	StateBooked:          {StateShooting},
	StateShooting:        {StatePostProduction},
	StatePostProduction:  {StateCompleted},
	StateCompleted:       {StateArchived},
	StateOnHold:          {},
	StateCancelled:       {},
	StateArchived:        {},
}

func ValidState(s string) bool {
	_, ok := allowed[State(s)]
	return ok
}

// CanTransition reports the structural transition graph. Entering on_hold and
// cancelled is allowed from every non-terminal state; leaving on_hold must go
// back to the state it interrupted, which needs persisted context and is
// enforced by CanResume in the service.
func CanTransition(from, to State) bool {
	if from == StateCancelled || from == StateArchived {
		return false
	}
	if to == StateCancelled || to == StateOnHold {
		return to != from
	}
	if from == StateOnHold {
		return false
	}
	for _, next := range allowed[from] {
		if next == to {
			return true
		}
	}
	return false
}

// CanResume is the only legal way out of on_hold: back to the state that was
// interrupted, or cancellation.
func CanResume(interrupted, to State) bool {
	return to == interrupted || to == StateCancelled
}

func AllStates() []State {
	out := make([]State, 0, len(allowed))
	for s := range allowed {
		out = append(out, s)
	}
	return out
}
