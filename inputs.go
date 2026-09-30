package main

// Jump input consistency. A real client derives the button states and the subtick moves from the same
// key events, so they always agree: every press or release shows up in both, and a command starts with
// the button where the previous one left it. Inputs written into the command by something else tend to
// fill in only one of them.

// EInButtonState of a button, built like CInButtonState::GetButtonState: buttonstate1 (held at the end
// of the command) + buttonstate2 * 2 + buttonstate3 * 4. The result is the button's history over the
// command, e.g. 1 (DOWN) is held throughout, 3 (UP_DOWN) is a fresh press that stays held and
// 4 (UP_DOWN_UP) is a press and release inside the same command, which is what a scroll wheel produces.
func buttonState(b ButtonStates, button uint64) uint8 {
	state := uint8(0)
	if b.ButtonState1&button != 0 {
		state |= 1
	}
	if b.ButtonState2&button != 0 {
		state |= 2
	}
	if b.ButtonState3&button != 0 {
		state |= 4
	}
	return state
}

// Whether the button was down at the start of the command, for each EInButtonState.
func buttonStateStartsDown(state uint8) bool {
	return state == 1 || state == 2 || state == 5 || state == 6
}

// Number of presses and releases the button went through during the command.
func buttonStateTransitions(state uint8) int {
	switch state {
	case 0, 1:
		return 0
	case 2, 3:
		return 1
	case 4, 5:
		return 2
	}
	return 3
}

func isScrollState(state uint8) bool {
	// Pressed and released again within the command.
	return state == 4 || state == 6
}

type InputStats struct {
	JumpPressMoves           int // Jump presses in the subtick moves
	PressMovesWithoutState   int // ...whose command's button state shows no press
	StateChangesWithoutMoves int // Commands whose jump button state changed with no jump move at all
	ChainBreaks              int // Commands starting with jump in a different state than the previous one ended
	TripleChanges            int // Commands where jump went down, up and down (or up, down, up) again
}

type inputState struct {
	lastCmdNum  int32
	lastEndDown bool
	valid       bool
}

// Suspicious once any of the counters reaches this. Legitimate players stay at 0 bar a rare chain break
// around reconnects.
const inputInconsistencyThreshold = 10

func (s InputStats) Flagged() bool {
	return s.PressMovesWithoutState >= inputInconsistencyThreshold ||
		s.StateChangesWithoutMoves >= inputInconsistencyThreshold ||
		s.ChainBreaks >= inputInconsistencyThreshold
}

func (pd *PlayerData) CheckInputConsistency(cmdNum int32, cmd *BaseUserCmd) {
	s := &pd.Inputs
	state := buttonState(cmd.Buttons, IN_JUMP)
	pressed, released := false, false
	for _, step := range cmd.SubtickMoves {
		if step.HasButton && step.Button == IN_JUMP {
			if step.Pressed {
				pressed = true
				s.JumpPressMoves++
			} else {
				released = true
			}
		}
	}
	if pressed && cmd.Buttons.NewlyPressed()&IN_JUMP == 0 {
		s.PressMovesWithoutState++
	}
	if buttonStateTransitions(state) > 0 && !pressed && !released {
		s.StateChangesWithoutMoves++
	}
	if buttonStateTransitions(state) == 3 {
		s.TripleChanges++
	}
	// Only compare with the directly preceding command, ignored and missing commands break the chain.
	if pd.inputs.valid && cmdNum == pd.inputs.lastCmdNum+1 && buttonStateStartsDown(state) != pd.inputs.lastEndDown {
		s.ChainBreaks++
	}
	pd.inputs = inputState{lastCmdNum: cmdNum, lastEndDown: state&1 != 0, valid: true}
}
