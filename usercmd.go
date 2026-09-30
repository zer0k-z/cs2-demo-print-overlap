package main

import (
	msg "github.com/markus-wa/demoinfocs-golang/v6/pkg/demoinfocs/msg"
)

// The parts of CSGOUserCmdPB / CBaseUserCmdPB (hl2sdk-cs2 game/shared/usercmd.proto) that the
// anticheat detectors need, with explicit presence flags so the checks read like the original.
// demoinfocs reconstructs the full commands from the demo's full and delta-encoded payloads.

type SubtickMoveStep struct {
	Button             uint64
	Pressed            bool
	When               float32
	AnalogForwardDelta float32
	AnalogLeftDelta    float32
	PitchDelta         float32
	YawDelta           float32

	HasButton             bool
	HasAnalogForwardDelta bool
	HasAnalogLeftDelta    bool
	HasYawDelta           bool
}

type ButtonStates struct {
	ButtonState1 uint64 // Buttons held at the end of the command
	ButtonState2 uint64
	ButtonState3 uint64
}

// Same rule as CInButtonState::IsButtonNewlyPressed, where any state from UP_DOWN upwards had a press.
func (b ButtonStates) NewlyPressed() uint64 {
	return b.ButtonState3 | (b.ButtonState1 & b.ButtonState2)
}

type BaseUserCmd struct {
	Buttons           ButtonStates
	SubtickMoves      []SubtickMoveStep
	HasExecutionNotes bool  // Set when the server ignored the command
	ServerTick        int32 // Server tick the command was executed in
	// Render time (in ticks) of the client frames in the command's input history. The client sends
	// every frame of the tick when there are at most 4, otherwise 4 spread over the tick.
	FrameTimes []float64
}

func baseUserCmdFromProto(cmd *msg.CSGOUserCmdPB, serverTick int32) BaseUserCmd {
	base := cmd.GetBase()
	out := BaseUserCmd{
		Buttons: ButtonStates{
			ButtonState1: base.GetButtonsPb().GetButtonstate1(),
			ButtonState2: base.GetButtonsPb().GetButtonstate2(),
			ButtonState3: base.GetButtonsPb().GetButtonstate3(),
		},
		HasExecutionNotes: base.GetExecutionNotes() != nil,
		ServerTick:        serverTick,
	}
	for _, entry := range cmd.GetInputHistory() {
		out.FrameTimes = append(out.FrameTimes, float64(entry.GetRenderTickCount())+float64(entry.GetRenderTickFraction()))
	}
	for _, step := range base.GetSubtickMoves() {
		out.SubtickMoves = append(out.SubtickMoves, SubtickMoveStep{
			Button:                step.GetButton(),
			Pressed:               step.GetPressed(),
			When:                  step.GetWhen(),
			AnalogForwardDelta:    step.GetAnalogForwardDelta(),
			AnalogLeftDelta:       step.GetAnalogLeftDelta(),
			PitchDelta:            step.GetPitchDelta(),
			YawDelta:              step.GetYawDelta(),
			HasButton:             step.Button != nil,
			HasAnalogForwardDelta: step.AnalogForwardDelta != nil,
			HasAnalogLeftDelta:    step.AnalogLeftDelta != nil,
			HasYawDelta:           step.YawDelta != nil,
		})
	}
	return out
}
