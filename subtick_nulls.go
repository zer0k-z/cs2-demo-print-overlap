package main

import (
	"fmt"
	"math"
	"sort"

	demoinfocs "github.com/markus-wa/demoinfocs-golang/v5/pkg/demoinfocs"
	msg "github.com/markus-wa/demoinfocs-golang/v5/pkg/demoinfocs/msg"
	"google.golang.org/protobuf/encoding/protowire"
)

const (
	numMinInputEventsForDetection                       = 128
	numConsecutivePerfectCStrafeForDetectionMin         = 128
	numConsecutivePerfectCStrafeForDetectionMax         = 640
	analogCStrafeWeight                                 = 2.0
	underlapCountThreshold                              = 0.2
	underlapPercentageThreshold                         = 0.1
	underlapMedianForgivenessThreshold                  = 0.02
	engineFixedTickInterval                     float32 = 1.0 / 64.0
)

type InputEvent struct {
	CmdNum   int32
	Fraction float32
	Button   uint64
	Pressed  bool
	Analog   bool
}

type subtickMoveStep struct {
	Button                uint64
	Pressed               bool
	When                  float32
	AnalogForwardDelta    float32
	AnalogLeftDelta       float32
	HasButton             bool
	HasPressed            bool
	HasWhen               bool
	HasAnalogForwardDelta bool
	HasAnalogLeftDelta    bool
}

type decodedBaseUserCmd struct {
	SubtickMoves []subtickMoveStep
}

type NullAxisMetrics struct {
	Detected           bool
	Status             string
	ConsecutivePerfect float64
	RequiredPerfect    float64
	Perfect            float64
	Overlaps           float64
	UnderlapCount      int
	UnderlapMinMs      float64
	UnderlapMaxMs      float64
	UnderlapMeanMs     float64
	UnderlapMedianMs   float64
	UnderlapP90Ms      float64
	UnderlapPct        float64
	Samples            int
}

func registerSubtickUserCmdHandler(p demoinfocs.Parser, resolveMoveData func(playerSlot int32) *MoveData) {
	p.RegisterNetMessageHandler(func(m *msg.CSVCMsg_UserCommands) {
		for _, cmd := range m.GetCommands() {
			mv := resolveMoveData(cmd.GetPlayerSlot())
			if mv == nil || !mv.Tracking {
				continue
			}
			ingestSubtickUserCmd(mv, cmd.GetCmdNumber(), cmd.GetData())
		}
	})
}

func ingestSubtickUserCmd(mv *MoveData, cmdNum int32, payload []byte) {
	decoded, err := decodeBaseUserCmd(payload)
	if err != nil {
		mv.SubtickDecodeErrors++
		return
	}
	if len(decoded.SubtickMoves) == 0 {
		return
	}

	mv.HasSubtickData = true
	oldButtons := mv.SubtickButtons

	recordEvent := func(button uint64, pressed bool, analog bool, fraction float32) {
		e := InputEvent{
			CmdNum:   cmdNum,
			Fraction: fraction,
			Button:   button,
			Pressed:  pressed,
			Analog:   analog,
		}
		if button == IN_FORWARD || button == IN_BACK {
			mv.RecentForwardBackwardEvents = append(mv.RecentForwardBackwardEvents, e)
		} else {
			mv.RecentLeftRightEvents = append(mv.RecentLeftRightEvents, e)
		}
	}

	tryRelease := func(button uint64, fraction float32) {
		if oldButtons&button != 0 {
			recordEvent(button, false, true, fraction)
			oldButtons &^= button
		}
	}

	tryPress := func(button uint64, fraction float32) {
		if oldButtons&button == 0 {
			recordEvent(button, true, true, fraction)
			oldButtons |= button
		}
	}

	for _, step := range decoded.SubtickMoves {
		fraction := step.When
		if !step.HasWhen {
			fraction = 0.0
		}

		if step.HasButton {
			button := step.Button
			if button == IN_FORWARD || button == IN_BACK || button == IN_MOVELEFT || button == IN_MOVERIGHT {
				recordEvent(button, step.Pressed, false, fraction)
				if step.Pressed {
					oldButtons |= button
				} else {
					oldButtons &^= button
				}
			}
			continue
		}

		if step.HasAnalogForwardDelta {
			delta := step.AnalogForwardDelta
			switch delta {
			case 1.0:
				if oldButtons&IN_BACK != 0 {
					tryRelease(IN_BACK, fraction)
				} else {
					tryPress(IN_FORWARD, fraction)
				}
			case -1.0:
				if oldButtons&IN_FORWARD != 0 {
					tryRelease(IN_FORWARD, fraction)
				} else {
					tryPress(IN_BACK, fraction)
				}
			case 2.0:
				tryRelease(IN_BACK, fraction)
				tryPress(IN_FORWARD, fraction)
			case -2.0:
				tryRelease(IN_FORWARD, fraction)
				tryPress(IN_BACK, fraction)
			}
		}

		if step.HasAnalogLeftDelta {
			delta := step.AnalogLeftDelta
			switch delta {
			case 1.0:
				if oldButtons&IN_MOVERIGHT != 0 {
					tryRelease(IN_MOVERIGHT, fraction)
				} else {
					tryPress(IN_MOVELEFT, fraction)
				}
			case -1.0:
				if oldButtons&IN_MOVELEFT != 0 {
					tryRelease(IN_MOVELEFT, fraction)
				} else {
					tryPress(IN_MOVERIGHT, fraction)
				}
			case 2.0:
				tryRelease(IN_MOVERIGHT, fraction)
				tryPress(IN_MOVELEFT, fraction)
			case -2.0:
				tryRelease(IN_MOVELEFT, fraction)
				tryPress(IN_MOVERIGHT, fraction)
			}
		}
	}

	mv.SubtickButtons = oldButtons
	if mv.DebugKeepAllSubtickEvents {
		return
	}
	mv.RecentForwardBackwardEvents = cleanupAxisEvents(mv.RecentForwardBackwardEvents)
	mv.RecentLeftRightEvents = cleanupAxisEvents(mv.RecentLeftRightEvents)
}

func cleanupAxisEvents(events []InputEvent) []InputEvent {
	if len(events) < 2 {
		return events
	}
	out := make([]InputEvent, 0, len(events))
	for _, e := range events {
		n := len(out)
		if n > 0 {
			prev := out[n-1]
			if prev.CmdNum == e.CmdNum && prev.Fraction == e.Fraction && prev.Button == e.Button && prev.Pressed != e.Pressed {
				out = out[:n-1]
				continue
			}
		}
		out = append(out, e)
	}
	return out
}

func decodeBaseUserCmd(payload []byte) (decodedBaseUserCmd, error) {
	base, ok, err := decodeCSGOUserCmdBase(payload)
	if err != nil {
		return decodedBaseUserCmd{}, err
	}
	if ok {
		return base, nil
	}

	// Some demos appear to store a base cmd blob directly.
	direct, err := decodeRawBaseUserCmd(payload)
	if err != nil {
		return decodedBaseUserCmd{}, err
	}
	return direct, nil
}

func decodeCSGOUserCmdBase(payload []byte) (decodedBaseUserCmd, bool, error) {
	var baseBlob []byte
	cursor := payload
	for len(cursor) > 0 {
		num, typ, n := protowire.ConsumeTag(cursor)
		if n < 0 {
			return decodedBaseUserCmd{}, false, fmt.Errorf("consume tag: %v", protowire.ParseError(n))
		}
		cursor = cursor[n:]

		switch typ {
		case protowire.BytesType:
			b, m := protowire.ConsumeBytes(cursor)
			if m < 0 {
				return decodedBaseUserCmd{}, false, fmt.Errorf("consume bytes: %v", protowire.ParseError(m))
			}
			if num == 1 {
				baseBlob = append([]byte(nil), b...)
			}
			cursor = cursor[m:]
		default:
			m := consumeScalar(cursor, typ)
			if m < 0 {
				return decodedBaseUserCmd{}, false, fmt.Errorf("consume scalar: %v", protowire.ParseError(m))
			}
			cursor = cursor[m:]
		}
	}

	if len(baseBlob) == 0 {
		return decodedBaseUserCmd{}, false, nil
	}
	base, err := decodeRawBaseUserCmd(baseBlob)
	if err != nil {
		return decodedBaseUserCmd{}, false, err
	}
	return base, true, nil
}

func decodeRawBaseUserCmd(payload []byte) (decodedBaseUserCmd, error) {
	out := decodedBaseUserCmd{}
	cursor := payload
	for len(cursor) > 0 {
		num, typ, n := protowire.ConsumeTag(cursor)
		if n < 0 {
			return decodedBaseUserCmd{}, fmt.Errorf("consume tag: %v", protowire.ParseError(n))
		}
		cursor = cursor[n:]

		switch {
		case num == 18 && typ == protowire.BytesType:
			b, m := protowire.ConsumeBytes(cursor)
			if m < 0 {
				return decodedBaseUserCmd{}, fmt.Errorf("consume subtick bytes: %v", protowire.ParseError(m))
			}
			step, err := decodeSubtickMoveStep(b)
			if err != nil {
				return decodedBaseUserCmd{}, err
			}
			out.SubtickMoves = append(out.SubtickMoves, step)
			cursor = cursor[m:]
		default:
			m := consumeValue(cursor, typ)
			if m < 0 {
				return decodedBaseUserCmd{}, fmt.Errorf("consume value: %v", protowire.ParseError(m))
			}
			cursor = cursor[m:]
		}
	}
	return out, nil
}

func decodeSubtickMoveStep(payload []byte) (subtickMoveStep, error) {
	out := subtickMoveStep{}
	cursor := payload
	for len(cursor) > 0 {
		num, typ, n := protowire.ConsumeTag(cursor)
		if n < 0 {
			return subtickMoveStep{}, fmt.Errorf("consume subtick tag: %v", protowire.ParseError(n))
		}
		cursor = cursor[n:]

		switch {
		case num == 1 && typ == protowire.VarintType:
			v, m := protowire.ConsumeVarint(cursor)
			if m < 0 {
				return subtickMoveStep{}, fmt.Errorf("consume button: %v", protowire.ParseError(m))
			}
			out.Button = v
			out.HasButton = true
			cursor = cursor[m:]
		case num == 2 && typ == protowire.VarintType:
			v, m := protowire.ConsumeVarint(cursor)
			if m < 0 {
				return subtickMoveStep{}, fmt.Errorf("consume pressed: %v", protowire.ParseError(m))
			}
			out.Pressed = v != 0
			out.HasPressed = true
			cursor = cursor[m:]
		case num == 3 && typ == protowire.Fixed32Type:
			v, m := protowire.ConsumeFixed32(cursor)
			if m < 0 {
				return subtickMoveStep{}, fmt.Errorf("consume when: %v", protowire.ParseError(m))
			}
			out.When = math.Float32frombits(v)
			out.HasWhen = true
			cursor = cursor[m:]
		case num == 4 && typ == protowire.Fixed32Type:
			v, m := protowire.ConsumeFixed32(cursor)
			if m < 0 {
				return subtickMoveStep{}, fmt.Errorf("consume analog_forward_delta: %v", protowire.ParseError(m))
			}
			out.AnalogForwardDelta = math.Float32frombits(v)
			out.HasAnalogForwardDelta = true
			cursor = cursor[m:]
		case num == 5 && typ == protowire.Fixed32Type:
			v, m := protowire.ConsumeFixed32(cursor)
			if m < 0 {
				return subtickMoveStep{}, fmt.Errorf("consume analog_left_delta: %v", protowire.ParseError(m))
			}
			out.AnalogLeftDelta = math.Float32frombits(v)
			out.HasAnalogLeftDelta = true
			cursor = cursor[m:]
		default:
			m := consumeValue(cursor, typ)
			if m < 0 {
				return subtickMoveStep{}, fmt.Errorf("consume subtick value: %v", protowire.ParseError(m))
			}
			cursor = cursor[m:]
		}
	}
	return out, nil
}

func consumeScalar(data []byte, typ protowire.Type) int {
	switch typ {
	case protowire.VarintType:
		_, n := protowire.ConsumeVarint(data)
		return n
	case protowire.Fixed32Type:
		_, n := protowire.ConsumeFixed32(data)
		return n
	case protowire.Fixed64Type:
		_, n := protowire.ConsumeFixed64(data)
		return n
	default:
		return -1
	}
}

func consumeValue(data []byte, typ protowire.Type) int {
	switch typ {
	case protowire.BytesType:
		_, n := protowire.ConsumeBytes(data)
		return n
	default:
		return consumeScalar(data, typ)
	}
}

func AnalyzeNullsForAxis(events []InputEvent, button1 uint64, button2 uint64) NullAxisMetrics {
	metrics, _ := AnalyzeNullsForAxisWithDetails(events, button1, button2)
	return metrics
}

func AnalyzeNullsForAxisWithDetails(events []InputEvent, button1 uint64, button2 uint64) (NullAxisMetrics, []float64) {
	metrics := NullAxisMetrics{Status: "ok", Samples: len(events)}
	if len(events) < numMinInputEventsForDetection {
		metrics.Status = "not_enough_input_events"
		return metrics, nil
	}

	requiredPerfect := float64(numConsecutivePerfectCStrafeForDetectionMin)
	numOverlaps := 0.0
	numPerfect := 0.0
	numConsecutivePerfect := 0.0
	underlapDurations := make([]float64, 0)

	var lastButton1Release *InputEvent
	var lastButton2Release *InputEvent
	button1Pressed := false
	button2Pressed := false

	for i := 0; i < len(events); i++ {
		event := events[i]

		if !event.Pressed {
			if event.Button == button1 {
				lastButton1Release = &events[i]
				button1Pressed = false
			} else if event.Button == button2 {
				lastButton2Release = &events[i]
				button2Pressed = false
			}
			continue
		}

		weight := 1.0
		if event.Analog {
			weight = analogCStrafeWeight
		}

		isOverlap := (event.Button == button1 && button2Pressed) || (event.Button == button2 && button1Pressed)
		if isOverlap {
			nulled := false
			for j := i + 1; j < len(events); j++ {
				next := events[j]
				if next.CmdNum != event.CmdNum || next.Fraction != event.Fraction {
					break
				}
				if !next.Pressed {
					if (event.Button == button1 && next.Button == button2) || (event.Button == button2 && next.Button == button1) {
						nulled = true
						break
					}
				}
			}

			if nulled {
				numPerfect += weight
				numConsecutivePerfect += weight
			} else {
				numOverlaps += weight
				numConsecutivePerfect = 0
			}
		}

		if event.Button == button1 {
			button1Pressed = true
		} else if event.Button == button2 {
			button2Pressed = true
		}

		if isOverlap {
			continue
		}

		var oppositeRelease *InputEvent
		if event.Button == button1 && lastButton2Release != nil {
			oppositeRelease = lastButton2Release
		} else if event.Button == button2 && lastButton1Release != nil {
			oppositeRelease = lastButton1Release
		}

		if oppositeRelease == nil {
			continue
		}

		timeDiff := (float64(event.CmdNum-oppositeRelease.CmdNum) + float64(event.Fraction-oppositeRelease.Fraction)) * float64(engineFixedTickInterval)
		if timeDiff > underlapCountThreshold {
			continue
		}

		if timeDiff == 0 {
			numPerfect += weight
			numConsecutivePerfect += weight
		} else {
			underlapDurations = append(underlapDurations, timeDiff)
		}
	}

	underlapMedian := 0.0
	if len(underlapDurations) > 0 {
		sort.Float64s(underlapDurations)
		metrics.UnderlapCount = len(underlapDurations)
		metrics.UnderlapMinMs = underlapDurations[0] * 1000.0
		metrics.UnderlapMaxMs = underlapDurations[len(underlapDurations)-1] * 1000.0
		sumUnderlap := 0.0
		for _, d := range underlapDurations {
			sumUnderlap += d
		}
		metrics.UnderlapMeanMs = (sumUnderlap / float64(len(underlapDurations))) * 1000.0
		underlapMedian = underlapDurations[len(underlapDurations)/2]
		p90Idx := int(math.Ceil(float64(len(underlapDurations))*0.9)) - 1
		if p90Idx < 0 {
			p90Idx = 0
		}
		if p90Idx >= len(underlapDurations) {
			p90Idx = len(underlapDurations) - 1
		}
		metrics.UnderlapP90Ms = underlapDurations[p90Idx] * 1000.0
	}

	total := numOverlaps + numPerfect + float64(len(underlapDurations))
	if total > 0 {
		metrics.UnderlapPct = float64(len(underlapDurations)) / total
	}
	if underlapMedian >= underlapMedianForgivenessThreshold && metrics.UnderlapPct >= underlapPercentageThreshold {
		metrics.Status = "underlap_median_too_high"
		metrics.Perfect = numPerfect
		metrics.Overlaps = numOverlaps
		metrics.ConsecutivePerfect = numConsecutivePerfect
		metrics.RequiredPerfect = requiredPerfect
		metrics.UnderlapMedianMs = underlapMedian * 1000.0
		return metrics, underlapDurations
	}

	underlapRatio := clampFloat64(underlapMedian/underlapMedianForgivenessThreshold, 0.0, 1.0)
	adjustedRequired := lerpFloat64(underlapRatio*underlapRatio, requiredPerfect, float64(numConsecutivePerfectCStrafeForDetectionMax))

	metrics.Detected = numConsecutivePerfect >= adjustedRequired
	metrics.ConsecutivePerfect = numConsecutivePerfect
	metrics.RequiredPerfect = adjustedRequired
	metrics.Perfect = numPerfect
	metrics.Overlaps = numOverlaps
	metrics.UnderlapMedianMs = underlapMedian * 1000.0
	return metrics, underlapDurations
}

func clampFloat64(v float64, min float64, max float64) float64 {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}

func lerpFloat64(t float64, a float64, b float64) float64 {
	return a + (b-a)*t
}
