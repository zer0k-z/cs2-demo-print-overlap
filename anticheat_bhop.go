package main

import (
	"fmt"

	st "github.com/markus-wa/demoinfocs-golang/v6/pkg/demoinfocs/sendtables"
)

// Port of cs2kz-metamod src/kz/anticheat/detectors/bhop.cpp
// Detects BhopHack and Hyperscroll infractions.
//
// A bhop attempt is added when the player lands and:
// 1. The player spent at least 4 ticks in the air before landing. This is to avoid cases where weird collisions cause false positives.
// 2. The player did not teleport or noclip in the last 4 ticks. This is to avoid false positives from telehops or leaving noclips.
// 3. sv_autobunnyhopping is off.
//
// The server-side hooks (OnAirMove, OnStartTouchGround, OnJump...) don't exist in a demo, so they are
// reconstructed every tick from the pawn's networked movement services state
// (m_nLastLandedTick/m_flLastLandedFrac and m_nLastJumpTick/m_flLastJumpFrac).

const (
	minAirTicksForBhop    = 4    // Minimum air time to consider a jump for bhop hack detection
	bhopIgnoreTicks       = 4    // Ignore teleports/noclips in the last 4 ticks
	oldJumpPurgeThreshold = 16.0 // Purge jump attempts older than 0.25s (in commands)
	minSampleCount        = 20   // Minimum number of samples before we start checking for bhop hacks
	windowSize            = 30   // Number of recent jumps to consider for bhop hack detection
	// Number of consecutive perfect bhops in the window to trigger a bhop hack infraction, regardless of ratio
	numConsecutivePerfsForInfraction = 25
	// Number of consecutive perfs in the window to trigger a pattern check for bhop hack detection, regardless of ratio.
	numConsecutivePerfsForPatternCheck = 18
	perfRatioForHyperscrollInfraction  = 0.6

	repetitivePatternThreshold = 0.9 // If 90% of the perfs are the same pattern, it might be a cheat...
	lowPatternThreshold        = 4   // ...if the most common pattern is smaller than 4.
	highPatternThreshold       = 16.0
)

const (
	moveTypeWalk = 2
	flOnGround   = 1
	// Anything moving further than sv_maxvelocity allows in one tick (plus some slack) got teleported.
	teleportDistancePerTick = 3500.0/engineFixedTickRate + 32.0
)

type LandingEvent struct {
	Time          float32 // Server tick
	HasPerfect    bool
	NumJumpBefore uint32
	NumJumpAfter  uint32
	// In the original this depends on sv_jump_spam_penalty_time >= tick interval. The default (0.015625) satisfies it.
	ShouldCountTowardsPerfChains bool
	// The recorded landing this event is for, so a later jump only counts towards the right one.
	landedTick int64
	landedFrac float32
}

func (e LandingEvent) String() string {
	perf := " "
	if e.HasPerfect {
		perf = "*"
	}
	return fmt.Sprintf("(%d%s%d)", e.NumJumpBefore, perf, e.NumJumpAfter)
}

type pawnMovementState struct {
	initialized           bool
	onGround              bool
	moveType              uint64
	lastLandedTick        int64
	lastLandedFrac        float32
	lastJumpTick          int64
	lastJumpFrac          float32
	lastJumpPressTick     int64
	lastJumpPressFrac     float32
	origin                [3]float64
	tick                  int
	currentAirTicks       int
	lastValidMoveTypeTick int
	lastTeleportTick      int
}

type jumpPress struct {
	Time float32
}

// The original works in command numbers. Here everything bhop related is in server ticks instead, since
// landings come from the pawn entity and jump inputs may come from either the commands or the pawn.
// With one command per tick, both are equivalent.
func (pd *PlayerData) ParseCommandForJump(cmd *BaseUserCmd, current float32) {
	if len(cmd.SubtickMoves) == 0 {
		if cmd.Buttons.NewlyPressed()&IN_JUMP != 0 {
			pd.addJumpInput(current, 0.0)
		}
	} else {
		// Assume that the +jump inputs are all legit. Faking +jump's wouldn't really help with anything.
		for _, step := range cmd.SubtickMoves {
			if step.Button == IN_JUMP && step.Pressed {
				pd.addJumpInput(current, step.When)
			}
		}
	}
	pd.purgeOldJumps(current)
}

func (pd *PlayerData) addJumpInput(current float32, when float32) {
	pd.recentJumps = append(pd.recentJumps, jumpPress{Time: current + when})
	// Add to recent landing events
	for _, event := range pd.recentLandingEvents {
		if event.Time+when >= current-oldJumpPurgeThreshold {
			event.NumJumpAfter++
		}
	}
}

func (pd *PlayerData) purgeOldJumps(current float32) {
	kept := pd.recentJumps[:0]
	for _, jump := range pd.recentJumps {
		if current-jump.Time <= oldJumpPurgeThreshold {
			kept = append(kept, jump)
		}
	}
	pd.recentJumps = kept
}

func (pd *PlayerData) createLandEvent(tick int, landedTick int64, landedFrac float32) {
	s := &pd.pawn
	// Check for minimum air time
	if s.currentAirTicks < minAirTicksForBhop {
		return
	}
	// Check for valid movetype
	if s.lastValidMoveTypeTick >= 0 && tick-s.lastValidMoveTypeTick < bhopIgnoreTicks {
		return
	}
	// Check for teleport
	if s.lastTeleportTick >= 0 && tick-s.lastTeleportTick < bhopIgnoreTicks {
		return
	}
	pd.recentLandingEvents = append(pd.recentLandingEvents, &LandingEvent{
		Time:                         float32(tick),
		NumJumpBefore:                uint32(len(pd.recentJumps)),
		ShouldCountTowardsPerfChains: true,
		landedTick:                   landedTick,
		landedFrac:                   landedFrac,
	})
	pd.NumLandingEvents++
}

// Marks the landing a perfect bhop came from, like OnJump does when IsPerfing(true).
func (pd *PlayerData) markPerfect(landedTick int64, landedFrac float32) {
	n := len(pd.recentLandingEvents)
	if n == 0 {
		return
	}
	event := pd.recentLandingEvents[n-1]
	if event.HasPerfect || event.landedTick != landedTick || event.landedFrac != landedFrac {
		return
	}
	event.HasPerfect = true
	pd.NumPerfectBhops++
}

// Called once per tick. Reconstructs OnChangeMoveType, OnAirMove, OnStartTouchGround, OnJump and
// OnProcessMovementPost from the pawn state, then runs CheckLandingEvents.
//
// Whether a jump was perfect comes from the recorded landing and jump times instead of the server's
// IsPerfing(true), see classifyJump. Both count perfect and in-window (restored) bhops, and not deferred
// jumps. m_nLastLandedTick isn't updated by landings on the way up (ledgegrabs) or crouchbugs, so those are
// taken from the ground flag, or from a jump out of the air.
func (pd *PlayerData) OnTick(tick int, pawn st.Entity, ctx *detectionContext) {
	s := &pd.pawn
	if pawn == nil || !pd.pl.IsAlive() {
		s.initialized = false
		s.currentAirTicks = 0
		return
	}

	flags, _ := pawn.PropertyValue("m_fFlags")
	moveType, _ := pawn.PropertyValue("m_MoveType")
	landedTick, _ := pawn.PropertyValue("m_pMovementServices.m_nLastLandedTick")
	landedFrac, _ := pawn.PropertyValue("m_pMovementServices.m_flLastLandedFrac")
	landedVelocityZ, _ := pawn.PropertyValue("m_pMovementServices.m_flLastLandedVelocityZ")
	jumpTick, _ := pawn.PropertyValue("m_pMovementServices.m_nLastJumpTick")
	jumpFrac, _ := pawn.PropertyValue("m_pMovementServices.m_flLastJumpFrac")
	jumpPressTick, _ := pawn.PropertyValue("m_pMovementServices.m_nLastActualJumpPressTick")
	jumpPressFrac, _ := pawn.PropertyValue("m_pMovementServices.m_flLastActualJumpPressFrac")
	pos := pd.pl.Position()

	cur := pawnMovementState{
		onGround:          propUInt64(flags)&flOnGround != 0,
		moveType:          propUInt64(moveType),
		lastLandedTick:    propInt64(landedTick),
		lastLandedFrac:    propFloat(landedFrac),
		lastJumpTick:      propInt64(jumpTick),
		lastJumpFrac:      propFloat(jumpFrac),
		lastJumpPressTick: propInt64(jumpPressTick),
		lastJumpPressFrac: propFloat(jumpPressFrac),
		origin:            [3]float64{pos.X, pos.Y, pos.Z},
		tick:              tick,
	}
	saveState := func() {
		s.onGround, s.moveType = cur.onGround, cur.moveType
		s.lastLandedTick, s.lastLandedFrac = cur.lastLandedTick, cur.lastLandedFrac
		s.lastJumpTick, s.lastJumpFrac = cur.lastJumpTick, cur.lastJumpFrac
		s.lastJumpPressTick, s.lastJumpPressFrac = cur.lastJumpPressTick, cur.lastJumpPressFrac
		s.origin, s.tick = cur.origin, cur.tick
	}

	if !s.initialized {
		s.initialized = true
		saveState()
		s.lastTeleportTick = tick // Spawning counts as a teleport.
		return
	}
	if tick <= s.tick {
		return
	}

	// Jump inputs. When the demo doesn't carry the commands, fall back to the pawn's last jump press. It
	// only holds the latest press, so at most one press per tick is seen, which can undercount scrolling.
	if !pd.hasReliableCommandData(tick) {
		if cur.lastJumpPressTick != s.lastJumpPressTick || cur.lastJumpPressFrac != s.lastJumpPressFrac {
			pd.addJumpInput(float32(tick), 0.0)
		}
		pd.purgeOldJumps(float32(tick))
	}

	// OnChangeMoveType
	if cur.moveType != s.moveType && cur.moveType != moveTypeWalk {
		s.lastValidMoveTypeTick = tick
	}
	// JustTeleported
	dx, dy, dz := cur.origin[0]-s.origin[0], cur.origin[1]-s.origin[1], cur.origin[2]-s.origin[2]
	maxDist := teleportDistancePerTick * float64(tick-s.tick)
	teleported := dx*dx+dy*dy+dz*dz > maxDist*maxDist
	if teleported {
		s.lastTeleportTick = tick
	}

	landed := cur.lastLandedTick != s.lastLandedTick || cur.lastLandedFrac != s.lastLandedFrac
	jumped := cur.lastJumpTick != s.lastJumpTick || cur.lastJumpFrac != s.lastJumpFrac
	walking := s.moveType == moveTypeWalk && cur.moveType == moveTypeWalk

	kind := jumpOther
	if jumped && walking && !teleported {
		kind, _ = classifyJump(cur.lastLandedTick, cur.lastLandedFrac, cur.lastJumpTick, cur.lastJumpFrac, s.onGround, landed)
	}
	// Landings the pawn doesn't record: touching the ground on the way up, or jumping straight back out of it.
	impliedLanding := !landed && walking && !s.onGround && (cur.onGround || kind == jumpNoLanding)
	landingTick, landingFrac := cur.lastLandedTick, cur.lastLandedFrac
	if impliedLanding {
		landingTick, landingFrac = -int64(tick), 0
	}

	// OnProcessMovement + OnAirMove: the player air moved this tick if they started it in the air, or jumped during it.
	airMoved := (!s.onGround && s.moveType == moveTypeWalk) || jumped
	if airMoved {
		s.currentAirTicks++
	}
	if landed || impliedLanding {
		pd.createLandEvent(tick, landingTick, landingFrac)
	}
	if jumped {
		if kind.countsAsPerf() {
			pd.markPerfect(landingTick, landingFrac)
		}
		pd.recordJump(kind, cur.lastJumpTick, cur.lastJumpFrac, pd.pressHeightFunc(ctx, s, &cur, propFloat(landedVelocityZ)))
	}
	// OnProcessMovementPost
	if !airMoved {
		s.currentAirTicks = 0
	}

	saveState()

	pd.CheckLandingEvents(ctx)
}

// How high above the floor the player was when a perfect bhop press grounded them. The press time is the
// recorded landing time, and the player's height then follows from the position at the start of the tick
// and the landing speed under gravity.
func (pd *PlayerData) pressHeightFunc(ctx *detectionContext, prev *pawnMovementState, cur *pawnMovementState, landedVelocityZ float32) func() (float64, float64, bool) {
	if ctx.geometry == nil || prev.tick != cur.tick-1 || landedVelocityZ >= 0 {
		return nil
	}
	// Copy the start of the tick, prev gets overwritten with this tick's state before the closure runs.
	start := prev.origin
	end := cur.origin
	frac := float64(cur.lastLandedFrac)
	return func() (float64, float64, bool) {
		dt := frac / engineFixedTickRate
		vz := float64(landedVelocityZ)
		x := start[0] + (end[0]-start[0])*frac
		y := start[1] + (end[1]-start[1])*frac
		z := start[2] + vz*dt + 0.5*gravity*dt*dt
		floor, ok := ctx.geometry.FloorBelow(x, y, z+0.5)
		if !ok {
			return 0, 0, false
		}
		return z - floor, -vz, true
	}
}

func (pd *PlayerData) CheckLandingEvents(ctx *detectionContext) {
	if len(pd.recentLandingEvents) > windowSize {
		pd.recentLandingEvents = pd.recentLandingEvents[len(pd.recentLandingEvents)-windowSize:]
	}
	if len(pd.recentLandingEvents) < minSampleCount {
		return
	}
	numPerfs := 0
	totalChainEligibleEvents := 0
	maxPerfChain := 0
	currentPerfChain := 0
	patterns := make(map[uint32]int)
	mostCommonPattern := uint32(0)
	mostCommonPatternCount := 0
	totalPatternOccurrences := 0
	weightedPatternSum := uint32(0)
	for _, event := range pd.recentLandingEvents {
		if event.NumJumpAfter > 0 || event.NumJumpBefore > 0 {
			pattern := event.NumJumpBefore + event.NumJumpAfter
			patterns[pattern]++
			count := patterns[pattern]
			totalPatternOccurrences++
			weightedPatternSum += pattern
			if count > mostCommonPatternCount {
				mostCommonPatternCount = count
				mostCommonPattern = pattern
			}
		}
		if !event.ShouldCountTowardsPerfChains {
			continue
		}
		totalChainEligibleEvents++
		if event.HasPerfect {
			numPerfs++
			currentPerfChain++
			maxPerfChain = max(maxPerfChain, currentPerfChain)
		} else {
			currentPerfChain = 0
		}
	}
	averagePattern := float32(0.0)
	if totalPatternOccurrences > 0 {
		averagePattern = float32(weightedPatternSum) / float32(totalPatternOccurrences)
	}
	pd.PeakPerfChain = max(pd.PeakPerfChain, maxPerfChain)
	pd.PeakAveragePattern = max(pd.PeakAveragePattern, averagePattern)
	if totalChainEligibleEvents > 0 {
		pd.PeakPerfRatio = max(pd.PeakPerfRatio, float32(numPerfs)/float32(totalChainEligibleEvents))
	}

	flag := func(t InfractionType, details string) {
		pd.MarkInfraction(t, ctx.tick, fmt.Sprintf("%s %s", details, pd.landingEventsString()), ctx.verbose)
		// A banned player stops being checked, so start over to look for the next independent detection.
		pd.recentLandingEvents = nil
		pd.recentJumps = nil
	}

	// Hard consecutive perf chain check.
	if maxPerfChain >= numConsecutivePerfsForInfraction {
		flag(InfractionBhopHack, fmt.Sprintf("%d/%d consecutive perfect bhops", maxPerfChain, totalChainEligibleEvents))
		return
	}

	// Pattern-based bhop infraction after medium chain threshold.
	if maxPerfChain >= numConsecutivePerfsForPatternCheck {
		if totalPatternOccurrences > 0 && float32(mostCommonPatternCount) >= float32(totalPatternOccurrences)*repetitivePatternThreshold &&
			mostCommonPattern < lowPatternThreshold {
			flag(InfractionBhopHack, fmt.Sprintf("%d/%d occurrences of pattern %d (avg pattern %.2f)", mostCommonPatternCount,
				totalPatternOccurrences, mostCommonPattern, averagePattern))
			return
		}
	}

	// Hyperscroll check
	perfectRatio := float32(0.0)
	if totalChainEligibleEvents > 0 {
		perfectRatio = float32(numPerfs) / float32(totalChainEligibleEvents)
	}
	if averagePattern >= highPatternThreshold && perfectRatio > perfRatioForHyperscrollInfraction && totalChainEligibleEvents >= minSampleCount {
		flag(InfractionHyperscroll, fmt.Sprintf("Average pattern %.2f >= %.2f with %.2f%% perfect ratio (%d/%d)", averagePattern, highPatternThreshold,
			perfectRatio*100.0, numPerfs, totalChainEligibleEvents))
		return
	}
}

func (pd *PlayerData) landingEventsString() string {
	s := ""
	for _, e := range pd.recentLandingEvents {
		s += e.String()
	}
	return s
}

func propUInt64(v st.PropertyValue) uint64 {
	if v.Any == nil {
		return 0
	}
	switch x := v.Any.(type) {
	case uint64:
		return x
	case uint32:
		return uint64(x)
	case int32:
		return uint64(x)
	case int64:
		return uint64(x)
	}
	return v.UInt64()
}

func propInt64(v st.PropertyValue) int64 {
	if v.Any == nil {
		return 0
	}
	switch x := v.Any.(type) {
	case int32:
		return int64(x)
	case int64:
		return x
	case uint32:
		return int64(x)
	case uint64:
		return int64(x)
	}
	return int64(v.Int())
}

func propFloat(v st.PropertyValue) float32 {
	if v.Any == nil {
		return 0
	}
	if f, ok := v.Any.(float32); ok {
		return f
	}
	return v.Float()
}
