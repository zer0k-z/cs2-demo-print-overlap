package main

import (
	"testing"

	common "github.com/markus-wa/demoinfocs-golang/v6/pkg/demoinfocs/common"
)

func newTestPlayer() *PlayerData {
	pd := &PlayerData{pl: &common.Player{Name: "test"}}
	pd.pawn.lastValidMoveTypeTick = -1
	pd.pawn.lastTeleportTick = -1
	return pd
}

// Feeds one command per tick, starting late enough to be past the initial ignore time.
func feedCommands(pd *PlayerData, n int, makeCmd func(i int) BaseUserCmd) {
	const start = 10000
	for i := 0; i < n; i++ {
		ctx := &detectionContext{tick: start + i}
		pd.OnUserCmd(int32(start+i), makeCmd(i), ctx)
	}
}

func countInfractions(pd *PlayerData, t InfractionType) int {
	return len(pd.InfractionsOfType(t))
}

func TestLegitCommandsAreClean(t *testing.T) {
	pd := newTestPlayer()
	feedCommands(pd, 64*60, func(i int) BaseUserCmd {
		cmd := BaseUserCmd{}
		// Tap W every 8 ticks at a random-ish subtick fraction, and jump every 64 ticks.
		switch i % 8 {
		case 0:
			cmd.SubtickMoves = []SubtickMoveStep{{Button: IN_FORWARD, HasButton: true, Pressed: true, When: 0.37}}
		case 4:
			cmd.SubtickMoves = []SubtickMoveStep{{Button: IN_FORWARD, HasButton: true, Pressed: false, When: 0.81}}
		}
		if i%64 == 0 {
			cmd.Buttons.ButtonState2 = IN_JUMP
			cmd.SubtickMoves = append(cmd.SubtickMoves, SubtickMoveStep{Button: IN_JUMP, HasButton: true, Pressed: true, When: 0.9})
		}
		return cmd
	})
	if len(pd.Infractions) != 0 {
		t.Fatalf("expected no infractions, got %+v", pd.Infractions)
	}
}

func TestInvalidInput(t *testing.T) {
	pd := newTestPlayer()
	// Jump presses that never show up in the subtick moves.
	feedCommands(pd, 64*20, func(i int) BaseUserCmd {
		cmd := BaseUserCmd{}
		if i%16 == 0 {
			cmd.Buttons.ButtonState2 = IN_JUMP
		}
		return cmd
	})
	if countInfractions(pd, InfractionInvalidInput) == 0 {
		t.Fatal("expected an Invalid Input infraction")
	}
}

func TestSubtickSpam(t *testing.T) {
	pd := newTestPlayer()
	feedCommands(pd, 64*20, func(i int) BaseUserCmd {
		cmd := BaseUserCmd{}
		for j := 0; j < 5; j++ {
			cmd.SubtickMoves = append(cmd.SubtickMoves, SubtickMoveStep{When: float32(j) * 0.2, YawDelta: 0.5, HasYawDelta: true})
		}
		return cmd
	})
	if countInfractions(pd, InfractionSubtickSpam) == 0 {
		t.Fatal("expected a Subtick Spam infraction")
	}
}

func TestDesubtick(t *testing.T) {
	pd := newTestPlayer()
	// Every key press lands exactly at the start of the tick.
	feedCommands(pd, 64*20, func(i int) BaseUserCmd {
		cmd := BaseUserCmd{}
		switch i % 8 {
		case 0:
			cmd.SubtickMoves = []SubtickMoveStep{{Button: IN_MOVELEFT, HasButton: true, Pressed: true}}
		case 4:
			cmd.SubtickMoves = []SubtickMoveStep{{Button: IN_MOVELEFT, HasButton: true, Pressed: false}}
		}
		return cmd
	})
	if countInfractions(pd, InfractionDesubtick) == 0 {
		t.Fatal("expected a Desubticking infraction")
	}
}

func TestDesubtickImpulseLimit(t *testing.T) {
	pd := newTestPlayer()
	// `forwardback 5 0 0` style analog deltas push the axis past what any input can reach.
	feedCommands(pd, 64*20, func(i int) BaseUserCmd {
		cmd := BaseUserCmd{}
		if i%4 == 0 {
			cmd.SubtickMoves = []SubtickMoveStep{{When: 0.5, AnalogForwardDelta: 5.0, HasAnalogForwardDelta: true}}
		}
		return cmd
	})
	if countInfractions(pd, InfractionDesubtick) == 0 {
		t.Fatal("expected a Desubticking infraction")
	}
}

func TestControllerIsNotDesubtick(t *testing.T) {
	pd := newTestPlayer()
	feedCommands(pd, 64*20, func(i int) BaseUserCmd {
		cmd := BaseUserCmd{}
		delta := float32(0.013)
		if (i/16)%2 == 1 {
			delta = -delta
		}
		cmd.SubtickMoves = []SubtickMoveStep{{AnalogForwardDelta: delta, HasAnalogForwardDelta: true}}
		return cmd
	})
	if countInfractions(pd, InfractionDesubtick) != 0 {
		t.Fatalf("expected no Desubticking infraction, got %+v", pd.Infractions)
	}
}

// Landing events are fed straight into the window, bypassing the pawn state reconstruction.
func addLanding(pd *PlayerData, tick int, perfect bool, jumpsBefore uint32, jumpsAfter uint32) {
	pd.recentLandingEvents = append(pd.recentLandingEvents, &LandingEvent{
		Time:                         float32(tick),
		HasPerfect:                   perfect,
		NumJumpBefore:                jumpsBefore,
		NumJumpAfter:                 jumpsAfter,
		ShouldCountTowardsPerfChains: true,
	})
	pd.CheckLandingEvents(&detectionContext{tick: tick})
}

func TestBhopHackConsecutivePerfs(t *testing.T) {
	pd := newTestPlayer()
	for i := 0; i < 30; i++ {
		addLanding(pd, i*64, true, 1, 0)
	}
	if countInfractions(pd, InfractionBhopHack) == 0 {
		t.Fatal("expected a Bhop Hack infraction")
	}
}

func TestBhopHackPattern(t *testing.T) {
	pd := newTestPlayer()
	// 19 consecutive perfs, each from a single jump press.
	for i := 0; i < 20; i++ {
		addLanding(pd, i*64, i > 0, 1, 0)
	}
	if countInfractions(pd, InfractionBhopHack) == 0 {
		t.Fatal("expected a Bhop Hack infraction")
	}
}

func TestHyperscroll(t *testing.T) {
	pd := newTestPlayer()
	// Alternating perfs, 20 scroll inputs around every landing.
	for i := 0; i < 30; i++ {
		addLanding(pd, i*64, i%3 != 0, 10, 10)
	}
	if countInfractions(pd, InfractionHyperscroll) == 0 {
		t.Fatal("expected a Hyperscroll infraction")
	}
	if countInfractions(pd, InfractionBhopHack) != 0 {
		t.Fatal("did not expect a Bhop Hack infraction")
	}
}

func TestLegitBhops(t *testing.T) {
	pd := newTestPlayer()
	// Half perfs with a normal amount of scrolling.
	for i := 0; i < 100; i++ {
		addLanding(pd, i*64, i%2 == 0, 3, 2)
	}
	if len(pd.Infractions) != 0 {
		t.Fatalf("expected no infractions, got %+v", pd.Infractions)
	}
}
