package lead

type State string

const (
	StateNew       State = "new"
	StateContacted State = "contacted"
	StateQualified State = "qualified"
	StateQuoted    State = "quoted"
	StateWon       State = "won"
	StateLost      State = "lost"
	StateConverted State = "converted"
)

var allowed = map[State][]State{
	StateNew:       {StateContacted, StateQualified, StateLost},
	StateContacted: {StateQualified, StateLost},
	StateQualified: {StateQuoted, StateLost},
	StateQuoted:    {StateWon, StateLost},
	StateWon:       {StateLost},
	StateLost:      {},
	StateConverted: {},
}

func ValidState(s string) bool {
	_, ok := allowed[State(s)]
	return ok
}

func CanTransition(from, to State) bool {
	if from == StateConverted || from == StateLost {
		return false
	}
	for _, next := range allowed[from] {
		if next == to {
			return true
		}
	}
	return false
}

// CanConvert reports whether a lead may be turned into a client/project. Lost
// and already-converted leads are terminal.
func CanConvert(status State) bool {
	switch status {
	case StateNew, StateContacted, StateQualified, StateQuoted, StateWon:
		return true
	}
	return false
}
