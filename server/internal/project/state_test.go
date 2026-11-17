package project

import "testing"

func TestHappyPathTransitions(t *testing.T) {
	path := []State{
		StateDraft, StateQuoted, StateContractPending, StateContracted,
		StateBooked, StateShooting, StatePostProduction, StateCompleted, StateArchived,
	}
	for i := 0; i < len(path)-1; i++ {
		if !CanTransition(path[i], path[i+1]) {
			t.Fatalf("expected %s -> %s to be allowed", path[i], path[i+1])
		}
	}
}

func TestIllegalTransitions(t *testing.T) {
	cases := [][2]State{
		{StateDraft, StateBooked},
		{StateDraft, StateCompleted},
		{StateQuoted, StateShooting},
		{StateContractPending, StateBooked},
		{StateBooked, StateCompleted},
		{StateArchived, StateDraft},
		{StateCancelled, StateDraft},
		{StateOnHold, StateBooked},
	}
	for _, c := range cases {
		if CanTransition(c[0], c[1]) {
			t.Fatalf("expected %s -> %s to be rejected", c[0], c[1])
		}
	}
}

func TestAnyActiveStateCanBeCancelledOrHeld(t *testing.T) {
	for _, s := range AllStates() {
		if s == StateCancelled || s == StateArchived {
			continue
		}
		if !CanTransition(s, StateCancelled) {
			t.Fatalf("%s -> cancelled should be allowed", s)
		}
		if s != StateOnHold && !CanTransition(s, StateOnHold) {
			t.Fatalf("%s -> on_hold should be allowed", s)
		}
	}
	if CanTransition(StateOnHold, StateOnHold) {
		t.Fatal("on_hold -> on_hold should be rejected")
	}
}

func TestOnHoldResumeOnlyToInterruptedState(t *testing.T) {
	if !CanResume(StatePostProduction, StatePostProduction) {
		t.Fatal("on_hold should resume to the interrupted state")
	}
	if !CanResume(StatePostProduction, StateCancelled) {
		t.Fatal("on_hold should allow cancellation")
	}
	if CanResume(StatePostProduction, StateCompleted) {
		t.Fatal("on_hold must not jump to a later state")
	}
	if CanResume(StateBooked, StateArchived) {
		t.Fatal("on_hold must not resume to archived")
	}
}

func TestTerminalStates(t *testing.T) {
	for _, s := range []State{StateCancelled, StateArchived} {
		for _, to := range AllStates() {
			if CanTransition(s, to) {
				t.Fatalf("%s must be terminal, but %s -> %s is allowed", s, s, to)
			}
		}
	}
}

func TestTransitionGates(t *testing.T) {
	cases := []struct {
		name    string
		to      State
		payment string
		blocked bool
	}{
		{"contracted without signature", StateContracted, "unpaid", false},
		{"booked needs deposit", StateBooked, "unpaid", true},
		{"booked without signature but with deposit", StateBooked, "deposit_paid", false},
		{"booked with full payment", StateBooked, "paid", false},
		{"completed needs final payment", StateCompleted, "deposit_paid", true},
		{"completed with payment", StateCompleted, "paid", false},
		{"other steps ungated", StateShooting, "unpaid", false},
	}
	for _, c := range cases {
		err := checkGate(c.to, c.payment)
		if c.blocked && err == nil {
			t.Fatalf("%s: expected gate rejection", c.name)
		}
		if !c.blocked && err != nil {
			t.Fatalf("%s: unexpected gate rejection: %v", c.name, err)
		}
	}
}

func TestValidState(t *testing.T) {
	if !ValidState("booked") {
		t.Fatal("booked should be valid")
	}
	for _, gone := range []string{"contract_sent", "deposit_due", "shot", "proofing", "delivered"} {
		if ValidState(gone) {
			t.Fatalf("%s must not be a project state anymore", gone)
		}
	}
	if got := len(AllStates()); got != 11 {
		t.Fatalf("AllStates length = %d, want 11", got)
	}
}
