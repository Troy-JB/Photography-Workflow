package lead

import "testing"

func TestCanTransition(t *testing.T) {
	cases := []struct {
		from, to State
		want     bool
	}{
		{StateNew, StateContacted, true},
		{StateNew, StateQualified, true},
		{StateContacted, StateQualified, true},
		{StateQualified, StateQuoted, true},
		{StateQuoted, StateWon, true},
		{StateWon, StateLost, true},
		{StateNew, StateWon, false},
		{StateQualified, StateWon, false},
		{StateLost, StateQualified, false},
		{StateConverted, StateNew, false},
	}
	for _, c := range cases {
		if got := CanTransition(c.from, c.to); got != c.want {
			t.Fatalf("CanTransition(%s, %s) = %v, want %v", c.from, c.to, got, c.want)
		}
	}
}

func TestCanConvert(t *testing.T) {
	for _, s := range []State{StateNew, StateContacted, StateQualified, StateQuoted, StateWon} {
		if !CanConvert(s) {
			t.Fatalf("%s should be convertible", s)
		}
	}
	for _, s := range []State{StateLost, StateConverted} {
		if CanConvert(s) {
			t.Fatalf("%s must not be convertible", s)
		}
	}
}

func TestValidState(t *testing.T) {
	for _, s := range []State{StateNew, StateContacted, StateQualified, StateQuoted, StateWon, StateLost, StateConverted} {
		if !ValidState(string(s)) {
			t.Fatalf("%s should be valid", s)
		}
	}
	if ValidState("archived") {
		t.Fatal("archived is not a lead state")
	}
}
