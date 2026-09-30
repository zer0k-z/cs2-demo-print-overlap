package main

import (
	"math"
	"sort"
)

// Per-jump analysis, based on how CS2 handles jumps around landings (see "Bunnyhopping in CS2"):
//
//   - A press within the last 2 units above the floor grounds the player at the press, records the
//     landing at the press time and jumps right away: a perfect bhop, jump time == landing time.
//   - A press within sv_bhop_time_window / 2 (0.25 tick) after the landing keeps the player's speed,
//     either as a perfect bhop (no WalkMove ran in between) or a restored one.
//   - Anything later, or any landing below max speed, jumps exactly one tick after the landing (deferred).
//
// The landing and jump times come from the pawn's m_nLastLandedTick/Frac and m_nLastJumpTick/Frac. Those
// ticks count 2 per game tick, while the fractions are fractions of a game tick.

const (
	movementTickScale = 2
	bhopWindowHalf    = 0.25 // sv_bhop_time_window / 2, in ticks
	fracEpsilon       = 1e-4
	gravity           = 800.0
	// A normal jump is one that happens this long after the last landing, from the ground.
	normalJumpMinTicks = 2
)

type jumpKind int

const (
	jumpPerfect   jumpKind = iota // jump time == landing time
	jumpInWindow                  // within 0.25 tick after the landing, speed kept
	jumpDeferred                  // exactly one tick after the landing
	jumpNoLanding                 // from the air without a recorded landing: jumpbugs and ledgegrabs
	jumpNormal                    // from the ground, well after the landing
	jumpOther
)

func (k jumpKind) isBhop() bool {
	return k == jumpPerfect || k == jumpInWindow || k == jumpDeferred || k == jumpNoLanding
}

// Whether the jump kept the player's speed the way the server's bhop detection sees it.
func (k jumpKind) countsAsPerf() bool {
	return k == jumpPerfect || k == jumpInWindow || k == jumpNoLanding
}

type JumpStats struct {
	Perfect, InWindow, Deferred, NoLanding int
	// Bhop jumps by the jump input that caused them.
	ScrollBhops, KeyBhops, UnknownBhops int
	ScrollPerfect, KeyPerfect           int
	// Perfect bhops with a subtick jump press at the jump time, i.e. not desubticked.
	SubtickPerfect int
	// Normal jumps, and how many of them happened exactly at the start of a tick. A jump without a subtick
	// move is processed at the start of the tick, so a client that doesn't send subtick moves always
	// jumps there. Legitimate jumps land anywhere in the tick.
	Normal, NormalAtTickStart int
	PressHeights              PressHeightStats
}

func (s JumpStats) BhopJumps() int {
	return s.Perfect + s.InWindow + s.Deferred + s.NoLanding
}

// Share of normal jumps at the start of a tick above which the jumps are considered desubticked.
const (
	desubtickedJumpsMinSamples = 20
	desubtickedJumpsThreshold  = 0.8
)

func (s JumpStats) DesubtickedShare() float64 {
	if s.Normal == 0 {
		return 0
	}
	return float64(s.NormalAtTickStart) / float64(s.Normal)
}

func (s JumpStats) DesubtickedJumpsFlagged() bool {
	return s.Normal >= desubtickedJumpsMinSamples && s.DesubtickedShare() >= desubtickedJumpsThreshold
}

// Where in the last 2 units above the floor each perfect bhop's press happened. A human pressing somewhere
// in that range lands anywhere in it, while an input generated from the player's position tends to land
// right as the player gets within 2 units.
type PressHeightStats struct {
	Heights   []float64 // units above the floor at the press
	IntoMs    []float64 // ms since the player got within 2 units
	BandShare []float64 // IntoMs / time spent in the last 2 units: 0 = just got within 2 units, 1 = contact
	Outliers  int       // Heights outside the 2 unit range (ducked, landing on players or props...)
	NoFloor   int
}

const (
	pressHeightMinSamples   = 20
	pressHeightEarlyShare   = 0.2 // First fifth of the last 2 units
	pressHeightEarlyFlagged = 0.6 // Share of presses there that gets flagged; about 20% is expected
)

func (s PressHeightStats) EarlyShare() float64 {
	if len(s.BandShare) == 0 {
		return 0
	}
	early := 0
	for _, r := range s.BandShare {
		if r <= pressHeightEarlyShare {
			early++
		}
	}
	return float64(early) / float64(len(s.BandShare))
}

func (s PressHeightStats) Flagged() bool {
	return len(s.BandShare) >= pressHeightMinSamples && s.EarlyShare() >= pressHeightEarlyFlagged
}

func (s *PressHeightStats) Sorted() (heights, into []float64) {
	heights = append([]float64(nil), s.Heights...)
	into = append([]float64(nil), s.IntoMs...)
	sort.Float64s(heights)
	sort.Float64s(into)
	return heights, into
}

// Adds a perfect bhop press at hovering height h above the floor, when landing at fallSpeed.
func (s *PressHeightStats) Add(h, fallSpeed float64) {
	if h < -0.25 || h > 2.25 {
		s.Outliers++
		return
	}
	h = math.Min(math.Max(h, 0), 2)
	// Speed when the player got within 2 units, and the time from there to the floor.
	v2 := math.Sqrt(math.Max(0, fallSpeed*fallSpeed-4*gravity))
	band := (fallSpeed - v2) / gravity
	fallen := 2 - h
	into := (-v2 + math.Sqrt(v2*v2+2*gravity*fallen)) / gravity
	s.Heights = append(s.Heights, h)
	s.IntoMs = append(s.IntoMs, into*1000)
	if band > 0 {
		s.BandShare = append(s.BandShare, into/band)
	}
}

// Recent commands, to find the one a jump came from.
const recentCommandCount = 16

type recentCommand struct {
	serverTick int32
	jumpState  uint8
	pressWhens []float32
}

func (pd *PlayerData) rememberCommand(cmd *BaseUserCmd) {
	rc := recentCommand{serverTick: cmd.ServerTick, jumpState: buttonState(cmd.Buttons, IN_JUMP)}
	for _, step := range cmd.SubtickMoves {
		if step.HasButton && step.Button == IN_JUMP && step.Pressed {
			rc.pressWhens = append(rc.pressWhens, step.When)
		}
	}
	pd.recentCommands[pd.recentCommandIndex%recentCommandCount] = rc
	pd.recentCommandIndex++
}

// The command a jump at jumpTick + jumpFrac came from. Prefers a command with a subtick press at the jump
// time; the server tick of the command and of the jump can be one apart.
func (pd *PlayerData) commandForJump(jumpServerTick int32, jumpFrac float32) (recentCommand, bool, bool) {
	var fallback *recentCommand
	for _, dt := range []int32{0, 1, -1} {
		for i := range pd.recentCommands {
			rc := &pd.recentCommands[i]
			if rc.serverTick == 0 || rc.serverTick != jumpServerTick+dt {
				continue
			}
			for _, when := range rc.pressWhens {
				if math.Abs(float64(when-jumpFrac)) < 1.0/128 {
					return *rc, true, true
				}
			}
			if fallback == nil && dt != -1 {
				fallback = rc
			}
		}
	}
	if fallback != nil {
		return *fallback, false, true
	}
	return recentCommand{}, false, false
}

func classifyJump(landedTick int64, landedFrac float32, jumpTick int64, jumpFrac float32, wasOnGround bool, landedThisTick bool) (jumpKind, float64) {
	delta := float64(jumpTick-landedTick)/movementTickScale + float64(jumpFrac-landedFrac)
	switch {
	case math.Abs(delta) < fracEpsilon:
		return jumpPerfect, delta
	case delta > 0 && delta <= bhopWindowHalf+fracEpsilon:
		return jumpInWindow, delta
	case math.Abs(delta-1) <= 1.0/64+fracEpsilon:
		return jumpDeferred, delta
	case !wasOnGround && !landedThisTick:
		return jumpNoLanding, delta
	case wasOnGround && delta >= normalJumpMinTicks:
		return jumpNormal, delta
	}
	return jumpOther, delta
}

// Records a jump seen on the pawn this tick.
func (pd *PlayerData) recordJump(kind jumpKind, jumpTick int64, jumpFrac float32, pressHeight func() (float64, float64, bool)) {
	s := &pd.Jumps
	switch kind {
	case jumpNormal:
		s.Normal++
		if jumpFrac == 0 {
			s.NormalAtTickStart++
		}
		return
	case jumpOther:
		return
	case jumpPerfect:
		s.Perfect++
	case jumpInWindow:
		s.InWindow++
	case jumpDeferred:
		s.Deferred++
	case jumpNoLanding:
		s.NoLanding++
	}

	cmd, hasPress, found := pd.commandForJump(int32(jumpTick/movementTickScale), jumpFrac)
	switch {
	case !found:
		s.UnknownBhops++
	case isScrollState(cmd.jumpState):
		s.ScrollBhops++
		if kind == jumpPerfect {
			s.ScrollPerfect++
		}
	default:
		s.KeyBhops++
		if kind == jumpPerfect {
			s.KeyPerfect++
		}
	}

	if kind != jumpPerfect {
		return
	}
	if hasPress {
		s.SubtickPerfect++
	}
	// Presses at the start of the tick are desubticked jumps, which land wherever the tick boundary
	// happens to be. Only subtick timed presses say something about how the press was timed.
	if jumpFrac == 0 || pressHeight == nil {
		return
	}
	h, fallSpeed, ok := pressHeight()
	if !ok {
		s.PressHeights.NoFloor++
		return
	}
	s.PressHeights.Add(h, fallSpeed)
}
