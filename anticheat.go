package main

import (
	"fmt"

	common "github.com/markus-wa/demoinfocs-golang/v6/pkg/demoinfocs/common"
)

// Port of a subset of cs2kz-metamod's anticheat (src/kz/anticheat), adapted to run on demo data.

const (
	engineFixedTickRate             = 64.0
	engineFixedTickInterval float32 = 1.0 / engineFixedTickRate
)

const (
	IN_JUMP      = uint64(0x2)
	IN_DUCK      = uint64(0x4)
	IN_FORWARD   = uint64(0x8)
	IN_BACK      = uint64(0x10)
	IN_MOVELEFT  = uint64(0x200)
	IN_MOVERIGHT = uint64(0x400)
	IN_ATTACK    = uint64(0x1)
	IN_ATTACK2   = uint64(0x800)
)

type InfractionType int

const (
	InfractionBhopHack InfractionType = iota
	InfractionHyperscroll
	InfractionInvalidInput
	InfractionSubtickSpam
	InfractionDesubtick
	InfractionCount
)

var infractionNames = [InfractionCount]string{
	"Bhop Hack",
	"Hyperscroll",
	"Invalid Input",
	"Subtick Spam",
	"Desubticking",
}

func (t InfractionType) String() string {
	return infractionNames[t]
}

type Infraction struct {
	Type    InfractionType
	Tick    int
	Details string
}

type PlayerData struct {
	pl *common.Player

	NumCommands        int
	NumIgnoredCommands int

	// Counters behind the ported subtick checks, useful to see how close a player got.
	NumInvalidCommands        int
	NumSubtickSpamCommands    int
	NumCommandsWithSubtick    int
	NumPossibleDesubtickedCmd int
	// Landings and perfs as the ported bhop check sees them.
	NumLandingEvents int
	NumPerfectBhops  int
	// Highest values seen by the bhop check across all of its (>= minSampleCount) windows.
	PeakPerfChain      int
	PeakPerfRatio      float32
	PeakAveragePattern float32

	Infractions []Infraction

	// Checks that aren't part of cs2kz.
	Jumps  JumpStats
	Inputs InputStats
	Frames FrameStats

	inputs             inputState
	recentCommands     [recentCommandCount]recentCommand
	recentCommandIndex int

	// Command timing. Detection windows are measured in command numbers, where one command is one tick.
	currentCmdNum    int32
	firstCmdNum      int32
	hasCmdNum        bool
	lastCmdTick      int
	movementImpulses [2]float32 // Emulated m_vecLastMovementImpulses (forward, side)

	// Subtick abuses
	suspiciousSubtickMoveTimes   []float32
	invalidCommandTimes          []float32
	zeroWhenCommandTimes         []float32
	numCommandsWithSubtickInputs []float32

	// Bhop / Hyperscroll
	recentJumps         []jumpPress
	recentLandingEvents []*LandingEvent
	pawn                pawnMovementState
}

func (pd *PlayerData) currentTime() float32 {
	return float32(pd.currentCmdNum) * engineFixedTickInterval
}

// Seconds since we first saw a command from this player.
func (pd *PlayerData) timeInServer() float32 {
	return float32(pd.currentCmdNum-pd.firstCmdNum) * engineFixedTickInterval
}

func (pd *PlayerData) MarkInfraction(t InfractionType, tick int, details string, verbose bool) {
	pd.Infractions = append(pd.Infractions, Infraction{Type: t, Tick: tick, Details: details})
	if verbose {
		fmt.Printf("[%s] %s (%d) @ tick %d: %s\n", t, pd.pl.Name, pd.pl.SteamID64, tick, details)
	}
}

func (pd *PlayerData) InfractionsOfType(t InfractionType) []Infraction {
	var out []Infraction
	for _, inf := range pd.Infractions {
		if inf.Type == t {
			out = append(out, inf)
		}
	}
	return out
}

// How long the commands can go missing before jump inputs are read from the pawn's networked jump
// press state instead.
const maxCommandGapForCommandJumps = 8

// Whether jump inputs currently come from the commands. Demos that don't record the commands, or
// stretches where they are missing, fall back to the pawn.
func (pd *PlayerData) hasReliableCommandData(tick int) bool {
	return pd.hasCmdNum && tick-pd.lastCmdTick <= maxCommandGapForCommandJumps
}

// Called for every usercmd of the player, mirrors KZAnticheatService::OnSetupMove.
func (pd *PlayerData) OnUserCmd(cmdNum int32, cmd BaseUserCmd, ctx *detectionContext) {
	// Commands get resent while the server is waiting on the client, only look at each one once.
	if pd.hasCmdNum && cmdNum == pd.currentCmdNum {
		return
	}
	// A reconnect restarts the command numbers, so start over.
	if pd.hasCmdNum && cmdNum < pd.currentCmdNum {
		pd.ClearDetectionBuffers()
		pd.hasCmdNum = false
	}
	if !pd.hasCmdNum {
		pd.firstCmdNum = cmdNum
		pd.hasCmdNum = true
	}
	pd.currentCmdNum = cmdNum
	pd.lastCmdTick = ctx.tick
	pd.NumCommands++
	// Commands the server ignored never reach SetupMove.
	if cmd.HasExecutionNotes {
		pd.NumIgnoredCommands++
		return
	}

	pd.Frames.Add(cmd.FrameTimes)
	pd.CheckInputConsistency(cmdNum, &cmd)
	pd.rememberCommand(&cmd)

	pd.CheckSubtickAbuse(&cmd)
	pd.UpdateMovementImpulses(&cmd)
	pd.ParseCommandForJump(&cmd, float32(ctx.tick))
	// Mirrors KZAnticheatService::OnPhysicsSimulatePost. The bhop check runs on every tick instead,
	// since landings are only known once the pawn entity updates.
	pd.CheckSuspiciousSubtickCommands(ctx)
}

func (pd *PlayerData) ClearDetectionBuffers() {
	pd.suspiciousSubtickMoveTimes = nil
	pd.invalidCommandTimes = nil
	pd.zeroWhenCommandTimes = nil
	pd.numCommandsWithSubtickInputs = nil
	pd.recentJumps = nil
	pd.recentLandingEvents = nil
	pd.pawn.currentAirTicks = 0
	pd.pawn.lastValidMoveTypeTick = -1
}

type detectionContext struct {
	tick     int
	verbose  bool
	debug    bool
	geometry *Geometry // nil when the map's collision isn't available
}
