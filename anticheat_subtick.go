package main

import (
	"fmt"
	"math"
)

// Port of cs2kz-metamod src/kz/anticheat/detectors/subtick.cpp
// Detects InvalidInput, SubtickSpam and Desubtick infractions.

// Tunable thresholds for subtick command checks
const (
	subtickInitialIgnoreTime        float32 = 10.0
	subtickInvalidCommandWindow     float32 = 5.0
	subtickInvalidCommandThreshold          = 5
	subtickSuspiciousMovesWindow    float32 = 0.5
	subtickSuspiciousMovesThreshold         = 20
	subtickYawDeltaThreshold                = 2
	subtickSubtickInputsWindow      float32 = 20.0
	subtickSubtickInputsThreshold           = 30
	subtickZeroWhenRatioThreshold   float32 = 0.9
)

// The engine's own limits on a subtick move list, from CCSPlayer_MovementServices.
const (
	engineMaxSubtickMoves         = 32
	maxSubtickMoveImpulse float32 = 2.0
	// One button per subtick move, and only the ones the engine knows how to apply.
	subtickMoveButtonMask = IN_ATTACK | IN_ATTACK2 | IN_JUMP | IN_DUCK | IN_FORWARD | IN_BACK | IN_MOVELEFT | IN_MOVERIGHT
)

// Why the engine would throw away a command's entire subtick move list.
type SubtickRejection int

const (
	SubtickRejectionNone SubtickRejection = iota
	SubtickRejectionMoveCount
	SubtickRejectionWhen
	SubtickRejectionButton
	SubtickRejectionImpulseLimit
)

// Every command should have all button presses/releases accounted for in subtick moves.
// Only cheats that modify buttons without updating subtick moves would fail this.
func verifyCommand(cmd *BaseUserCmd) bool {
	// Expect a press/release this tick
	expectedButtons := cmd.Buttons.ButtonState2 | cmd.Buttons.ButtonState3
	// We only care about certain movement buttons.
	expectedButtons &= IN_JUMP | IN_DUCK
	for _, step := range cmd.SubtickMoves {
		if step.HasButton {
			expectedButtons &^= step.Button
		}
	}
	return expectedButtons == 0
}

// Yaw alias abuse detection: A legitimate client only produces a handful of angle changes per command, unless they are scrolling to jump.
// If there's more than subtickYawDeltaThreshold subtick moves carrying an actual yaw change in a single tick, flag it.
func hasExcessiveSubtickMovesWithAngles(cmd *BaseUserCmd) bool {
	numYawDeltas := 0
	for _, step := range cmd.SubtickMoves {
		// Only count moves that actually turn the player.
		if !step.HasYawDelta || step.YawDelta == 0.0 {
			continue
		}
		numYawDeltas++
		if numYawDeltas > subtickYawDeltaThreshold {
			return true
		}
	}
	return false
}

// The engine validates the whole subtick move list before it applies any of it, and throws all of it away if
// any move fails. A command that fails this moves the player from its final button state for the whole tick,
// the same as a command that carried no subtick moves at all.
// forwardAxis and sideAxis are the movement impulses the command starts from. The accumulated impulses are
// returned alongside the result.
func verifySubtickMoves(cmd *BaseUserCmd, forwardAxis float32, sideAxis float32) (SubtickRejection, float32, float32) {
	if len(cmd.SubtickMoves) > engineMaxSubtickMoves {
		return SubtickRejectionMoveCount, forwardAxis, sideAxis
	}
	lastWhen := float32(0.0)
	for _, step := range cmd.SubtickMoves {
		// The moves have to be ordered and land inside the tick.
		if step.When < lastWhen || step.When > 1.0 {
			return SubtickRejectionWhen, forwardAxis, sideAxis
		}
		lastWhen = step.When
		button := step.Button
		if button != 0 {
			// Only one button can be pressed at a time, and it must be a valid button.
			if button&(button-1) != 0 || button&subtickMoveButtonMask == 0 {
				return SubtickRejectionButton, forwardAxis, sideAxis
			}
			delta := float32(-1.0)
			if step.Pressed {
				delta = 1.0
			}
			switch button {
			case IN_FORWARD:
				forwardAxis += delta
			case IN_BACK:
				forwardAxis -= delta
			case IN_MOVELEFT:
				sideAxis += delta
			case IN_MOVERIGHT:
				sideAxis -= delta
			}
		} else {
			forwardAxis += step.AnalogForwardDelta
			sideAxis += step.AnalogLeftDelta
		}
		// Neither a stick nor a keyboard can push an axis past this.
		if abs32(forwardAxis) > maxSubtickMoveImpulse || abs32(sideAxis) > maxSubtickMoveImpulse {
			return SubtickRejectionImpulseLimit, forwardAxis, sideAxis
		}
	}
	return SubtickRejectionNone, forwardAxis, sideAxis
}

func possibleDesubtickedCommand(cmd *BaseUserCmd, impulses [2]float32) bool {
	// Can't tell if there are no subtick moves.
	if len(cmd.SubtickMoves) == 0 {
		return false
	}

	// An oversized analog delta makes the engine discard the whole list, and falls back to the buttonstates alone.
	// This can be achieved by using `forwardback 5 0 0` for instance.
	if rejection, _, _ := verifySubtickMoves(cmd, impulses[0], impulses[1]); rejection == SubtickRejectionImpulseLimit {
		return true
	}

	// If there are subtick moves but the timing is at exactly 0, it's suspicious... unless it's controller-issued commands.
	hasZeroWhen := true
	hasNonAnalogMove := false
	for _, step := range cmd.SubtickMoves {
		if step.HasAnalogForwardDelta || step.HasAnalogLeftDelta {
			continue
		}
		hasNonAnalogMove = true
		hasZeroWhen = hasZeroWhen && step.When == 0.0
	}
	// If every move was analog, we have no non-analog evidence to call this suspicious.
	return hasNonAnalogMove && hasZeroWhen
}

// m_vecLastMovementImpulses isn't networked, so emulate what SetupMove leaves in it: the accumulated
// subtick impulses when the move list is accepted, otherwise the impulses derived from the final button state.
func (pd *PlayerData) UpdateMovementImpulses(cmd *BaseUserCmd) {
	if len(cmd.SubtickMoves) > 0 {
		rejection, forward, side := verifySubtickMoves(cmd, pd.movementImpulses[0], pd.movementImpulses[1])
		if rejection == SubtickRejectionNone {
			pd.movementImpulses = [2]float32{forward, side}
			return
		}
	}
	buttons := cmd.Buttons.ButtonState1
	axis := func(positive uint64, negative uint64) float32 {
		v := float32(0.0)
		if buttons&positive != 0 {
			v += 1.0
		}
		if buttons&negative != 0 {
			v -= 1.0
		}
		return v
	}
	pd.movementImpulses = [2]float32{axis(IN_FORWARD, IN_BACK), axis(IN_MOVELEFT, IN_MOVERIGHT)}
}

func (pd *PlayerData) CheckSubtickAbuse(cmd *BaseUserCmd) {
	now := pd.currentTime()
	// Verify all button presses/releases are accounted for in subtick moves
	if !verifyCommand(cmd) {
		pd.invalidCommandTimes = append(pd.invalidCommandTimes, now)
		pd.NumInvalidCommands++
	}
	// Check for excessive subtick moves with angles
	if hasExcessiveSubtickMovesWithAngles(cmd) {
		pd.suspiciousSubtickMoveTimes = append(pd.suspiciousSubtickMoveTimes, now)
		pd.NumSubtickSpamCommands++
	}
	// Check for possible desubticked commands
	if len(cmd.SubtickMoves) > 0 {
		pd.numCommandsWithSubtickInputs = append(pd.numCommandsWithSubtickInputs, now)
		pd.NumCommandsWithSubtick++
	}
	if possibleDesubtickedCommand(cmd, pd.movementImpulses) {
		pd.zeroWhenCommandTimes = append(pd.zeroWhenCommandTimes, now)
		pd.NumPossibleDesubtickedCmd++
	}
}

func (pd *PlayerData) CheckSuspiciousSubtickCommands(ctx *detectionContext) {
	if pd.timeInServer() < subtickInitialIgnoreTime {
		// Don't check for the first 10 seconds to avoid false positives on connect... just in case.
		return
	}
	currentTime := pd.currentTime()

	// Clear out invalid commands within configured window
	pd.invalidCommandTimes = purgeOlderThan(pd.invalidCommandTimes, currentTime, subtickInvalidCommandWindow)
	if len(pd.invalidCommandTimes) >= subtickInvalidCommandThreshold {
		pd.MarkInfraction(InfractionInvalidInput, ctx.tick,
			fmt.Sprintf("Excessive invalid commands detected (%d within %.1fs)", len(pd.invalidCommandTimes), subtickInvalidCommandWindow), ctx.verbose)
		pd.invalidCommandTimes = nil
	}

	// Clear out old entries beyond configured window
	pd.suspiciousSubtickMoveTimes = purgeOlderThan(pd.suspiciousSubtickMoveTimes, currentTime, subtickSuspiciousMovesWindow)
	// If there are too many within the timeframe, ban
	if len(pd.suspiciousSubtickMoveTimes) >= subtickSuspiciousMovesThreshold {
		pd.MarkInfraction(InfractionSubtickSpam, ctx.tick,
			fmt.Sprintf("Excessive subtick moves detected (%d within %.1fs)", len(pd.suspiciousSubtickMoveTimes), subtickSuspiciousMovesWindow), ctx.verbose)
		pd.suspiciousSubtickMoveTimes = nil
	}

	// Clear out old entries beyond configured window
	pd.numCommandsWithSubtickInputs = purgeOlderThan(pd.numCommandsWithSubtickInputs, currentTime, subtickSubtickInputsWindow)
	pd.zeroWhenCommandTimes = purgeOlderThan(pd.zeroWhenCommandTimes, currentTime, subtickSubtickInputsWindow)
	// If the vast majority of commands in the timeframe have subtick inputs with zero 'when', kick.
	if len(pd.numCommandsWithSubtickInputs) >= subtickSubtickInputsThreshold {
		ratio := float32(len(pd.zeroWhenCommandTimes)) / float32(len(pd.numCommandsWithSubtickInputs))
		if ratio >= subtickZeroWhenRatioThreshold {
			pd.MarkInfraction(InfractionDesubtick, ctx.tick,
				fmt.Sprintf("Desubticking detected (%d/%d commands, %.1f%%)", len(pd.zeroWhenCommandTimes), len(pd.numCommandsWithSubtickInputs), ratio*100.0), ctx.verbose)
			pd.zeroWhenCommandTimes = nil
			pd.numCommandsWithSubtickInputs = nil
		}
	}
}

func purgeOlderThan(times []float32, now float32, window float32) []float32 {
	i := 0
	for i < len(times) && now-times[i] > window {
		i++
	}
	return times[i:]
}

func abs32(v float32) float32 {
	return float32(math.Abs(float64(v)))
}
